// 3-layer ABAC attribute resolver — D-2.10.
//
// D-2.10's contract: every ABAC attribute lookup walks three layers in
// strict order, returning the first non-empty value:
//
//  1. OIDC claims (carried with the request, set by the IdP).
//  2. Principal-scoped graph attributes
//     (Principal)-[:HAS_ATTRIBUTE]->(Attribute {name, value}).
//  3. Tenant-scoped defaults stored in tenants.tenant_default_attributes
//     (a JSONB string→string map, set during tenant provisioning).
//
// Pitfall 8 / null-safety: when ALL three layers are exhausted, Resolve
// returns "" (the empty string), NEVER an error or nil. Policy authors
// detect "attribute absent" by comparing against the empty string:
//
//	principal.attribute(principal, "region") == ""
//
// Returning an error or nil here would surface as a CEL evaluation
// failure (the wider Evaluate path is fail-closed → 503), making
// "missing attribute" indistinguishable from "policy engine broken".
// The empty-string sentinel keeps the two distinguishable AND keeps
// policies authorable without try/catch idioms (which CEL doesn't have).
//
// Layer 2 (graph) reuses the same ExecuteInTenant + AGE wrapper pattern
// as LoadPoliciesForTable in age.go: the Cypher body is built with
// fmt.Sprintf around assertSafeCypherLiteral-sanitised literals
// (parameter binding into the Cypher body is not supported by AGE 1.6),
// and the agtype scalar result is unwrapped via stripAgtypeQuotes (also
// in age.go — same package, no re-export needed).
//
// WR-A1: the previous implementation called graph.MustSanitizeCypherLiteral
// directly, which PANICS on bytes outside the ASCII allowlist. The
// fail-soft contract at this layer (errors → "" → next layer consulted)
// is incompatible with a panic — a single malformed input would have
// crashed the gateway process. The refactor routes both splice inputs
// through the non-panicking assertSafeCypherLiteral helper; rejection
// emits a slog.WarnContext audit event and returns "" so Layer 3 takes
// over (Pitfall 8).
//
// Layer 3 (admin pool) hits the catalog `tenants` row directly. The
// admin pool is the SAME pool used for tenant lifecycle (repo.go,
// provision.go) — caller supplies it explicitly, the resolver does not
// reach into a package-level singleton.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

// AttributeResolver implements cel.AttributeResolver. It is constructed
// once at gateway-startup (Plan 02-04 wiring) with handles on the
// graph client (for Layer 2) and the admin pool (for Layer 3). The
// zero value is unusable; use NewAttributeResolver.
type AttributeResolver struct {
	gc        *graph.GraphClient
	adminPool *pgxpool.Pool
}

// NewAttributeResolver constructs the resolver. Either gc or adminPool
// MAY be nil — the corresponding layer is then silently skipped,
// matching the "first non-empty wins" contract (a missing layer is
// equivalent to a miss in that layer).
//
// Both being nil reduces Resolve to a Layer-1 (OIDC) passthrough,
// which is useful for tests but never the production configuration.
func NewAttributeResolver(gc *graph.GraphClient, adminPool *pgxpool.Pool) *AttributeResolver {
	return &AttributeResolver{gc: gc, adminPool: adminPool}
}

// Compile-time assertion: AttributeResolver satisfies the
// cel.AttributeResolver interface. The interface lives in `cel` to
// avoid a cel→store import cycle (store already imports cel for the
// Policy struct); this assertion ensures we notice immediately if the
// interface drifts.
var _ cel.AttributeResolver = (*AttributeResolver)(nil)

// Resolve walks Layer 1 → 2 → 3, returning the first non-empty value.
// All-miss returns "" (Pitfall 8). The principalSub identifies the
// requesting principal at the graph layer; oidcClaims carries the
// gateway-projected OIDC claim map for Layer 1.
//
// The ctx carries the tenant ID (via tenant.IDFromContext) — same
// idiom as age.go's LoadPoliciesForTable. Without a tenant ID,
// Layers 2 + 3 are skipped (a deliberate "fail-soft on Layer 2/3
// only" branch: D-2.10 keeps Layer 1 reachable in pre-tenant request
// paths, e.g. the dev-mode `/policy/preview` endpoint that runs CEL
// over a synthetic principal with no tenant binding yet).
//
// Errors from the graph or admin pool are NOT returned — they are
// treated as misses on that layer and the next layer is consulted.
// This matches the D-2.10 contract: "the resolver MUST be tolerant of
// transient backend faults; the gateway already fails closed on a
// truly-broken policy engine via the Evaluate boundary". A persistent
// Layer-2 outage would not silently mask a policy denial because the
// authoritative claim source for sensitive attributes is OIDC (Layer
// 1) and the Evaluator itself fail-closes on CEL errors.
func (r *AttributeResolver) Resolve(
	ctx context.Context,
	principalSub, name string,
	oidcClaims map[string]string,
) string {
	// Layer 1 — OIDC claims (already projected by the gateway).
	if v, ok := oidcClaims[name]; ok && v != "" {
		return v
	}

	tenantID, hasTenant := tenant.IDFromContext(ctx)

	// Layer 2 — Principal HAS_ATTRIBUTE Attribute in the tenant graph.
	if r.gc != nil && hasTenant {
		if v := r.layer2Graph(ctx, tenantID.String(), principalSub, name); v != "" {
			return v
		}
	}

	// Layer 3 — tenant_default_attributes JSONB on the catalog row.
	if r.adminPool != nil && hasTenant {
		if v := r.layer3TenantDefault(ctx, tenantID, name); v != "" {
			return v
		}
	}

	return ""
}

