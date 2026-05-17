//go:build integration

// Plan 03-05 Task 2 — sqlproxy Dremio integration tests.
//
// Three integration test behaviors exercising the live DremioInjector
// (Phase 3 D-3.02) through the real sqlproxy HTTP handler:
//
//  1. TestSqlProxyDremio_WhereSplice: boots Phase2Fixture, provisions
//     a fresh tenant, seeds an active dremio CompiledPolicy row-filter
//     artifact, drives the handler through 6 SELECT shapes (5 supported
//     + 1 JOIN rejection), asserts spliced WHERE clause in all 5
//     supported cases and HTTP 422 for the JOIN case.
//
//  2. TestSqlProxyDremio_NoPolicyPassthrough: no CompiledPolicy seeded
//     for the dremio engine → injector returns ErrPolicyEngineUnavailable
//     → HTTP 503 (fail-closed posture; Dremio has no "pass-through"
//     mode — the injector always returns an error when no active policy
//     exists for the table).
//
//  3. TestSqlProxyDremio_DivergentSuspendedFailsClosed: an active
//     CompiledPolicy is first seeded (so the injector hits the store),
//     then updated to status=divergent_suspended; the next sqlproxy
//     request must return HTTP 503 +
//     sql_proxy_inject_failures_total{reason='policy_engine_unavailable'}
//     increments. Note: the cache must be bypassed — use a fresh server
//     instance (fresh LRU) after the status flip to avoid a cache hit
//     on the still-active artifact.
//
// Per Pitfall 11: test assertions check status codes, result-row counts,
// and metric labels only — never query body content in error branches.
//
// Dremio container: these tests use Phase2Fixture (not Phase3Fixture) —
// the DremioInjector only talks to the AGE graph store (for CompiledPolicy
// nodes), not to a live Dremio SQL endpoint. Phase 3 live Dremio endpoint
// forwarding lands in Plan 03-09. Using Phase2Fixture keeps startup time
// to ~60s (no Dremio Docker container needed here).
//
// divergent_suspended flip: done via store.UpsertCompiledPolicy with the
// new status — cleaner than raw Cypher UPDATE and exercises the same
// code path an operator would use.

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/observability"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const sqlProxyDremioSpliceTenant = "d70cd70c-0305-4d77-8a77-777777777777"
const sqlProxyDremioNoPolicyTenant = "d71cd71c-0305-4d77-8a77-777777777771"
const sqlProxyDremioInvalidTenant = "d72cd72c-0305-4d77-8a77-777777777772"

