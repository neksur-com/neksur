//go:build integration

// Plan 02-05 Wave 2 dispatch D — sqlproxy fail-closed tests.
//
// Two integration tests covering the proxy's REJECT paths:
//
//   1. TestSQLProxyFailClosedOnInactivePolicy — a CompiledPolicy node
//      exists for (tenant, table, trino) but its status is
//      `probe_failed` (not `active`). The TrinoInjector iterates and
//      finds no active artifact → returns ErrPolicyEngineUnavailable;
//      the server returns 503 + increments
//      commit_rejected_total{reason="policy_engine_unavailable"}
//      (D-1.09 reuse — the L1 gateway's fail-closed counter is
//      reused on the SQL proxy path so the existing Phase 1
//      dashboards / alert rules cover both surfaces).
//
//   2. TestSQLProxyFailClosedOnDremio — the Dremio Injector is a
//      Phase 2 stub that returns iceberg.ErrAdapterStub on every call.
//      The server's error switch maps this sentinel to HTTP 501
//      "engine not supported" so clients can distinguish "stub" from
//      "transient failure".

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/observability"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const (
	sqlProxyInactivePolicyTenant = "d30cd30c-0205-4d33-8a33-333333333333"
	sqlProxyDremioTenant         = "d40cd40c-0205-4d44-8a44-444444444444"
)

// TestSQLProxyFailClosedOnInactivePolicy — see file header.
func TestSQLProxyFailClosedOnInactivePolicy(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyInactivePolicyTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyInactivePolicyTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	defer gc.Close()

	const policyID = "inactive-policy-1"
	const tableName = "orders"
	const tableNS = "ns"
	seedPolicyOfKind(t, gc, sqlProxyInactivePolicyTenant, policyID,
		`region = 'us-east-1'`, tableName, tableNS, "Policy", "row_filter",
		"ROW_FILTER_GOVERNS")

	// Upsert a CompiledPolicy with status=probe_failed — the
	// TrinoInjector's "find active" loop will skip it and return
	// ErrPolicyEngineUnavailable.
	tenantUUID := uuid.MustParse(sqlProxyInactivePolicyTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	cstore := store.NewCompiledStore(gc)
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "trino",
		EngineVersion:  "467",
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusProbeFailed,
		SourceChecksum: "deadbeef",
		ArtifactBody:   "WHERE region='us-east-1'",
	}))

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()

	client := newSqlproxyTestClient(t, caPEM, clientCert)

	// Snapshot the counter so we measure the delta this request
	// produces (other tests in the same package may have already
	// incremented it; the process-wide promauto registry is shared).
	beforeCount := testutil.ToFloat64(
		observability.CommitRejectedTotal.WithLabelValues(
			observability.ReasonPolicyEngineUnavailable))

	body := mustJSON(t, map[string]any{
		"query": "SELECT * FROM orders",
		"table": map[string]string{"namespace": tableNS, "name": tableName},
		"principal": map[string]any{
			"sub":   "u1",
			"email": "u1@example.com",
			"roles": []string{"analyst"},
		},
	})
	resp, errBody := doSqlproxyPOST(t, client,
		ts.URL+"/v1/sql/trino/myprefix/dummy",
		body, sqlProxyInactivePolicyTenant)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"inactive (probe_failed) policy must produce 503; body=%s", errBody)
	require.Contains(t, errBody, "policy engine unavailable",
		"503 body must surface the policy-engine-unavailable reason")

	afterCount := testutil.ToFloat64(
		observability.CommitRejectedTotal.WithLabelValues(
			observability.ReasonPolicyEngineUnavailable))
	require.Equal(t, beforeCount+1, afterCount,
		"commit_rejected_total{reason=policy_engine_unavailable} must increment by exactly 1")
}

// TestSQLProxyFailClosedOnDremio — see file header.
func TestSQLProxyFailClosedOnDremio(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyDremioTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyDremioTenant,
		[]string{"trino", "spark", "dremio"})

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()

	client := newSqlproxyTestClient(t, caPEM, clientCert)
	body := mustJSON(t, map[string]any{
		"query": "SELECT * FROM orders",
		"table": map[string]string{"namespace": "ns", "name": "orders"},
		"principal": map[string]any{
			"sub":   "u1",
			"email": "u1@example.com",
			"roles": []string{"analyst"},
		},
	})

	resp, errBody := doSqlproxyPOST(t, client,
		ts.URL+"/v1/sql/dremio/myprefix/dummy",
		body, sqlProxyDremioTenant)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode,
		"Dremio stub must produce 501; body=%s", errBody)
	require.Contains(t, errBody, "engine not supported",
		"501 body must surface the engine-not-supported reason")
}
