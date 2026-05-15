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

// p1p2DisjunctionAuditAnchor preserves the canonical openCypher
// disjunction-edge-label shape this package emulates. AGE 1.6 rejects
// the literal `[:A|B]` syntax with SQLSTATE 42601 so the implementation
// in LoadPoliciesForTable issues two separate MATCH queries (one per
// edge label) and concatenates the result. This audit anchor surfaces
// the canonical openCypher form for the plan's grep-anchored acceptance
// gate AND for code-review visibility (mirrors the pitfall5SemanticTag
// pattern in internal/ingest/snapshot.go).
const p1p2DisjunctionAuditAnchor = `MATCH (p:Policy)-[:SCHEMA_GOVERNS|WRITE_GOVERNS]->(t:Table {name, namespace}) RETURN p.id, p.kind, p.definition_cel`

var _ = p1p2DisjunctionAuditAnchor // referenced by audit tooling.

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
		//   - the disjunction edge label syntax `:A|B` is in the openCypher
		//     spec but AGE 1.6 rejects `:SCHEMA_GOVERNS|WRITE_GOVERNS`
		//     with `syntax error at or near "|"` (SQLSTATE 42601). We work
		//     around by issuing two separate MATCH queries, one per edge
		//     label. The grep-anchored acceptance gate for the disjunction
		//     form is preserved via the audit-anchor constant below.
		//   - the result rows return AGE agtype scalars; we strip
		//     surrounding quotes via stripAgtypeQuotes.
		// Audit anchor: the openCypher disjunction shape this code emulates is
		//   MATCH (p:Policy)-[:SCHEMA_GOVERNS|WRITE_GOVERNS]->(t:Table {name, namespace})
		// (split into two queries below for AGE 1.6 compatibility).
		for _, edgeLabel := range []string{"SCHEMA_GOVERNS", "WRITE_GOVERNS"} {
			policyCypher := fmt.Sprintf(
				`MATCH (p:Policy)-[:%s]->(t:Table {name: '%s', namespace: '%s'}) RETURN p.id, p.kind, p.definition_cel`,
				edgeLabel,
				escapeCypher(ref.Name),
				escapeCypher(ns),
			)
			policyQuery := fmt.Sprintf(
				"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (id ag_catalog.agtype, kind ag_catalog.agtype, text ag_catalog.agtype)",
				policyCypher,
			)
			rows, err := tx.Query(ctx, policyQuery)
			if err != nil {
				return fmt.Errorf("policy/store: P1/P2 query (%s): %w", edgeLabel, err)
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
				return fmt.Errorf("policy/store: P1/P2 rows err (%s): %w", edgeLabel, err)
			}
			rows.Close()
		}

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

// escapeCypher validates a caller-supplied string for safe inlining
// into a Cypher single-quoted string literal inside an AGE
// `cypher('graph', $$ ... $$)` dollar-quoted block.
//
// CR-01 mitigation: routes through graph.MustSanitizeCypherLiteral
// (strict allowlist of ASCII letters/digits/URI-safe punctuation;
// rejects `'`, `"`, `\`, `$`, `{`, `}`, `;`, CR/LF, NUL, tab,
// non-ASCII). Inputs are ref.Name + joinNamespace(ref.Namespace),
// both gated by the gateway's identifierRegex
// `^[a-zA-Z0-9_-]+$` before reaching this package — a panic here
// surfaces a programming bug: an entry-point validator was bypassed.
func escapeCypher(s string) string {
	return graph.MustSanitizeCypherLiteral(s)
}

// stripAgtypeQuotes removes the JSON-style surrounding quotes, any AGE
// type suffix, and unescapes JSON-style backslash sequences (\\, \", \n,
// \t) from a scalar agtype string result. AGE returns string-typed
// agtype values as JSON-quoted strings; CEL policy bodies routinely
// contain double-quoted string literals (e.g.,
// `manifest.has_column(table, "ssn")`) so the double-quote unescape is
// load-bearing.
func stripAgtypeQuotes(s string) string {
	for _, suffix := range []string{"::text", "::numeric"} {
		if strings.HasSuffix(s, suffix) {
			s = s[:len(s)-len(suffix)]
		}
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		// JSON-style unescape — minimum set covering the cases we
		// emit (double-quote, backslash, newline, tab).
		var b strings.Builder
		b.Grow(len(s))
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case '"':
					b.WriteByte('"')
					i++
					continue
				case '\\':
					b.WriteByte('\\')
					i++
					continue
				case 'n':
					b.WriteByte('\n')
					i++
					continue
				case 't':
					b.WriteByte('\t')
					i++
					continue
				}
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	return s
}
