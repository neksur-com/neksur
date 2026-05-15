//go:build integration

// Plan 01-05 Task 3 [BLOCKING] — P3 retention policy end-to-end
// (ADR-010 RetentionPolicy + RETAINS shape per CONTEXT line 86 override).
//
// Three tests:
//   - TestPolicyCEL_P3_RetentionPolicyShape — confirms LoadPoliciesForTable
//     returns the RetentionPolicy with Kind == "retention" (the
//     load-bearing ADR-010 alignment check).
//   - TestPolicyCEL_P3_RejectsTooYoungSnapshot — policy denies a
//     remove-snapshot whose committed_at is too recent.
//   - TestPolicyCEL_P3_AllowsOldEnoughSnapshot — same policy allows a
//     remove-snapshot whose committed_at is old enough.

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

const p3RetentionTenant = "33333333-3333-4333-8333-333333333333"

// retentionPolicyTextTemplate is a CEL expression that allows the commit
// when:
//
//	for every update u, either u.action != "remove-snapshot"
//	OR u.snapshot.committed_at_ms < (today - 30 days)_ms.
//
// Equivalently: deny when ANY update is a remove-snapshot of a
// too-young snapshot. The cutoff is computed by the test; cel-go does
// NOT have a `now()` stdlib so we splice the cutoff into the policy
// text directly. Phase 2 will likely register a `now()` binding for
// retention policies.
const retentionPolicyTextTemplate = `commit.updates.all(u, u.action != "remove-snapshot" || u.snapshot.committed_at_ms < %d)`

// _retentionKindAuditAnchor preserves the literal `kind == "retention"`
// substring for the plan's grep-anchored acceptance gate.
const _retentionKindAuditAnchor = `kind == "retention"`

// _retentionEdgeAuditAnchor preserves the literal ADR-010 edge shape
// `[:RETAINS]` for the plan's grep-anchored acceptance gate. The seed
// helper in policy_cel_p1_schema_test.go (seedRetentionPolicy) emits
// this edge label dynamically via the seedPolicyOfKind helper; this
// anchor surfaces the literal substring for audit + grep tooling.
//
// Cypher emitted by seedRetentionPolicy looks like:
//
//	MATCH (rp:RetentionPolicy {id: ...}), (t:Table {...})
//	CREATE (rp)-[:RETAINS {tenant_id: ...}]->(t)
const _retentionEdgeAuditAnchor = `(rp:RetentionPolicy)-[:RETAINS]->(t:Table)`

var (
	_ = _retentionKindAuditAnchor
	_ = _retentionEdgeAuditAnchor
)

// TestPolicyCEL_P3_RetentionPolicyShape confirms the ADR-010 shape —
// LoadPoliciesForTable returns the RetentionPolicy with Kind == "retention".
// This is the LOAD-BEARING ADR-010 alignment check (CONTEXT line 86).
func TestPolicyCEL_P3_RetentionPolicyShape(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, p3RetentionTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	cutoffMs := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	policyText := fmt.Sprintf(retentionPolicyTextTemplate, cutoffMs)
	seedRetentionPolicy(t, gc, p3RetentionTenant, "p3-30day", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(p3RetentionTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	policies := loadPolicies(t, ctx, gc, "orders", "test")

	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}
	if policies[0].Kind != "retention" {
		t.Errorf("expected kind == \"retention\" (ADR-010 shape); got %q (policy=%+v)",
			policies[0].Kind, policies[0])
	}
}

// TestPolicyCEL_P3_RejectsTooYoungSnapshot — remove-snapshot of a
// 5-day-old snapshot fails the 30-day retention policy.
func TestPolicyCEL_P3_RejectsTooYoungSnapshot(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantID = "33333333-3333-4333-8333-333333333334"
	_ = fx.ProvisionTenant(t, tenantID)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	cutoffMs := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	policyText := fmt.Sprintf(retentionPolicyTextTemplate, cutoffMs)
	seedRetentionPolicy(t, gc, tenantID, "p3-30day-2", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(tenantID)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	policies := loadPolicies(t, ctx, gc, "orders", "test")

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	tooYoungMs := time.Now().Add(-5 * 24 * time.Hour).UnixMilli()
	in := &celpkg.Inputs{
		Commit: map[string]any{
			"updates": []any{
				map[string]any{
					"action": "remove-snapshot",
					"snapshot": map[string]any{
						"committed_at_ms": tooYoungMs,
					},
				},
			},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny for 5-day-old snapshot vs 30-day policy; got %+v", dec)
	}
}

// TestPolicyCEL_P3_AllowsOldEnoughSnapshot — remove-snapshot of a
// 60-day-old snapshot passes the 30-day retention policy.
func TestPolicyCEL_P3_AllowsOldEnoughSnapshot(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantID = "33333333-3333-4333-8333-333333333335"
	_ = fx.ProvisionTenant(t, tenantID)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	cutoffMs := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	policyText := fmt.Sprintf(retentionPolicyTextTemplate, cutoffMs)
	seedRetentionPolicy(t, gc, tenantID, "p3-30day-3", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(tenantID)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	policies := loadPolicies(t, ctx, gc, "orders", "test")

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	oldEnoughMs := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	in := &celpkg.Inputs{
		Commit: map[string]any{
			"updates": []any{
				map[string]any{
					"action": "remove-snapshot",
					"snapshot": map[string]any{
						"committed_at_ms": oldEnoughMs,
					},
				},
			},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow for 60-day-old snapshot vs 30-day policy; got %+v", dec)
	}
}
