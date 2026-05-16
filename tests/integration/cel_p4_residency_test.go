//go:build integration

// Plan 02-03 Task 2B [BLOCKING] — P4 residency policy end-to-end.
//
// Exercises the new RESIDENCY_GOVERNS edge label (added to
// AGEStore.LoadPoliciesForTable in Task 2A / commit 842881d) together
// with the location.region CEL binding (registered in Task 1 / commit
// 09418b2). Two sub-tests:
//
//   - allow:  commit.location_region == "us-east-1" → ActionAllow.
//   - deny:   commit.location_region == "eu-west-1" → ActionDeny.
//
// Direct Evaluator.Evaluate call — no gateway involvement; Plan 02-04
// owns the gateway-side X-Neksur-Region projection that populates
// commit.location_region in production.

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

const p4ResidencyTenant = "44444444-4444-4444-8444-444444444444"

// TestPolicyCEL_P4_Residency seeds a RESIDENCY_GOVERNS-edged Policy
// requiring us-east-1 and asserts allow + deny across two evaluations.
func TestPolicyCEL_P4_Residency(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, p4ResidencyTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `location.region(commit) == "us-east-1"`
	seedPolicyOfKind(t, gc, p4ResidencyTenant, "p4-region-us-east-1",
		policyText, "orders", "test",
		"Policy", "residency", "RESIDENCY_GOVERNS")

	tenantUUID := uuid.MustParse(p4ResidencyTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	policies := loadPolicies(t, ctx, gc, "orders", "test")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	// Allow: matching region.
	allowIn := &celpkg.Inputs{
		Commit: map[string]any{
			"location_region": "us-east-1",
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], allowIn)
	if err != nil {
		t.Fatalf("Evaluate (allow case): %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow for us-east-1; got %+v", dec)
	}

	// Deny: non-matching region.
	denyIn := &celpkg.Inputs{
		Commit: map[string]any{
			"location_region": "eu-west-1",
		},
	}
	dec, err = ev.Evaluate(ctx, policies[0], denyIn)
	if err != nil {
		t.Fatalf("Evaluate (deny case): %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny for eu-west-1; got %+v", dec)
	}
}
