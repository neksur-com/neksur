// Engine registry reader — D-2.04 / Plan 02-04.
//
// `public.engines` (V0070) is the per-tenant registry of query engines
// connected to Neksur. The cross-engine policy compiler reads this
// table to discover which (kind, version) pairs need a CompiledPolicy
// artifact for each Policy node.
//
// V0070 enforces RLS via `tenant_id::text = current_setting(
// 'app.current_tenant', true)` so this reader runs INSIDE
// `ExecuteInTenant` to inherit the tenant scoping (same pattern as
// LoadPoliciesForTable in age.go). The DISCARD ALL release hook
// guarantees no session bleed across tenants.
//
// The reader is read-only — engine registration / deletion happens
// through the admin onboarding scripts (admin_role bypasses RLS via
// BYPASSRLS).

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
)

// Engine is the in-memory projection of a `public.engines` row.
// EndpointURL is included so the probe runner can construct the
// engine-side client connection; the compiler itself only needs
// (Kind, Version) for dispatch.
type Engine struct {
	ID          string
	Kind        string
	Version     string
	EndpointURL string
}

// EngineRegistry reads `public.engines` rows for the calling tenant.
// Construct ONCE per process via NewEngineRegistry. Thread-safe.
type EngineRegistry struct {
	gc *graph.GraphClient
}

// NewEngineRegistry wraps the given graph client. The graph client's
// pool is reused; do NOT introduce a second pgxpool here (Phase 0.5
// invariant — DISCARD ALL release hook lives on that pool).
func NewEngineRegistry(gc *graph.GraphClient) *EngineRegistry {
	return &EngineRegistry{gc: gc}
}

// List returns every engine the calling tenant has registered.
// Empty result is NOT an error — a fresh tenant with no engines
// connected returns ([], nil) and the cross-engine compiler simply
// emits no CompiledPolicy nodes.
//
// The order matches `created_at` ASC — earliest registrations first
// — so the compiler's "first dialect" tie-breaker is deterministic.
func (r *EngineRegistry) List(ctx context.Context) ([]Engine, error) {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("policy/store: engine list: %w", ErrTenantMissing)
	}

	var engines []Engine
	err := r.gc.ExecuteInTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		const q = `
			SELECT id::text, kind, version, endpoint_url
			FROM public.engines
			ORDER BY created_at ASC
		`
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("policy/store: engines query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Engine
			if err := rows.Scan(&e.ID, &e.Kind, &e.Version, &e.EndpointURL); err != nil {
				return fmt.Errorf("policy/store: engines scan: %w", err)
			}
			engines = append(engines, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return engines, nil
}

// ListByKind filters the registry to engines of a single kind (e.g.,
// "trino"). Returned in the same created_at ASC order as List.
//
// `kind` is sanitized via graph.MustSanitizeCypherLiteral as a
// defence-in-depth — the SQL parameter binding already prevents SQL
// injection but rejecting weird inputs at the API boundary surfaces
// caller bugs earlier.
func (r *EngineRegistry) ListByKind(ctx context.Context, kind string) ([]Engine, error) {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("policy/store: engine list by kind: %w", ErrTenantMissing)
	}
	_ = graph.MustSanitizeCypherLiteral(kind) // defence-in-depth identifier check

	var engines []Engine
	err := r.gc.ExecuteInTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		const q = `
			SELECT id::text, kind, version, endpoint_url
			FROM public.engines
			WHERE kind = $1
			ORDER BY created_at ASC
		`
		rows, err := tx.Query(ctx, q, kind)
		if err != nil {
			return fmt.Errorf("policy/store: engines (by kind) query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Engine
			if err := rows.Scan(&e.ID, &e.Kind, &e.Version, &e.EndpointURL); err != nil {
				return fmt.Errorf("policy/store: engines (by kind) scan: %w", err)
			}
			engines = append(engines, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return engines, nil
}
