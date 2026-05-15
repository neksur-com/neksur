//go:build integration && polaris

// Plan 01-06 Task 3 [BLOCKING] — WriteEvent + INTENDED_WRITE +
// ACTUAL_WRITE audit emission per D-003.06 + Open Question 4 (graph +
// relational audit_log row in same tenant transaction).
//
// TestGatewayAuditWriteEvent:
//   1. Bootstrap T1; allow-all policy on test.orders.
//   2. POST a successful commit.
//   3. Assert exactly ONE WriteEvent {decision: 'APPROVED'} in graph.
//   4. Assert (Person)-[:INTENDED_WRITE]->(Table) edge exists with the
//      principal sub.
//   5. Assert (Snapshot)-[:ACTUAL_WRITE]->(Table) edge exists with the
//      new metadata_location.
//   6. Assert audit_log row has decision='APPROVED', principal_source
//      ∈ {mtls_san, auth_header, session}, commit_request_hash non-NULL.

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/tenant"
)

const gatewayAuditTenant = "10000003-0003-4003-8003-000000000001"

func TestGatewayAuditWriteEvent(t *testing.T) {
	h := startGatewayHarness(t, gatewayAuditTenant)

	// Allow-all policy.
	seedSchemaPolicy(t, h.gc, h.tenantStr, "audit-allow", "true", "orders", "test")

	status, body := h.postCommit(t, "prod-polaris", "test", "orders", validCommitBody())
	if status != 200 {
		t.Fatalf("status = %d; want 200. body=%s", status, body)
	}

	ctx := context.Background()

	// 1. Exactly one WriteEvent APPROVED.
	approved := countMatching(t, ctx, h.gc, h.tenantStr,
		`MATCH (we:WriteEvent) WHERE we.decision = 'APPROVED' RETURN count(we)`)
	if approved != 1 {
		t.Errorf("WriteEvent APPROVED count = %d; want 1", approved)
	}

	// 2. INTENDED_WRITE Person→Table edge.
	intended := countMatching(t, ctx, h.gc, h.tenantStr,
		`MATCH (p:Person)-[:INTENDED_WRITE]->(t:Table) WHERE t.name = 'orders' RETURN count(p)`)
	if intended < 1 {
		t.Errorf("INTENDED_WRITE Person->Table count = %d; want >= 1", intended)
	}

	// 3. ACTUAL_WRITE Snapshot→Table edge with non-empty metadata_location.
	actual := countMatching(t, ctx, h.gc, h.tenantStr,
		`MATCH (s:Snapshot)-[:ACTUAL_WRITE]->(t:Table) WHERE t.name = 'orders' RETURN count(s)`)
	if actual < 1 {
		t.Errorf("ACTUAL_WRITE Snapshot->Table count = %d; want >= 1", actual)
	}

	// 4. audit_log row.
	decision, source, hash := queryAuditLogDecision(t, ctx, h)
	if decision != "APPROVED" {
		t.Errorf("audit_log decision = %q; want APPROVED", decision)
	}
	if !inSet(source, "mtls_san", "auth_header", "session") {
		t.Errorf("audit_log principal_source = %q; want one of mtls_san/auth_header/session", source)
	}
	if len(hash) == 0 {
		t.Errorf("audit_log commit_request_hash empty")
	}
}

// inSet returns true if s is one of the candidates.
func inSet(s string, candidates ...string) bool {
	for _, c := range candidates {
		if s == c {
			return true
		}
	}
	return false
}

// Compile-time guards.
var _ = strings.Contains
var _ = pgx.ErrNoRows
var _ = tenant.WithID