// TestSqlProxyDremio_WhereSplice — see file header (behavior 1).
//
// Mirrors TestSqlProxyTrino_WhereSplice from sqlproxy_trino_splice_test.go
// for the dremio engine. The DremioInjector is now live (Phase 3 D-3.02);
// this test exercises the full sqlproxy handler stack including cache
// population on the first request and cache hits on subsequent requests.
func TestSqlProxyDremio_WhereSplice(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyDremioSpliceTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyDremioSpliceTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	defer gc.Close()

	const policyID = "dremio-row-filter"
	const tableName = "orders"
	const tableNS = "ns"
	// dremio version must match Phase2Fixture.ProvisionEngineRegistry's
	// version for "dremio" (see phase2_fixtures.go versionFor).
	const dremioVersion = "25.0"

	seedPolicyOfKind(t, gc, sqlProxyDremioSpliceTenant, policyID,
		`deleted = false`, tableName, tableNS, "Policy", "row_filter",
		"ROW_FILTER_GOVERNS")

	tenantUUID := uuid.MustParse(sqlProxyDremioSpliceTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	cstore := store.NewCompiledStore(gc)
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "dremio",
		EngineVersion:  dremioVersion,
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusActive,
		SourceChecksum: "dremiodeadbeef",
		// Row-filter artifact: bare predicate (no WHERE keyword).
		// The splicer wraps it in WHERE (...) or AND-conjoins into an existing clause.
		ArtifactBody: "deleted = false",
		ArtifactKind: store.KindRowFilter,
	}))

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()
	client := newSqlproxyTestClient(t, caPEM, clientCert)

	// Snapshot the unsupported-shape counter so we can verify the
	// JOIN sub-case increments it by exactly 1.
	beforeUnsupportedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"dremio", observability.ReasonSqlProxyUnsupportedQueryShape))

	cases := []struct {
		name             string
		query            string
		expectStatus     int
		expectSubstrings []string
	}{
		{
			name:         "no_where_appended",
			query:        "SELECT * FROM orders",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (deleted = false)",
			},
		},
		{
			name:         "existing_where_and_conjoined",
			query:        "SELECT * FROM orders WHERE id > 10",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (id > 10) AND (deleted = false)",
			},
		},
		{
			name:         "where_plus_group_by",
			query:        "SELECT region, count(*) FROM orders WHERE id > 10 GROUP BY region",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (id > 10) AND (deleted = false)",
				"GROUP BY region",
			},
		},
		{
			name:         "where_plus_order_by",
			query:        "SELECT * FROM orders WHERE id > 10 ORDER BY id",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (id > 10) AND (deleted = false)",
				"ORDER BY id",
			},
		},
		{
			name:         "where_plus_limit",
			query:        "SELECT * FROM orders WHERE id > 10 LIMIT 10",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (id > 10) AND (deleted = false)",
				"LIMIT 10",
			},
		},
		{
			name:         "join_rejected",
			query:        "SELECT * FROM orders JOIN customers ON orders.cid = customers.id",
			expectStatus: http.StatusUnprocessableEntity,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := mustJSON(t, map[string]any{
				"query": tc.query,
				"table": map[string]string{"namespace": tableNS, "name": tableName},
				"principal": map[string]any{
					"sub":   "u1",
					"email": "u1@example.com",
					"roles": []string{"analyst"},
				},
			})
			resp, rewritten := doSqlproxyPOST(t, client,
				ts.URL+"/v1/sql/dremio/myprefix/dummy",
				body, sqlProxyDremioSpliceTenant)
			require.Equal(t, tc.expectStatus, resp.StatusCode,
				"unexpected status; body=%s", rewritten)
			if tc.expectStatus != http.StatusOK {
				return
			}
			for _, sub := range tc.expectSubstrings {
				require.Contains(t, rewritten, sub,
					"expected rewritten_query to contain %q; got %q", sub, rewritten)
			}
			// Sanity: the WHERE clause must appear in the rewritten query,
			// and the table reference must be preserved.
			require.True(t, strings.Contains(rewritten, "WHERE"),
				"rewritten_query missing WHERE: %q", rewritten)
			require.Contains(t, rewritten, tableName,
				"rewritten_query missing table reference: %q", rewritten)
		})
	}

	// Assert the JOIN sub-case incremented the unsupported-shape counter
	// by exactly 1. The 5 supported sub-cases must NOT have incremented it.
	afterUnsupportedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"dremio", observability.ReasonSqlProxyUnsupportedQueryShape))
	require.Equal(t, beforeUnsupportedCount+1, afterUnsupportedCount,
		"sql_proxy_inject_failures_total{dremio, unsupported_query_shape} "+
			"must increment by exactly 1 (one JOIN sub-case); before=%v after=%v",
		beforeUnsupportedCount, afterUnsupportedCount)
}

