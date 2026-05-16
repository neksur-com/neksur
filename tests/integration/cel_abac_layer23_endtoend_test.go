//go:build integration

// CR-A2 end-to-end coverage — Plan 02-11 Task 2.
//
// Closes iteration-2 review finding CR-A2 (carryover from CR-04) by
// exercising the FULL Phase 2 ABAC fan-out path:
//
//	CEL policy text
//	  → cel.Program (Plan 02-01 InterruptCheckFrequency + Plan 02-11
//	                 principalAttributeDecorator ProgramOptions)
//	  → principalAttributeInterpretable.Eval(activation)
//	      reads __resolver = *store.AttributeResolver
//	      reads __ctx     = tenant-scoped context.Context
//	  → store.AttributeResolver.Resolve(ctx, sub, name, oidcClaims)
//	      Layer 1: oidcClaims[name]   (from principal["claims"])
//	      Layer 2: AGE graph MATCH (p)-[:HAS_ATTRIBUTE]->(a)
//	      Layer 3: tenants.tenant_default_attributes JSONB
//	  → CEL string return value
//	  → CEL comparison expression
//	  → ActionAllow / ActionDeny
//
// The companion test (abac_three_layer_test.go) exercises Resolve in
// isolation; THIS test exercises the cel-go-to-Resolver glue that was
// broken before Plan 02-11 (the CR-A2 surface).
//
// 3 sub-tests cover the precedence + the all-miss Pitfall-8 sentinel:
//
//	(a) Layer 2 wins over Layer 3 — graph HAS_ATTRIBUTE present AND
//	    tenant default present → CEL returns the graph value.
//	(b) Layer 3 fills the gap — graph empty + tenant default present
//	    → CEL returns the tenant default.
//	(c) All-layers-miss — CEL returns "" → policy gating on the
//	    sentinel evaluates true (proves the empty-string is the
//	    observable miss signal end-to-end, NOT a 503 or evaluator error).
//
// Layer-1 priority is already covered by cel_abac_attribute_test.go
// (which seeds the OIDC claim and confirms allow). Reproducing it
// here would be redundant; the decorator's Layer-1 path through the
// Resolver is identical regardless of test seed.

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const abacFanOutTenant = "abacfa11-fa11-4abc-8abc-abacfa110011"

// TestABACFanOut_EndToEnd asserts the decorator-wired CEL binding
// reaches Layers 2 + 3 of the AttributeResolver against real Postgres
// + AGE infrastructure. Pre-Plan-02-11 this surface was broken — the
// binding was Layer-1-only and a P5/P6 policy depending on graph or
// tenant-default attributes would have silently seen "" and produced
// a wrong allow/deny decision.
func TestABACFanOut_EndToEnd(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, abacFanOutTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	tenantUUID := uuid.MustParse(abacFanOutTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	resolver := store.NewAttributeResolver(gc, fx.pool)

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	// CEL policy under test — gates on the Layer 2/3-resolved value of
	// the "region" attribute. The decorator (functions.go) is the only
	// path that can populate this value beyond Layer 1; if the
	// decorator hook were absent (the pre-CR-A2 state) the binding
	// would return "" and every sub-test below would fail.
	policy := celpkg.Policy{
		ID:   "fan-out-region-eq-us-east",
		Kind: "schema",
		Text: `principal.attribute(principal, "region") == "us-east"`,
	}

	// Helper to construct Inputs with the real resolver + a principal
	// that has NO OIDC claims (Layer 1 miss — forces the test through
	// the Layer 2/3 path that CR-A2 covers).
	buildInputs := func() *celpkg.Inputs {
		return &celpkg.Inputs{
			Principal: map[string]any{
				"sub":    "user-1",
				"claims": map[string]any{}, // Layer 1 miss
			},
			AttributeResolver: resolver,
		}
	}

	// (a) Layer 2 wins over Layer 3: graph HAS_ATTRIBUTE has
	// region=us-east AND tenant_default_attributes has region=us-west
	// → CEL returns "us-east" → policy allows.
	t.Run("layer2_wins_over_layer3", func(t *testing.T) {
		seedGraphAttribute(t, gc, abacFanOutTenant, "user-1", "region", "us-east")
		defer cleanupGraphAttribute(t, gc, abacFanOutTenant, "user-1", "region")
		setTenantDefault(t, fx, abacFanOutTenant, `{"region":"us-west"}`)
		defer setTenantDefault(t, fx, abacFanOutTenant, `{}`)

		dec, err := ev.Evaluate(ctx, policy, buildInputs())
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if dec.Action != celpkg.ActionAllow {
			t.Errorf("Layer-2-wins-over-3: want ActionAllow; got %+v", dec)
		}
	})

	// (b) Layer 3 fills the gap: no graph attribute, tenant default
	// has region=us-east → CEL returns "us-east" → policy allows.
	// Pre-CR-A2: this was the load-bearing failure mode — the binding
	// returned "" because it never reached the tenant-default layer.
	t.Run("layer3_when_layer2_empty", func(t *testing.T) {
		setTenantDefault(t, fx, abacFanOutTenant, `{"region":"us-east"}`)
		defer setTenantDefault(t, fx, abacFanOutTenant, `{}`)

		dec, err := ev.Evaluate(ctx, policy, buildInputs())
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if dec.Action != celpkg.ActionAllow {
			t.Errorf("Layer-3-when-2-empty: want ActionAllow; got %+v", dec)
		}
	})

	// (c) All-layers-miss: no OIDC claim, no graph attribute, no
	// tenant default → CEL returns "" (Pitfall 8 sentinel) → comparison
	// `"" == "us-east"` is false → ActionDeny. The point of this case
	// is that the gateway sees a DENY, not a 503 — the empty-string
	// sentinel keeps "missing attribute" distinguishable from "policy
	// engine broken".
	t.Run("all_layers_miss_returns_empty_sentinel", func(t *testing.T) {
		// Ensure clean state: no graph attr, empty tenant default.
		setTenantDefault(t, fx, abacFanOutTenant, `{}`)
		defer setTenantDefault(t, fx, abacFanOutTenant, `{}`)

		dec, err := ev.Evaluate(ctx, policy, buildInputs())
		if err != nil {
			t.Fatalf("Evaluate (all-miss): %v", err)
		}
		if dec.Action != celpkg.ActionDeny {
			t.Errorf("All-layers-miss: want ActionDeny (binding returns \"\"); got %+v", dec)
		}

		// And explicitly verify the binding returns "" by gating on
		// it: policy `principal.attribute(principal, "region") == ""`
		// must evaluate to ActionAllow when every layer is empty.
		sentinelPolicy := celpkg.Policy{
			ID:   "fan-out-region-eq-empty",
			Kind: "schema",
			Text: `principal.attribute(principal, "region") == ""`,
		}
		dec, err = ev.Evaluate(ctx, sentinelPolicy, buildInputs())
		if err != nil {
			t.Fatalf("Evaluate (sentinel check): %v", err)
		}
		if dec.Action != celpkg.ActionAllow {
			t.Errorf("Sentinel-check: want ActionAllow (binding returns \"\"); got %+v", dec)
		}
	})
}
