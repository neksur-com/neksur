// Repo + Credentials — per-tenant catalog credentials store backed by
// the V0060 catalog_credentials table.
//
// CC2 (PATTERNS.md line 21): every per-tenant DB access goes through
// `tenant.WithTenantTx` so:
//   1. Layer 1 — search_path resolves to the tenant's schema (the row
//      lives in tenant_<uuid>.catalog_credentials, NOT public).
//   2. Layer 2 — the SET LOCAL ROLE means even a misrouted SELECT cannot
//      reach a different tenant's schema.
//   3. Layer 3 — app.current_tenant GUC is set so the V0066 RLS
//      predicate fires for the FORCE-RLS catalog_credentials table.
// CC3: reuse the existing pgxpool — DO NOT introduce a second pgxpool
// here (the BeforeAcquire DISCARD ALL hook is the ONLY guarantee
// against session bleed).
//
// The sentinel ErrCredentialsNotFound covers both the row-missing and
// row-exists-but-RLS-hides-it cases — the SELECT never observes a row
// from another tenant, so a "not found" result IS the cross-tenant
// isolation working.

package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

// Credentials is the in-memory mirror of one V0060 catalog_credentials
// row. ConfigJSON is intentionally json.RawMessage (deferred unmarshal)
// so the gateway's per-kind dispatcher (BuildAdapter) can hand the bytes
// to the matching adapter's typed Config struct without round-tripping
// through map[string]any.
//
// EncryptedSecret is omitted from the Phase 1 surface — Plan 01-09
// (admin CLI) lands the per-tenant secret unwrap path; the Phase 1
// gateway reads the bytes via a future Repo.GetCatalogSecret call but
// the structural shape is owned by Plan 01-09. ConfigJSON carries
// non-sensitive endpoint + warehouse + scope; all secret material
// (ClientSecret, BearerToken) lives in EncryptedSecret.
type Credentials struct {
	Kind       string          // "polaris" | "nessie" | "glue" | "unity" — V0060 CHECK
	Nickname   string          // UNIQUE per tenant — the gateway URL prefix
	Endpoint   string          // upstream catalog ROOT URL (e.g., https://polaris.acme.com/api/catalog)
	ConfigJSON json.RawMessage // per-kind Config struct as JSON bytes
}

// Repo wraps the pgxpool with the single read surface the L1 gateway
// needs. Construct ONCE at neksur-server startup (cmd/neksur-server/main.go)
// and share the pointer with the gateway's Deps struct — Repo is
// stateless beyond the pool reference.
//
// Mirror of internal/tenant/repo.go::Repo (PATTERNS.md line 116 "exact
// match" — same Repo idiom, same NewRepo constructor shape, same pool
// dependency).
type Repo struct {
	pool *pgxpool.Pool
}

// NewRepo constructs a Repo bound to the given *pgxpool.Pool. Callers
// MUST pass the application's existing pool (the one with
// graph.WithBeforeAcquireDiscardAll applied — Phase 0.5 must_have); a
// second pool would defeat the session-bleed prevention contract.
func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

// GetCatalogCredentials returns the row matching `nickname` for the
// tenant in ctx. RLS-scoped via tenant.WithTenantTx — a different
// tenant's row with the same nickname is invisible (returns
// ErrCredentialsNotFound).
//
// The query uses positional binding ($1) — pgx forwards the value with
// no Cypher / SQL splicing.
//
// Returns:
//   - (*Credentials, nil) on success.
//   - (nil, ErrCredentialsNotFound) wrapped — when no row matches OR
//     when RLS hides the matching row (the gateway maps this to HTTP
//     404).
//   - (nil, tenant.ErrTenantNotInContext) wrapped — when the calling
//     ctx has no tenant ID (handler wired outside TenantMiddleware —
//     gateway maps to 500).
//   - (nil, wrapped pgx error) — transport / RLS / GUC failures (500).
func (r *Repo) GetCatalogCredentials(ctx context.Context, nickname string) (*Credentials, error) {
	const op = "getcatalogcredentials"
	var creds Credentials
	err := tenant.WithTenantTx(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT catalog_kind, nickname, endpoint, config_json
			FROM catalog_credentials
			WHERE nickname = $1
		`, nickname).Scan(&creds.Kind, &creds.Nickname, &creds.Endpoint, &creds.ConfigJSON)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("catalog: %s: %w", op, ErrCredentialsNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", op, err)
	}
	return &creds, nil
}
