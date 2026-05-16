//go:build integration

// Plan 02-03 Task 2B [BLOCKING] — P5 classification policy end-to-end.
//
// Exercises the new CLASSIFICATION_GOVERNS edge label together with
// the manifest.classification_satisfied(table, pattern, requiredTag)
// CEL binding. Two sub-tests against a single seeded policy that says
// "every column matching ^.*_ssn$ MUST be tagged ENCRYPTED":
//
//   - allow:  table has column customer_ssn AND a classification
//             entry {column_name: customer_ssn, tag: ENCRYPTED}.
//   - deny:   same table but classifications list is empty — the
//             policy contract requires the entry to be present.

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

const p5ClassificationTenant = "55555555-5555-4555-8555-555555555555"

// TestPolicyCEL_P5_Classification asserts allow when *_ssn columns
// carry the required tag and deny when the classification entry is
// missing.
func TestPolicyCEL_P5_Classification(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, p5ClassificationTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `manifest.classification_satisfied(table, "^.*_ssn$", "ENCRYPTED")`
	seedPolicyOfKind(t, gc, p5ClassificationTenant, "p5-ssn-encrypted",
		policyText, "orders", "test",
		"Policy", "classification", "CLASSIFICATION_GOVERNS")

	tenantUUID := uuid.MustParse(p5ClassificationTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	policies := loadPolicies(t, ctx, gc, "orders", "test")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	// Allow: column present AND classification entry present.
	allowIn := &celpkg.Inputs{
		Table: map[string]any{
			"columns": []any{
				map[string]any{"name": "id", "type": "long"},
				map[string]any{"name": "customer_ssn", "type": "string"},
			},
			"classifications": []any{
				map[string]any{"column_name": "customer_ssn", "tag": "ENCRYPTED"},
			},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], allowIn)
	if err != nil {
		t.Fatalf("Evaluate (allow case): %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow when *_ssn is ENCRYPTED-tagged; got %+v", dec)
	}

	// Deny: same column, no classification entry → contract violated.
	denyIn := &celpkg.Inputs{
		Table: map[string]any{
			"columns": []any{
				map[string]any{"name": "id", "type": "long"},
				map[string]any{"name": "customer_ssn", "type": "string"},
			},
			"classifications": []any{},
		},
	}
	dec, err = ev.Evaluate(ctx, policies[0], denyIn)
	if err != nil {
		t.Fatalf("Evaluate (deny case): %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny when classification entry absent; got %+v", dec)
	}
}
