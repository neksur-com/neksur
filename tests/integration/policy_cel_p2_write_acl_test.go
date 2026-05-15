//go:build integration

// Plan 01-05 Task 3 [BLOCKING] — P2 write-ACL policy end-to-end.
//
// Two tests:
//   - TestPolicyCEL_P2_RejectsDisallowedPrincipal — policy
//     "principal.sub in ['alice','bob']" denies for sub=mallory.
//   - TestPolicyCEL_P2_AllowsAllowedPrincipal     — same policy allows
//     for sub=alice.
//
// Exercises Policy + WRITE_GOVERNS edge load via store.AGEStore.

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

const p2AclTenant = "22222222-2222-4222-8222-222222222222"

func TestPolicyCEL_P2_RejectsDisallowedPrincipal(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, p2AclTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `principal.sub in ["alice", "bob"]`
	seedAclPolicy(t, gc, p2AclTenant, "p2-allowlist", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(p2AclTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	policies := loadPolicies(t, ctx, gc, "orders", "test")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	in := &celpkg.Inputs{
		Principal: map[string]any{
			"sub":   "mallory",
			"roles": []any{"reader"},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny for sub=mallory; got %+v", dec)
	}
}

func TestPolicyCEL_P2_AllowsAllowedPrincipal(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantID = "22222222-2222-4222-8222-222222222223"
	_ = fx.ProvisionTenant(t, tenantID)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `principal.sub in ["alice", "bob"]`
	seedAclPolicy(t, gc, tenantID, "p2-allowlist-2", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(tenantID)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	policies := loadPolicies(t, ctx, gc, "orders", "test")

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	in := &celpkg.Inputs{
		Principal: map[string]any{
			"sub":   "alice",
			"roles": []any{"writer"},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow for sub=alice; got %+v", dec)
	}
}
