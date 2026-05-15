//go:build integration && polaris

// Plan 01-06 Task 3 [BLOCKING] — Multi-table Reject-All semantics
// (Pitfall 6).
//
// TestGatewayMultiTableCommitRejectAll:
//   1. Bootstrap T1 + Phase1Fixture.
//   2. Seed two tables: orders (allow-all policy) + items (deny-all policy).
//   3. POST a multi-table transaction body covering BOTH tables to
//      /v1/iceberg/prod-polaris/transactions/commit.
//   4. Assert 403 (Reject-All — ANY deny rejects the entire tx).
//   5. Assert NEITHER table received a CommitTable forward (the
//      Reject-All semantics short-circuit BEFORE upstream commits).
//   6. Assert exactly ONE WriteEvent {REJECTED} for the offending
//      table (`items`); ZERO {APPROVED} events for either.

package integration

import (
	"context"
	"strings"
	"testing"
)

const gatewayMultiTenant = "10000002-0002-4002-8002-000000000001"

// TestGatewayMultiTableCommitRejectAll — Pitfall 6 Reject-All proof.
func TestGatewayMultiTableCommitRejectAll(t *testing.T) {
	h := startGatewayHarness(t, gatewayMultiTenant)

	// Seed two policies — orders allows everything, items denies
	// everything. The multi-table body's order is (orders, items) —
	// the gateway iterates them in order, gates orders ALLOW, then
	// gates items DENY → Reject-All.
	seedSchemaPolicy(t, h.gc, h.tenantStr, "mt-allow", "true", "orders", "test")
	seedSchemaPolicy(t, h.gc, h.tenantStr, "mt-deny", "false", "items", "test")

	body := []byte(`{
		"table-changes": [
			{
				"identifier": {"namespace": ["test"], "name": "orders"},
				"requirements": [],
				"updates": [{"action":"set-properties","updates":{"x":"y"}}]
			},
			{
				"identifier": {"namespace": ["test"], "name": "items"},
				"requirements": [],
				"updates": [{"action":"set-properties","updates":{"x":"y"}}]
			}
		]
	}`)

	status, respBody := h.postMultiTable(t, "prod-polaris", body)
	if status != 403 {
		t.Fatalf("status = %d; want 403 (Reject-All). body=%s", status, respBody)
	}
	if !strings.Contains(respBody, "items") {
		t.Errorf("response body should mention the offending table 'items'; got: %s", respBody)
	}

	// Reject-All: ZERO upstream commits should have fired.
	if len(h.fake.CommitsReceived) != 0 {
		t.Errorf("CommitsReceived = %d; want 0 (Reject-All — no upstream forwards on any deny)", len(h.fake.CommitsReceived))
	}

	// Exactly ONE WriteEvent REJECTED (for `items`); ZERO APPROVED.
	rejected := countMatching(t, context.Background(), h.gc, h.tenantStr,
		`MATCH (we:WriteEvent) WHERE we.decision = 'REJECTED' RETURN count(we)`)
	if rejected != 1 {
		t.Errorf("WriteEvent REJECTED count = %d; want 1 (just the offending ref)", rejected)
	}
	approved := countMatching(t, context.Background(), h.gc, h.tenantStr,
		`MATCH (we:WriteEvent) WHERE we.decision = 'APPROVED' RETURN count(we)`)
	if approved != 0 {
		t.Errorf("WriteEvent APPROVED count = %d; want 0 (Reject-All — no APPROVED on any ref)", approved)
	}
}
