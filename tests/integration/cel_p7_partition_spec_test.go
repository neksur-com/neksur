//go:build integration

// Plan 02-03 Task 2B [BLOCKING] — P7 partition-spec policy end-to-end.
//
// Exercises the new PARTITION_GOVERNS edge label together with the
// manifest.partition_spec(table) CEL binding (returns map<string,string>
// suitable for direct CEL indexing). Two sub-tests against a policy
// that says "partition field 'ts' MUST use the 'hours' transform":
//
//   - allow:  table.partition_spec.ts == "hours"
//   - deny:   table.partition_spec.ts == "days"

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

const p7PartitionTenant = "77777777-7777-4777-8777-777777777777"

// TestPolicyCEL_P7_PartitionSpec asserts allow on the required
// transform and deny on the wrong transform.
func TestPolicyCEL_P7_PartitionSpec(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, p7PartitionTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `manifest.partition_spec(table)["ts"] == "hours"`
	seedPolicyOfKind(t, gc, p7PartitionTenant, "p7-ts-hours",
		policyText, "orders", "test",
		"Policy", "partition", "PARTITION_GOVERNS")

	tenantUUID := uuid.MustParse(p7PartitionTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	policies := loadPolicies(t, ctx, gc, "orders", "test")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	// Allow: correct transform.
	allowIn := &celpkg.Inputs{
		Table: map[string]any{
			"partition_spec": map[string]any{
				"ts": "hours",
			},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], allowIn)
	if err != nil {
		t.Fatalf("Evaluate (allow case): %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow for ts=hours; got %+v", dec)
	}

	// Deny: wrong transform.
	denyIn := &celpkg.Inputs{
		Table: map[string]any{
			"partition_spec": map[string]any{
				"ts": "days",
			},
		},
	}
	dec, err = ev.Evaluate(ctx, policies[0], denyIn)
	if err != nil {
		t.Fatalf("Evaluate (deny case): %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny for ts=days; got %+v", dec)
	}
}
