// Package store implements the AGE-backed Policy Node loader.
//
// LoadPoliciesForTable returns the merged set of policies that govern a
// single Iceberg table, sourced from two graph-shape conventions:
//
//   - Generic Policy nodes (P1/P2): `Policy-[:SCHEMA_GOVERNS]->Table`
//     (D-1.08 schema policy) + `Policy-[:WRITE_GOVERNS]->Table`
//     (D-1.08 write-ACL policy). The Policy.kind property
//     discriminates ("schema" / "write_acl").
//
//   - RetentionPolicy nodes (P3): `RetentionPolicy-[:RETAINS]->Table`
//     per ADR-010 override (CONTEXT line 86). NOT a generic Policy
//     with kind="retention" — Phase 2 policy-driven scheduling
//     (ADR-010) extends this shape, so Phase 1 must adopt it too or
//     migrate later.
//
// Both queries run inside `gc.ExecuteInTenant` so:
//   1. Layer 3 RLS scopes the MATCH to the calling tenant
//      (cross-tenant policy leak T-1-cross-tenant-policy-leak
//      is mitigated by the V0030/V0032 RLS predicate
//      `(properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)`).
//   2. The pgxpool BeforeAcquire DISCARD ALL hook (Plan 01-04 deviation #2/#3)
//      keeps tenant context clean across requests.
//   3. D-001.14 telemetry collectors (cypher_duration_ms,
//      cypher_errors_total) emit on every query.
//
// CC2 (PATTERNS.md line 21): every AGE access goes through
// `ExecuteInTenant` — never via raw `pool.Query` — to prevent the
// silent 0-row return that would result from the RLS predicate failing
// when `app.current_tenant` is unset.
//
// CC3: reuse the existing graph.GraphClient pool — DO NOT introduce a
// second pgxpool here (the BeforeAcquire DISCARD ALL hook is the ONLY
// guarantee against session bleed).

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

// ErrTenantMissing is the sentinel returned when LoadPoliciesForTable
// is called without a tenant ID in the context. The L1 gateway MUST
// mount behind workosauth.TenantMiddleware so this should never fire
// in production; the sentinel exists so test paths and audit-log
// scrapers can catch the omission.
var ErrTenantMissing = errors.New("policy/store: tenant context missing")

// AGEStore is the Phase 1 policy loader. Wraps a graph.GraphClient and
// exposes a single method (LoadPoliciesForTable). Construct ONCE per
// process; share across all evaluators.
type AGEStore struct {
	gc *graph.GraphClient
}

// NewAGEStore constructs an AGEStore against the given graph client.
// The graph client owns the only pool — DO NOT introduce a second pool
// here (Phase 0.5 must_have: pgxpool BeforeAcquire DISCARD ALL is the
// ONLY enforcement of session-bleed prevention).
func NewAGEStore(gc *graph.GraphClient) *AGEStore {
	return &AGEStore{gc: gc}
}