// layer2Graph runs a single tenant-scoped Cypher query against the
// per-tenant AGE graph:
//
//	MATCH (p:Principal {sub: $sub})-[:HAS_ATTRIBUTE]->(a:Attribute {name: $name})
//	RETURN a.value LIMIT 1
//
// Both string literals are sanitised through assertSafeCypherLiteral
// (Phase 1 CR-01 mitigation; WR-A1 closure replaces the previous
// panic-on-reject MustSanitizeCypherLiteral routing) BEFORE being
// spliced into the Cypher body — AGE 1.6 does not bind parameters into
// the Cypher body, so this is the only safe way to interpolate
// user-controlled strings. The principalSub originates from the request
// principal's `sub` OIDC claim, which the gateway already validates
// against a strict regexp before any policy code runs; defence-in-depth.
//
// A LIMIT 1 caps the cost at a single row even if the graph schema
// drifts to allow multiple Attribute nodes per (Principal, name). The
// "first hit wins" semantics match D-2.10 (a principal SHOULD have
// at most one Attribute per name; the constraint will be enforced in
// a later migration).
//
// Errors are swallowed: a transient query failure returns "" and lets
// Layer 3 take over (Pitfall 8 fail-soft contract). Unsafe-input
// rejections from assertSafeCypherLiteral are logged via
// slog.WarnContext so the audit trail still records the rejected input
// without crashing the gateway process.
func (r *AttributeResolver) layer2Graph(
	ctx context.Context,
	tenantID, principalSub, name string,
) string {
	safeSub, err := assertSafeCypherLiteral(principalSub)
	if err != nil {
		slog.WarnContext(ctx, "attribute resolver: layer2Graph: unsafe principal sub",
			"err", err,
			"tenant_id", tenantID,
		)
		return ""
	}
	safeName, err := assertSafeCypherLiteral(name)
	if err != nil {
		slog.WarnContext(ctx, "attribute resolver: layer2Graph: unsafe attribute name",
			"err", err,
			"tenant_id", tenantID,
			"name", name,
		)
		return ""
	}
	cypher := fmt.Sprintf(
		`MATCH (p:Principal {sub: '%s'})-[:HAS_ATTRIBUTE]->(a:Attribute {name: '%s'}) RETURN a.value LIMIT 1`,
		safeSub, safeName,
	)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (value ag_catalog.agtype)",
		cypher,
	)

	var value string
	err = r.gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, query)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		if rows.Next() {
			var raw string
			if scanErr := rows.Scan(&raw); scanErr != nil {
				return scanErr
			}
			value = stripAgtypeQuotes(raw)
		}
		return rows.Err()
	})
	if err != nil {
		return ""
	}
	return value
}

// layer3TenantDefault reads the tenants.tenant_default_attributes
// JSONB column for the request tenant and returns the value mapped to
// `name` (or "" if absent). The column schema is map<string,string> —
// any non-string value is coerced to "" by the json.Unmarshal target
// type (map[string]string).
//
// Errors (column missing, row missing, JSON malformed) are swallowed
// to "" — same fail-soft posture as Layer 2; the broader fail-closed
// contract applies at the Evaluate boundary.
//
// Note: this query bypasses ExecuteInTenant because it reads a CATALOG
// row (the `tenants` table is admin-scoped, not tenant-scoped — there
// is no `app.current_tenant` RLS predicate on it). The admin pool's
// role has been provisioned with select-only access to tenants
// (Phase 1 V0030 migration).
func (r *AttributeResolver) layer3TenantDefault(
	ctx context.Context,
	tenantID uuid.UUID,
	name string,
) string {
	var attrsRaw []byte
	err := r.adminPool.QueryRow(ctx,
		`SELECT tenant_default_attributes FROM tenants WHERE id = $1`,
		tenantID,
	).Scan(&attrsRaw)
	if err != nil || len(attrsRaw) == 0 {
		return ""
	}
	var attrs map[string]string
	if jErr := json.Unmarshal(attrsRaw, &attrs); jErr != nil {
		return ""
	}
	if v, ok := attrs[name]; ok && v != "" {
		return v
	}
	return ""
}