// TestSqlProxyDremio_NoPolicyPassthrough — see file header (behavior 2).
//
// When no CompiledPolicy exists for the table+dremio engine pair, the
// DremioInjector returns ErrPolicyEngineUnavailable (fail-closed).
// The sqlproxy server maps this to HTTP 503.
func TestSqlProxyDremio_NoPolicyPassthrough(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyDremioNoPolicyTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyDremioNoPolicyTenant,
		[]string{"trino", "spark", "dremio"})

	// NOTE: intentionally do NOT seed any CompiledPolicy for dremio.
	// The injector should return ErrPolicyEngineUnavailable → 503.

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()
	client := newSqlproxyTestClient(t, caPEM, clientCert)

	body := mustJSON(t, map[string]any{
		"query": "SELECT id FROM orders",
		"table": map[string]string{"namespace": "ns", "name": "orders"},
		"principal": map[string]any{
			"sub":   "u1",
			"email": "u1@example.com",
			"roles": []string{"analyst"},
		},
	})

	// Snapshot the policy-engine-unavailable counter.
	beforeFailedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"dremio", observability.ReasonSqlProxyPolicyEngineUnavailable))

	resp, rawBody := doSqlproxyPOST(t, client,
		ts.URL+"/v1/sql/dremio/myprefix/dummy",
		body, sqlProxyDremioNoPolicyTenant)
	// DremioInjector: no active policy → ErrPolicyEngineUnavailable → 503.
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"no-policy case must be fail-closed (503); body=%s", rawBody)

	afterFailedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"dremio", observability.ReasonSqlProxyPolicyEngineUnavailable))
	require.Equal(t, beforeFailedCount+1, afterFailedCount,
		"sql_proxy_inject_failures_total{dremio, policy_engine_unavailable} "+
			"must increment by 1; before=%v after=%v",
		beforeFailedCount, afterFailedCount)
}

// TestSqlProxyDremio_DivergentSuspendedFailsClosed — see file header (behavior 3).
//
// A CompiledPolicy in status=divergent_suspended is treated as fail-closed
// per D-3.05 / T-3-dremio-divergent-bypass. The test:
//  1. Seeds an active CompiledPolicy (dremio, row-filter).
//  2. Flips status to divergent_suspended via UpsertCompiledPolicy.
//  3. Starts a FRESH sqlproxy server (fresh LRU cache) to bypass the cache.
//  4. Asserts HTTP 503 + metric increment.
func TestSqlProxyDremio_DivergentSuspendedFailsClosed(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyDremioInvalidTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyDremioInvalidTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	defer gc.Close()

	const policyID = "dremio-divergent-policy"
	const tableName = "orders"
	const tableNS = "ns"
	const dremioVersion = "25.0"

	seedPolicyOfKind(t, gc, sqlProxyDremioInvalidTenant, policyID,
		`deleted = false`, tableName, tableNS, "Policy", "row_filter",
		"ROW_FILTER_GOVERNS")

	tenantUUID := uuid.MustParse(sqlProxyDremioInvalidTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	cstore := store.NewCompiledStore(gc)

	// Step 1: seed active policy.
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "dremio",
		EngineVersion:  dremioVersion,
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusActive,
		SourceChecksum: "dremio-divergent-hash",
		ArtifactBody:   "deleted = false",
		ArtifactKind:   store.KindRowFilter,
	}))

	// Step 2: flip to divergent_suspended.
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "dremio",
		EngineVersion:  dremioVersion,
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusDivergentSuspended,
		SourceChecksum: "dremio-divergent-hash",
		ArtifactBody:   "deleted = false",
		ArtifactKind:   store.KindRowFilter,
	}))

	// Step 3: fresh server (fresh LRU cache) to bypass cache.
	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()
	client := newSqlproxyTestClient(t, caPEM, clientCert)

	body := mustJSON(t, map[string]any{
		"query": "SELECT id FROM orders",
		"table": map[string]string{"namespace": tableNS, "name": tableName},
		"principal": map[string]any{
			"sub":   "u1",
			"email": "u1@example.com",
			"roles": []string{"analyst"},
		},
	})

	// Snapshot counter before.
	beforeFailedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"dremio", observability.ReasonSqlProxyPolicyEngineUnavailable))

	// Step 4: assert 503.
	resp, rawBody := doSqlproxyPOST(t, client,
		ts.URL+"/v1/sql/dremio/myprefix/dummy",
		body, sqlProxyDremioInvalidTenant)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"divergent_suspended must be fail-closed (503); body=%s", rawBody)

	afterFailedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"dremio", observability.ReasonSqlProxyPolicyEngineUnavailable))
	require.Equal(t, beforeFailedCount+1, afterFailedCount,
		"sql_proxy_inject_failures_total{dremio, policy_engine_unavailable} "+
			"must increment by 1 on divergent_suspended; before=%v after=%v",
		beforeFailedCount, afterFailedCount)
}
