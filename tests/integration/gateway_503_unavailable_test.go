//go:build integration

// Plan 01-06 Task 3 [BLOCKING] — D-1.09 fail-closed semantics:
//
// TestGateway503OnPolicyFetchFailure:
//   1. Bootstrap T1; seed allow-all policy.
//   2. CLOSE the graph client mid-test so LoadPoliciesForTable returns
//      a transport error.
//   3. POST a commit; assert 503 + body "policy-engine-unavailable".
//   4. Assert observability.CommitRejectedTotal{reason="policy_engine_unavailable"}
//      incremented by 1.
//
// TestGateway503OnCELPanic:
//   1. Bootstrap T1; seed a policy whose CEL text triggers a runtime
//      error inside the cel-go interpreter (we use a malformed
//      expression that compiles but evaluates to a type-error which
//      cel-go surfaces as ErrPolicyEvalFailed). The fail-closed gateway
//      maps any non-nil error to 503 — this exercises the same
//      counter-increment path even though the OUTER recover panic path
//      isn't fired (cel-go's internal recover catches plain panics
//      and converts to eval errors).
//   2. POST commit; assert 503; assert counter increment.

package integration

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/neksur-com/neksur/internal/observability"
)

const gateway503TenantA = "10000004-0004-4004-8004-000000000001"
const gateway503TenantB = "10000004-0004-4004-8004-000000000002"

// TestGateway503OnPolicyFetchFailure — close the graph client so
// LoadPoliciesForTable fails; assert 503 + counter increment.
func TestGateway503OnPolicyFetchFailure(t *testing.T) {
	h := startGatewayHarness(t, gateway503TenantA)

	// Seed a policy so the gateway TRIES to fetch (without policies the
	// allow-by-default short-circuits before fetch — but the AGEStore
	// always queries; an empty result is fine. We seed for parity with
	// the real failure path.).
	seedSchemaPolicy(t, h.gc, h.tenantStr, "fetch-fail-allow", "true", "orders", "test")

	beforeCount := getCounterValue(t, observability.CommitRejectedTotal,
		observability.ReasonPolicyEngineUnavailable)

	// Close the graph client mid-test so the next ExecuteInTenant fails.
	h.gc.Close()

	status, body := h.postCommit(t, "prod-polaris", "test", "orders", validCommitBody())
	if status != 503 {
		t.Fatalf("status = %d; want 503. body=%s", status, body)
	}
	if !strings.Contains(body, "policy-engine-unavailable") {
		t.Errorf("body should contain 'policy-engine-unavailable'; got: %s", body)
	}

	afterCount := getCounterValue(t, observability.CommitRejectedTotal,
		observability.ReasonPolicyEngineUnavailable)
	if afterCount-beforeCount < 1 {
		t.Errorf("CommitRejectedTotal{policy_engine_unavailable} did not increment: before=%v after=%v",
			beforeCount, afterCount)
	}
}

// TestGateway503OnCELPanic — seed a policy with a CEL expression that
// produces a runtime error during evaluation. cel-go converts the
// runtime error to ErrPolicyEvalFailed; the gateway maps any err to
// 503 + counter increment. This is the second D-1.09 fail-closed path.
func TestGateway503OnCELPanic(t *testing.T) {
	h := startGatewayHarness(t, gateway503TenantB)

	// `int(table.no_such_field) / 0` — cel-go's NewEnv declares table
	// as MapType(StringType, DynType), so accessing a missing key
	// returns a typed Value error at evaluation time. cel-go surfaces
	// this as a non-nil err from Program.ContextEval → fail-closed.
	const panicCEL = `int(table.no_such_field) / 0 > 0`
	seedSchemaPolicy(t, h.gc, h.tenantStr, "panic-policy", panicCEL, "orders", "test")

	beforeCount := getCounterValue(t, observability.CommitRejectedTotal,
		observability.ReasonPolicyEngineUnavailable)

	status, body := h.postCommit(t, "prod-polaris", "test", "orders", validCommitBody())
	if status != 503 {
		t.Fatalf("status = %d; want 503 (CEL eval err fail-closed). body=%s", status, body)
	}
	if !strings.Contains(body, "policy-engine-unavailable") {
		t.Errorf("body should contain 'policy-engine-unavailable'; got: %s", body)
	}

	afterCount := getCounterValue(t, observability.CommitRejectedTotal,
		observability.ReasonPolicyEngineUnavailable)
	if afterCount-beforeCount < 1 {
		t.Errorf("CommitRejectedTotal{policy_engine_unavailable} did not increment on CEL panic: before=%v after=%v",
			beforeCount, afterCount)
	}
}

// getCounterValue returns the float64 value of a CounterVec for the
// given label set. testutil.ToFloat64 reads the current sum without
// resetting.
func getCounterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	return testutil.ToFloat64(c.WithLabelValues(labels...))
}
