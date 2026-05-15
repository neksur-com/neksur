//go:build integration && polaris

// Plan 01-06 Task 3 [BLOCKING] — gateway proxy forward + 403 deny.
//
// TestGatewayCommitProxyForwardsOnPass:
//   1. Bootstrap T1 + Phase1Fixture (creates the catalog_credentials
//      polaris row + the per-tenant graph schema).
//   2. Seed a Policy node `definition_cel = "true"` (always allow) +
//      SCHEMA_GOVERNS edge to Table {name: 'orders', namespace: 'test'}.
//   3. POST a valid CommitRequest to /v1/iceberg/prod-polaris/...
//   4. Assert 200; assert the fake adapter received the commit (Plan
//      01-06 uses a fake adapter via AdapterFactory because the live
//      Polaris CreateTable + STS path is deferred — see
//      gateway_helpers_test.go header for the rationale).
//   5. Assert response body decodes to a CommitResult with the canned
//      NewMetadataLocation.
//
// TestGatewayCommitProxyRejectsOn403:
//   1. Same setup but seed `definition_cel = "false"` (always deny).
//   2. POST commit; assert 403 + "policy denied" in body.
//   3. Assert fake adapter recorded NO CommitTable calls (deny
//      short-circuits BEFORE forward).
//   4. Assert audit_log row decision='REJECTED'.

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/tenant"
)

const gatewayProxyTenant = "10000001-0001-4001-8001-000000000001"

// TestGatewayCommitProxyForwardsOnPass — happy path: allow-all policy,
// gateway forwards, response 200 + body echoes upstream result.
func TestGatewayCommitProxyForwardsOnPass(t *testing.T) {
	h := startGatewayHarness(t, gatewayProxyTenant)

	// Seed allow-all policy on test.orders.
	seedSchemaPolicy(t, h.gc, h.tenantStr, "p1-allow", "true", "orders", "test")

	status, body := h.postCommit(t, "prod-polaris", "test", "orders", validCommitBody())
	if status != 200 {
		t.Fatalf("status = %d; want 200. body=%s", status, body)
	}

	// Fake adapter received exactly one CommitTable call.
	if len(h.fake.CommitsReceived) != 1 {
		t.Fatalf("CommitsReceived = %d; want 1", len(h.fake.CommitsReceived))
	}
	got := h.fake.CommitsReceived[0]
	if got.Ref.Name != "orders" || len(got.Ref.Namespace) != 1 || got.Ref.Namespace[0] != "test" {
		t.Errorf("forwarded ref = %+v; want {Namespace:[test] Name:orders}", got.Ref)
	}

	// Body echoed the canned CommitResult.
	result := extractDecodedResult(body)
	if result == nil {
		t.Fatalf("response body did not decode as CommitResult: %s", body)
	}
	if result.NewMetadataLocation == "" {
		t.Errorf("NewMetadataLocation empty in response: %+v", result)
	}
}

// TestGatewayCommitProxyRejectsOn403 — deny-all policy: 403 + no
// upstream forward + REJECTED audit row.
func TestGatewayCommitProxyRejectsOn403(t *testing.T) {
	const tenantStr = "10000001-0001-4001-8001-000000000002"
	h := startGatewayHarness(t, tenantStr)

	// Seed deny-all policy.
	seedSchemaPolicy(t, h.gc, h.tenantStr, "p1-deny", "false", "orders", "test")

	status, body := h.postCommit(t, "prod-polaris", "test", "orders", validCommitBody())
	if status != 403 {
		t.Fatalf("status = %d; want 403. body=%s", status, body)
	}
	if !strings.Contains(body, "policy denied") {
		t.Errorf("body does not contain 'policy denied': %s", body)
	}

	// Fake adapter saw a LoadTable but NOT a CommitTable — the deny
	// short-circuits before upstream forward.
	if len(h.fake.CommitsReceived) != 0 {
		t.Errorf("CommitsReceived = %d; want 0 (deny short-circuit)", len(h.fake.CommitsReceived))
	}
	if len(h.fake.LoadsReceived) == 0 {
		t.Errorf("LoadsReceived = 0; gateway should have called LoadTable BEFORE policy eval")
	}

	// audit_log row exists with decision = REJECTED.
	decision, source, hash := queryAuditLogDecision(t, context.Background(), h)
	if decision != "REJECTED" {
		t.Errorf("audit_log decision = %q; want REJECTED", decision)
	}
	if source == "" {
		t.Errorf("audit_log principal_source empty (Pitfall 8)")
	}
	if len(hash) == 0 {
		t.Errorf("audit_log commit_request_hash empty")
	}

	// WriteEvent {decision: 'REJECTED'} exists in the graph.
	weCount := countMatching(t, context.Background(), h.gc, h.tenantStr,
		`MATCH (we:WriteEvent) WHERE we.decision = 'REJECTED' RETURN count(we)`)
	if weCount < 1 {
		t.Errorf("WriteEvent REJECTED count = %d; want >= 1", weCount)
	}
}

// Compile-time anchors so the test file builds standalone (the JSON
// parse + tenant ctx imports are referenced by the helpers but the
// test itself doesn't use them — the unused-import lint would
// otherwise drop them).
var _ = json.Unmarshal
var _ = tenant.WithID
var _ = pgx.ErrNoRows
