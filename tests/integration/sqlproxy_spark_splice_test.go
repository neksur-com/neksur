//go:build integration

// Plan 02-12 — TestSqlProxySpark_WhereSplice (CR-A3 acceptance test,
// Spark dialect mirror of sqlproxy_trino_splice_test.go).
//
// End-to-end coverage of the Phase 2 SQL proxy splicer for the Spark
// dialect. Seeds an active CompiledPolicy with a row-filter artifact
// for the spark engine, drives the sqlproxy HTTP handler through 6
// SELECT shapes, and asserts the same 5 supported / 1 rejected
// contract the Trino mirror asserts.
//
// Spark-specific note: the Phase 2 splicer accepts only `^[A-Za-z_]
// [A-Za-z0-9_]*$` identifiers (matching the existing sql_grammar.go
// pattern). Backtick-quoted identifiers (Spark SQL's preferred
// identifier-with-special-chars syntax) are explicitly NOT supported
// in Phase 2 — the Phase 2 contract is bare-identifier tables only;
// Phase 3 lifts the limit.

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

const sqlProxySparkSpliceTenant = "d60cd60c-0212-4d66-8a66-666666666666"

// TestSqlProxySpark_WhereSplice — see file header.
func TestSqlProxySpark_WhereSplice(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxySparkSpliceTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxySparkSpliceTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	defer gc.Close()

	const policyID = "splice-row-filter-spark"
	const tableName = "orders"
	const tableNS = "ns"
	// sparkVersion must match the version Phase2Fixture provisions in
	// ProvisionEngineRegistry (see tests/integration/phase2_fixtures.go).
	const sparkVersion = "3.5.0"
	seedPolicyOfKind(t, gc, sqlProxySparkSpliceTenant, policyID,
		`region = 'us-east-1'`, tableName, tableNS, "Policy", "row_filter",
		"ROW_FILTER_GOVERNS")

	tenantUUID := uuid.MustParse(sqlProxySparkSpliceTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	cstore := store.NewCompiledStore(gc)
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "spark",
		EngineVersion:  sparkVersion,
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusActive,
		SourceChecksum: "sparksplicebeef",
		ArtifactBody:   "region = 'us-east-1'",
		ArtifactKind:   store.KindRowFilter,
	}))

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()
	client := newSqlproxyTestClient(t, caPEM, clientCert)

	beforeUnsupportedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"spark", observability.ReasonSqlProxyUnsupportedQueryShape))

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
				"WHERE (region = 'us-east-1')",
			},
		},
		{
			name:         "existing_where_and_conjoined",
			query:        "SELECT * FROM orders WHERE total > 100",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (total > 100) AND (region = 'us-east-1')",
			},
		},
		{
			name:         "where_plus_group_by",
			query:        "SELECT region, count(*) FROM orders WHERE total > 100 GROUP BY region",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (total > 100) AND (region = 'us-east-1')",
				"GROUP BY region",
			},
		},
		{
			name:         "where_plus_order_by",
			query:        "SELECT * FROM orders WHERE total > 100 ORDER BY id",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (total > 100) AND (region = 'us-east-1')",
				"ORDER BY id",
			},
		},
		{
			name:         "where_plus_limit",
			query:        "SELECT * FROM orders WHERE total > 100 LIMIT 10",
			expectStatus: http.StatusOK,
			expectSubstrings: []string{
				"WHERE (total > 100) AND (region = 'us-east-1')",
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
				ts.URL+"/v1/sql/spark/myprefix/dummy",
				body, sqlProxySparkSpliceTenant)
			require.Equal(t, tc.expectStatus, resp.StatusCode,
				"unexpected status; body=%s", rewritten)
			if tc.expectStatus != http.StatusOK {
				return
			}
			for _, sub := range tc.expectSubstrings {
				require.Contains(t, rewritten, sub,
					"expected rewritten_query to contain %q; got %q", sub, rewritten)
			}
			require.True(t, strings.Contains(rewritten, "WHERE"),
				"rewritten_query missing WHERE: %q", rewritten)
			require.Contains(t, rewritten, tableName,
				"rewritten_query missing table reference: %q", rewritten)
		})
	}

	afterUnsupportedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"spark", observability.ReasonSqlProxyUnsupportedQueryShape))
	require.Equal(t, beforeUnsupportedCount+1, afterUnsupportedCount,
		"sql_proxy_inject_failures_total{spark, unsupported_query_shape} "+
			"must increment by exactly 1 (one JOIN sub-case); before=%v after=%v",
		beforeUnsupportedCount, afterUnsupportedCount)
}
