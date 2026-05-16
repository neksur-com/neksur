//go:build integration

// Plan 02-12 — TestSqlProxyTrino_WhereSplice (CR-A3 acceptance test).
//
// End-to-end coverage of the Phase 2 SQL proxy splicer for the Trino
// dialect. Seeds an active CompiledPolicy with a row-filter artifact,
// drives the sqlproxy HTTP handler through 6 SELECT shapes, and asserts:
//
//   1. The 5 supported shapes (no WHERE; existing WHERE; WHERE+GROUP BY;
//      WHERE+ORDER BY; WHERE+LIMIT) all return HTTP 200 with a
//      `rewritten_query` body that contains the spliced WHERE predicate
//      (`region = 'us-east-1'` wrapped in the canonical `WHERE (...)`
//      or AND-conjoined into an existing clause).
//   2. The unsupported shape (JOIN) returns HTTP 422 + the distinct
//      metric label `sql_proxy_inject_failures_total{reason=
//      "unsupported_query_shape"}` (Plan 02-12 reason label).
//
// The test reuses the in-process startSqlproxyTestServer helper from
// sqlproxy_trino_test.go (file-local; same `integration` build tag).
// Spark dialect mirror lives in sqlproxy_spark_splice_test.go.

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

const sqlProxyTrinoSpliceTenant = "d50cd50c-0212-4d55-8a55-555555555555"

// TestSqlProxyTrino_WhereSplice — see file header.
func TestSqlProxyTrino_WhereSplice(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyTrinoSpliceTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyTrinoSpliceTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	defer gc.Close()

	const policyID = "splice-row-filter"
	const tableName = "orders"
	const tableNS = "ns"
	const trinoVersion = "467"
	seedPolicyOfKind(t, gc, sqlProxyTrinoSpliceTenant, policyID,
		`region = 'us-east-1'`, tableName, tableNS, "Policy", "row_filter",
		"ROW_FILTER_GOVERNS")

	tenantUUID := uuid.MustParse(sqlProxyTrinoSpliceTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	cstore := store.NewCompiledStore(gc)
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "trino",
		EngineVersion:  trinoVersion,
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusActive,
		SourceChecksum: "splicebeef",
		ArtifactBody:   "region = 'us-east-1'",
		ArtifactKind:   store.KindRowFilter,
	}))

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()
	client := newSqlproxyTestClient(t, caPEM, clientCert)

	// Snapshot the unsupported-shape counter so we can verify the
	// JOIN sub-case increments it by exactly 1.
	beforeUnsupportedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"trino", observability.ReasonSqlProxyUnsupportedQueryShape))

	cases := []struct {
		name              string
		query             string
		expectStatus      int
		expectSubstrings  []string // every substring must be present (200 case)
		expectSubstrAbsent []string // every substring must be absent (200 case)
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
				ts.URL+"/v1/sql/trino/myprefix/dummy",
				body, sqlProxyTrinoSpliceTenant)
			require.Equal(t, tc.expectStatus, resp.StatusCode,
				"unexpected status; body=%s", rewritten)
			if tc.expectStatus != http.StatusOK {
				return
			}
			for _, sub := range tc.expectSubstrings {
				require.Contains(t, rewritten, sub,
					"expected rewritten_query to contain %q; got %q", sub, rewritten)
			}
			for _, absent := range tc.expectSubstrAbsent {
				require.NotContains(t, rewritten, absent,
					"expected rewritten_query NOT to contain %q; got %q", absent, rewritten)
			}
			// Sanity: the original `WHERE` (or new WHERE) must appear,
			// and the original table reference must be preserved.
			require.True(t, strings.Contains(rewritten, "WHERE"),
				"rewritten_query missing WHERE: %q", rewritten)
			require.Contains(t, rewritten, tableName,
				"rewritten_query missing table reference: %q", rewritten)
		})
	}

	// Assert the JOIN sub-case incremented the unsupported-shape counter
	// by exactly 1. The 5 supported sub-cases must NOT have incremented
	// it (they returned 200).
	afterUnsupportedCount := testutil.ToFloat64(
		observability.SqlProxyInjectFailuresTotal.WithLabelValues(
			"trino", observability.ReasonSqlProxyUnsupportedQueryShape))
	require.Equal(t, beforeUnsupportedCount+1, afterUnsupportedCount,
		"sql_proxy_inject_failures_total{trino, unsupported_query_shape} "+
			"must increment by exactly 1 (one JOIN sub-case); before=%v after=%v",
		beforeUnsupportedCount, afterUnsupportedCount)
}
