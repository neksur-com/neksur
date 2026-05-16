//go:build integration

// Plan 02-03 Task 2B [BLOCKING] — ABAC principal.attribute binding end-to-end.
//
// Exercises the new ABAC_GOVERNS edge label together with the
// principal.attribute(principal, name) CEL binding. This test
// exercises **Layer 1 only** (OIDC claims projected onto
// principal["claims"]) — Layers 2 (graph HAS_ATTRIBUTE) and 3
// (tenant_default_attributes JSONB) become reachable from a CEL
// policy expression after Plan 02-04 wires the activation-side
// AttributeResolver. The Layer 1→2→3 fetch chain from outside CEL
// (direct Resolve call) is covered by abac_three_layer_test.go.
//
// Two sub-tests against a policy that says
// `principal.attribute(principal, "clearance") == "top-secret"`:
//
//   - allow: principal.claims has clearance=top-secret.
//   - deny:  principal.claims has no clearance entry.

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

const abacAttrTenant = "abacabac-abac-4abc-8abc-abacabacabac"

// TestPolicyCEL_ABAC_PrincipalAttribute asserts allow when the OIDC
// claim is present and deny when absent (Layer 1 exercised in
// isolation).
func TestPolicyCEL_ABAC_PrincipalAttribute(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, abacAttrTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `principal.attribute(principal, "clearance") == "top-secret"`
	seedPolicyOfKind(t, gc, abacAttrTenant, "abac-clearance-top-secret",
		policyText, "orders", "test",
		"Policy", "abac", "ABAC_GOVERNS")

	tenantUUID := uuid.MustParse(abacAttrTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	policies := loadPolicies(t, ctx, gc, "orders", "test")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	// Allow: Layer-1 OIDC claim matches the required value.
	allowIn := &celpkg.Inputs{
		Principal: map[string]any{
			"sub": "alice",
			"claims": map[string]any{
				"clearance": "top-secret",
			},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], allowIn)
	if err != nil {
		t.Fatalf("Evaluate (allow case): %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow when clearance=top-secret; got %+v", dec)
	}

	// Deny: claim absent → binding returns "" (Pitfall 8 null-safe
	// sentinel) → policy comparison fails → ActionDeny.
	denyIn := &celpkg.Inputs{
		Principal: map[string]any{
			"sub":    "alice",
			"claims": map[string]any{},
		},
	}
	dec, err = ev.Evaluate(ctx, policies[0], denyIn)
	if err != nil {
		t.Fatalf("Evaluate (deny case): %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny when clearance claim absent; got %+v", dec)
	}
}
