//go:build integration

// sqlproxy_metric_isolation_test.go — Plan 02-10 WR-A3 regression coverage.
//
// Asserts that a sqlproxy 503-on-policy-engine-unavailable response
// increments sql_proxy_inject_failures_total{reason="policy_engine_unavailable"}
// ONLY, and does NOT increment commit_rejected_total{reason="policy_engine_unavailable"}.
//
// Why this matters: Phase 1 alert rules page on commit_rejected_total.
// Iteration-1's CR-09 fix accidentally added a CommitRejectedTotal.Inc()
// call inside the sqlproxy 503 branch — meaning Phase 2 sqlproxy traffic
// (which can spike for non-incident reasons like an artifact compile
// being slow) would page on the same counter as L1 catalog-gateway
// fail-closed events. The duplicate-increment was removed in this plan;
// this test locks the contract against regression.

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/observability"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	"github.com/neksur-com/neksur/internal/tenant"
)

// TestSqlProxy_PolicyEngineUnavailable_DoesNotIncrementCommitRejected
// boots a minimal sqlproxy.Server with a stub Injector returning
// sqlproxy.ErrPolicyEngineUnavailable, captures Prometheus snapshots
// before + after, and asserts:
//
//   - commit_rejected_total{reason="policy_engine_unavailable"} delta == 0
//     (L1 catalog gateway only — must NOT increment from sqlproxy)
//   - sql_proxy_inject_failures_total{engine="trino", reason="policy_engine_unavailable"}
//     delta >= 1 (the sqlproxy path's documented metric home)
//
// The Server is constructed via NewServer rather than calling
// handleInject directly so the test covers the full HTTP path (path
// regex, body parse, tenant context, error switch).
func TestSqlProxy_PolicyEngineUnavailable_DoesNotIncrementCommitRejected(t *testing.T) {
	t.Parallel()

	// Stub Injector — always returns a wrapped ErrPolicyEngineUnavailable
	// so the server's error switch lands in the WR-A3 branch under test.
	// errors.Is on the typed wrapper resolves to the package sentinel
	// (see errPolicyEngineUnavailableWrap.Unwrap below).
	injector := &sqlProxyMetricIsolationInjector{
		err: wrapPolicyEngineUnavailable(),
	}

	srv, err := sqlproxy.NewServer(sqlproxy.Deps{
		Injectors: map[string]sqlproxy.Injector{
			"trino": injector,
		},
		// TLSConfig is required by NewServer's invariant check; a
		// non-nil empty *tls.Config satisfies the constructor without
		// triggering any real handshake — we drive the handler directly
		// via the returned *http.Handler.
		TLSConfig: &tls.Config{},
	})
	if err != nil {
		t.Fatalf("sqlproxy.NewServer: %v", err)
	}

	// Snapshot counters BEFORE the request.
	beforeCommitRejected := getCounterValue(t, observability.CommitRejectedTotal,
		observability.ReasonPolicyEngineUnavailable)
	beforeInjectFailures := getCounterValue(t, observability.SqlProxyInjectFailuresTotal,
		"trino", observability.ReasonSqlProxyPolicyEngineUnavailable)

	// Drive the handler in-process via httptest.NewServer so the full
	// dispatch path (path regex → body parse → tenant ctx → error switch)
	// runs. Use Server.Handler() to bypass TLS.
	httpSrv := httptest.NewServer(addTenantMW(srv.Handler()))
	t.Cleanup(httpSrv.Close)

	body, _ := json.Marshal(map[string]any{
		"query": "SELECT 1 FROM orders",
		"table": map[string]string{
			"namespace": "sales",
			"name":      "orders",
		},
		"principal": map[string]any{
			"sub":   "test-sub",
			"email": "test@example.com",
			"roles": []string{"analyst"},
		},
	})
	req, err := http.NewRequest(http.MethodPost,
		httpSrv.URL+"/v1/sql/trino/myprefix/exec", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (policy engine unavailable). body=%s",
			resp.StatusCode, string(rb))
	}

	// Snapshot counters AFTER the request.
	afterCommitRejected := getCounterValue(t, observability.CommitRejectedTotal,
		observability.ReasonPolicyEngineUnavailable)
	afterInjectFailures := getCounterValue(t, observability.SqlProxyInjectFailuresTotal,
		"trino", observability.ReasonSqlProxyPolicyEngineUnavailable)

	// WR-A3 invariant: commit_rejected_total MUST NOT increment from the
	// sqlproxy path. The Phase 1 L1 catalog gateway is the sole increment
	// site (per the doc-comment contract in internal/observability/metrics.go).
	if delta := afterCommitRejected - beforeCommitRejected; delta != 0 {
		t.Errorf("commit_rejected_total{policy_engine_unavailable} incremented by %v on sqlproxy 503 — "+
			"WR-A3 regression (Phase 1 alert rules will page on sqlproxy traffic). "+
			"before=%v after=%v", delta, beforeCommitRejected, afterCommitRejected)
	}

	// Documented sqlproxy metric must fire.
	if delta := afterInjectFailures - beforeInjectFailures; delta < 1 {
		t.Errorf("sql_proxy_inject_failures_total{engine=trino, reason=policy_engine_unavailable} "+
			"did not increment: before=%v after=%v (delta=%v)",
			beforeInjectFailures, afterInjectFailures, delta)
	}
}

// addTenantMW injects a tenant UUID into the request ctx so the
// sqlproxy handler's defence-in-depth tenant assertion passes. Mirrors
// the wrap shim in gateway_helpers_test.go.
func addTenantMW(h http.Handler) http.Handler {
	tenantUUID := uuid.MustParse("10000010-0010-4010-8010-00000000a3a3")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := tenant.WithID(r.Context(), tenantUUID)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// wrapPolicyEngineUnavailable produces an error that errors.Is the
// sqlproxy sentinel — covers the Injector contract (the server checks
// errors.Is against the package's exported sentinels).
func wrapPolicyEngineUnavailable() error {
	return errPolicyEngineUnavailableWrap{}
}

// errPolicyEngineUnavailableWrap is a tiny wrapper that satisfies
// errors.Is by Unwrap-ing to sqlproxy.ErrPolicyEngineUnavailable. We use
// a typed wrapper rather than fmt.Errorf(%w) inline to keep the test's
// intent self-documenting at the call site.
type errPolicyEngineUnavailableWrap struct{}

func (errPolicyEngineUnavailableWrap) Error() string {
	return "sqlproxy_metric_isolation_test: stub injector returning policy engine unavailable"
}

func (errPolicyEngineUnavailableWrap) Unwrap() error {
	return sqlproxy.ErrPolicyEngineUnavailable
}

// sqlProxyMetricIsolationInjector is a minimal sqlproxy.Injector that
// returns a configured error on every InjectPolicy call. Used to drive
// the server's error switch into the ErrPolicyEngineUnavailable branch.
type sqlProxyMetricIsolationInjector struct {
	err error
}

func (i *sqlProxyMetricIsolationInjector) InjectPolicy(
	_ context.Context,
	_ string,
	_ sqlproxy.TableRef,
	_ sqlproxy.Claims,
) (string, string, error) {
	return "", sqlproxy.CacheStatusError, i.err
}

var _ sqlproxy.Injector = (*sqlProxyMetricIsolationInjector)(nil)
