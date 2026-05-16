//go:build integration

// Plan 02-03 Task 2B [BLOCKING] — D-2.10 3-layer ABAC fetch end-to-end.
//
// Directly exercises (*store.AttributeResolver).Resolve, which walks
// the 3 layers in strict priority order:
//
//   1. OIDC claims (oidcClaims arg, projected by the gateway).
//   2. Principal-scoped graph attribute
//      (:Principal {sub})-[:HAS_ATTRIBUTE]->(:Attribute {name, value}).
//   3. Tenant-scoped default in public.tenants.tenant_default_attributes
//      (JSONB string→string map).
//
// 4 sub-tests cover the precedence + the all-empty Pitfall-8 sentinel
// ("" never nil / error). Plan 02-04 will land the gateway-side
// activation wiring that exposes Layers 2+3 through the CEL binding
// (functions.go::principalAttributeImpl); this test exercises the
// resolver directly so the precedence contract is locked in
// independently of the binding plumbing.

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const abac3LTenant = "abac3333-3333-4abc-8abc-abc333333333"

// TestABACThreeLayerFetch covers the precedence contract:
//
//	(a) Layer 1 wins over Layer 2 — OIDC claim trumps graph attribute.
//	(b) Layer 2 wins over Layer 3 — graph attribute trumps tenant default.
//	(c) Layer 3 falls through    — empty claims + no graph attr → default.
//	(d) All-empty                — returns "" (Pitfall 8 sentinel).
func TestABACThreeLayerFetch(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, abac3LTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	tenantUUID := uuid.MustParse(abac3LTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	resolver := store.NewAttributeResolver(gc, fx.pool)

	// (a) Layer 1 wins: OIDC has clearance=oidc-val AND graph also has
	// clearance=graph-val → expect "oidc-val".
	t.Run("layer1_oidc_wins_over_graph", func(t *testing.T) {
		seedGraphAttribute(t, gc, abac3LTenant, "user-1", "clearance", "graph-val")
		defer cleanupGraphAttribute(t, gc, abac3LTenant, "user-1", "clearance")

		got := resolver.Resolve(ctx, "user-1", "clearance",
			map[string]string{"clearance": "oidc-val"})
		if got != "oidc-val" {
			t.Errorf("layer1 precedence: want %q got %q", "oidc-val", got)
		}
	})

	// (b) Layer 2 wins over Layer 3: empty OIDC + graph has clearance=
	// graph-val + tenant_default has clearance=default-val → expect
	// "graph-val".
	t.Run("layer2_graph_wins_over_default", func(t *testing.T) {
		seedGraphAttribute(t, gc, abac3LTenant, "user-1", "clearance", "graph-val")
		defer cleanupGraphAttribute(t, gc, abac3LTenant, "user-1", "clearance")
		setTenantDefault(t, fx, abac3LTenant, `{"clearance":"default-val"}`)
		defer setTenantDefault(t, fx, abac3LTenant, `{}`)

		got := resolver.Resolve(ctx, "user-1", "clearance", map[string]string{})
		if got != "graph-val" {
			t.Errorf("layer2 precedence: want %q got %q", "graph-val", got)
		}
	})

	// (c) Layer 3 fall-through: empty OIDC + no graph attr +
	// tenant_default has clearance=default-val → expect "default-val".
	t.Run("layer3_tenant_default", func(t *testing.T) {
		setTenantDefault(t, fx, abac3LTenant, `{"clearance":"default-val"}`)
		defer setTenantDefault(t, fx, abac3LTenant, `{}`)

		got := resolver.Resolve(ctx, "user-1", "clearance", map[string]string{})
		if got != "default-val" {
			t.Errorf("layer3 fall-through: want %q got %q", "default-val", got)
		}
	})

	// (d) All empty → "" (Pitfall 8 null-safe sentinel, NEVER nil/error).
	t.Run("all_empty_returns_pitfall8_sentinel", func(t *testing.T) {
		got := resolver.Resolve(ctx, "user-1", "clearance", map[string]string{})
		if got != "" {
			t.Errorf("all-empty: want %q got %q", "", got)
		}
	})
}

// ---- helpers ------------------------------------------------------------

// seedGraphAttribute creates (:Principal {sub})-[:HAS_ATTRIBUTE]->
// (:Attribute {name, value, tenant_id}) inside the tenant graph. All
// literals are routed through graph.MustSanitizeCypherLiteral
// (Phase 1 CR-01 — AGE 1.6 cannot bind params into the Cypher body).
// MERGE keeps the seed idempotent across sub-test re-runs.
func seedGraphAttribute(t *testing.T, gc *graph.GraphClient, tenantID, sub, name, value string) {
	t.Helper()
	safeSub := graph.MustSanitizeCypherLiteral(sub)
	safeName := graph.MustSanitizeCypherLiteral(name)
	safeValue := graph.MustSanitizeCypherLiteral(value)
	safeTenant := graph.MustSanitizeCypherLiteral(tenantID)

	cypher := fmt.Sprintf(
		`MERGE (p:Principal {sub: '%s', tenant_id: '%s'}) `+
			`MERGE (a:Attribute {name: '%s', tenant_id: '%s'}) `+
			`SET a.value = '%s' `+
			`MERGE (p)-[r:HAS_ATTRIBUTE {tenant_id: '%s'}]->(a)`,
		safeSub, safeTenant, safeName, safeTenant, safeValue, safeTenant,
	)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
		cypher,
	)
	if err := gc.ExecuteInTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query)
		return err
	}); err != nil {
		t.Fatalf("seedGraphAttribute: %v", err)
	}
}

// cleanupGraphAttribute removes the Attribute + HAS_ATTRIBUTE edge for
// the given (sub, name). DETACH DELETE drops the relationship along
// with the node so subsequent sub-tests start from a known state.
func cleanupGraphAttribute(t *testing.T, gc *graph.GraphClient, tenantID, sub, name string) {
	t.Helper()
	safeSub := graph.MustSanitizeCypherLiteral(sub)
	safeName := graph.MustSanitizeCypherLiteral(name)

	cypher := fmt.Sprintf(
		`MATCH (p:Principal {sub: '%s'})-[r:HAS_ATTRIBUTE]->(a:Attribute {name: '%s'}) DETACH DELETE a`,
		safeSub, safeName,
	)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
		cypher,
	)
	if err := gc.ExecuteInTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query)
		return err
	}); err != nil {
		t.Logf("cleanupGraphAttribute (best-effort): %v", err)
	}
}

// setTenantDefault writes the supplied JSON string into
// public.tenants.tenant_default_attributes for the tenant under test.
// Uses the Phase1Fixture admin pool (CC3 — no second pgxpool).
func setTenantDefault(t *testing.T, fx *Phase1Fixture, tenantID, jsonLiteral string) {
	t.Helper()
	conn, err := fx.pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("setTenantDefault: pool acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(),
		`UPDATE public.tenants SET tenant_default_attributes = $1::jsonb WHERE id = $2::uuid`,
		jsonLiteral, tenantID,
	); err != nil {
		t.Fatalf("setTenantDefault: update: %v", err)
	}
}