// LoadPoliciesForTable returns all policies (P1 schema + P2 write-ACL +
// P3 retention) governing the given Iceberg table for the calling
// tenant. Order is unspecified — callers iterate all and deny on first
// Deny per the gateway aggregation rule.
//
// Returns:
//   - ([]cel.Policy{...}, nil) on success — possibly empty if no
//     policies attach to the table (NOT an error).
//   - (nil, ErrTenantMissing) when the tenant context was not set.
//   - (nil, wrapped pgx error) on transport / RLS / Cypher failures.
//
// CONTEXT line 86 ADR-010 override: P3 retention uses RetentionPolicy +
// RETAINS, NOT generic Policy + RETENTION_GOVERNS. Phase 2 extends the
// retention scheduler (ADR-010); maintaining the ADR-010 shape today
// avoids a migration later.
func (s *AGEStore) LoadPoliciesForTable(ctx context.Context, ref iceberg.TableRef) ([]cel.Policy, error) {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("policy/store: load policies: %w", ErrTenantMissing)
	}

	ns := joinNamespace(ref.Namespace)
	var policies []cel.Policy

	err := s.gc.ExecuteInTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		// P1 + P2 — generic Policy nodes.
		// AGE 1.6 quirks (Plan 01-04 lessons applied):
		//   - parameter binding into the Cypher body is NOT directly
		//     supported — we splice the values via fmt.Sprintf with
		//     escapeCypher to defend against quote injection.
		//   - the disjunction edge label syntax `:A|B` IS supported by
		//     AGE 1.6 (verified against the test fixture).
		//   - the result rows return AGE agtype scalars; we strip
		//     surrounding quotes via stripAgtypeQuotes.
		policyCypher := fmt.Sprintf(
			`MATCH (p:Policy)-[:SCHEMA_GOVERNS|WRITE_GOVERNS]->(t:Table {name: '%s', namespace: '%s'}) RETURN p.id, p.kind, p.definition_cel`,
			escapeCypher(ref.Name),
			escapeCypher(ns),
		)
		policyQuery := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (id ag_catalog.agtype, kind ag_catalog.agtype, text ag_catalog.agtype)",
			policyCypher,
		)
		rows, err := tx.Query(ctx, policyQuery)
		if err != nil {
			return fmt.Errorf("policy/store: P1/P2 query: %w", err)
		}
		for rows.Next() {
			var id, kind, text string
			if err := rows.Scan(&id, &kind, &text); err != nil {
				rows.Close()
				return fmt.Errorf("policy/store: P1/P2 scan: %w", err)
			}
			policies = append(policies, cel.Policy{
				ID:   stripAgtypeQuotes(id),
				Kind: stripAgtypeQuotes(kind),
				Text: stripAgtypeQuotes(text),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("policy/store: P1/P2 rows err: %w", err)
		}
		rows.Close()

		// P3 — RetentionPolicy nodes per ADR-010 (CONTEXT line 86 override).
		retentionCypher := fmt.Sprintf(
			`MATCH (rp:RetentionPolicy)-[:RETAINS]->(t:Table {name: '%s', namespace: '%s'}) RETURN rp.id, rp.definition_cel`,
			escapeCypher(ref.Name),
			escapeCypher(ns),
		)
		retentionQuery := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (id ag_catalog.agtype, text ag_catalog.agtype)",
			retentionCypher,
		)
		rows2, err := tx.Query(ctx, retentionQuery)
		if err != nil {
			return fmt.Errorf("policy/store: P3 query: %w", err)
		}
		for rows2.Next() {
			var id, text string
			if err := rows2.Scan(&id, &text); err != nil {
				rows2.Close()
				return fmt.Errorf("policy/store: P3 scan: %w", err)
			}
			policies = append(policies, cel.Policy{
				ID:   stripAgtypeQuotes(id),
				Kind: "retention",
				Text: stripAgtypeQuotes(text),
			})
		}
		if err := rows2.Err(); err != nil {
			rows2.Close()
			return fmt.Errorf("policy/store: P3 rows err: %w", err)
		}
		rows2.Close()

		return nil
	})
	if err != nil {
		return nil, err
	}
	return policies, nil
}

// joinNamespace flattens a multi-segment namespace path into a
// dot-delimited string. Phase 1 single-level namespaces flatten cleanly
// (e.g., ["sales"] → "sales"); Phase 3 nested namespaces may need a
// different separator if conflicts emerge.
//
// Example: joinNamespace([]string{"prod", "sales"}) == "prod.sales".
func joinNamespace(parts []string) string {
	return strings.Join(parts, ".")
}

// escapeCypher single-quote-escapes a string for safe inlining into a
// Cypher MATCH/MERGE body — same shape as internal/ingest's
// escapeCypher, duplicated here to avoid a cross-package dependency
// (the policy/store package should not import internal/ingest).
func escapeCypher(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.ReplaceAll(s, "'", "\\'")
}

// stripAgtypeQuotes removes the JSON-style surrounding quotes and any
// AGE type suffix from a scalar agtype string result. Mirrors the
// helper in internal/ingest/cycle.go (kept package-local to avoid the
// internal/ingest dependency from policy/store).
func stripAgtypeQuotes(s string) string {
	for _, suffix := range []string{"::text", "::numeric"} {
		if strings.HasSuffix(s, suffix) {
			s = s[:len(s)-len(suffix)]
		}
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
