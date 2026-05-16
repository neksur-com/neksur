// Plan 02-12 Task 1 — SpliceArtifact unit tests.
//
// Table-driven coverage of the Phase 2 splice contract:
//
//   - row-filter splice: appends or AND-conjoins the artifact body as a
//     real WHERE-clause predicate, preserving GROUP BY / ORDER BY /
//     LIMIT tails.
//   - column-mask splice: rewrites the projection list to substitute
//     masked columns with the engine-native masked expression while
//     keeping the original column name as an alias.
//   - rejection: every shape outside the Phase 2 single-table SELECT
//     grammar (JOIN, subquery, CTE, multi-table comma, set operation,
//     non-SELECT DML) returns ErrUnsupportedQueryShape; malformed SQL
//     returns ErrInjectionFailed; column-mask referencing a column
//     absent from the projection returns ErrSpliceMismatch.
//
// CR-A3 closure: the env-gate fail-closed posture of iter-1 is gone —
// this test suite proves the real splice paths exist and surface
// loud, distinct error sentinels for every unsupported shape.

package dialect_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	"github.com/neksur-com/neksur/internal/sqlproxy/dialect"
)

// normalize collapses runs of whitespace to single spaces and trims
// edges. Splice output is tolerated through this normalizer so the
// test assertions are robust against cosmetic whitespace drift inside
// the splicer's emit code.
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func TestSpliceArtifact_RowFilter(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		body     string
		expected string
	}{
		{
			name:     "no_where_simple_star",
			query:    "SELECT * FROM orders",
			body:     "region = 'us-east'",
			expected: "SELECT * FROM orders WHERE (region = 'us-east')",
		},
		{
			name:     "no_where_specific_cols",
			query:    "SELECT id, total FROM orders",
			body:     "region = 'us-east'",
			expected: "SELECT id, total FROM orders WHERE (region = 'us-east')",
		},
		{
			name:     "existing_where_and_conjoin",
			query:    "SELECT * FROM orders WHERE total > 100",
			body:     "region = 'us-east'",
			expected: "SELECT * FROM orders WHERE (total > 100) AND (region = 'us-east')",
		},
		{
			name:     "where_plus_group_by",
			query:    "SELECT region, count(*) FROM orders WHERE total > 100 GROUP BY region",
			body:     "region = 'us-east'",
			expected: "SELECT region, count(*) FROM orders WHERE (total > 100) AND (region = 'us-east') GROUP BY region",
		},
		{
			name:     "where_plus_order_by_limit",
			query:    "SELECT * FROM orders WHERE total > 100 ORDER BY id LIMIT 10",
			body:     "region = 'us-east'",
			expected: "SELECT * FROM orders WHERE (total > 100) AND (region = 'us-east') ORDER BY id LIMIT 10",
		},
		{
			name:     "no_where_with_order_limit",
			query:    "SELECT * FROM orders ORDER BY id LIMIT 10",
			body:     "region = 'us-east'",
			expected: "SELECT * FROM orders WHERE (region = 'us-east') ORDER BY id LIMIT 10",
		},
		{
			name:     "multiline_extra_whitespace_input",
			query:    "SELECT\n  id,\n  total\nFROM    orders\n  WHERE  total > 100",
			body:     "region = 'us-east'",
			expected: "SELECT id, total FROM orders WHERE (total > 100) AND (region = 'us-east')",
		},
		{
			name:     "having_clause_preserved",
			query:    "SELECT region, count(*) FROM orders GROUP BY region HAVING count(*) > 5",
			body:     "region = 'us-east'",
			expected: "SELECT region, count(*) FROM orders WHERE (region = 'us-east') GROUP BY region HAVING count(*) > 5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dialect.SpliceArtifact(tc.query, []byte(tc.body), store.KindRowFilter, sqlproxy.Claims{})
			require.NoError(t, err)
			require.Equal(t, normalize(tc.expected), normalize(got))
		})
	}
}

func TestSpliceArtifact_ColumnMask(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		body     string
		expected string
	}{
		{
			name:     "literal_mask_replaces_column",
			query:    "SELECT id, ssn, email FROM customers",
			body:     "ssn AS '***'",
			expected: "SELECT id, '***' AS ssn, email FROM customers",
		},
		{
			name:     "literal_mask_preserves_where",
			query:    "SELECT id, ssn, email FROM customers WHERE id > 0",
			body:     "ssn AS '***'",
			expected: "SELECT id, '***' AS ssn, email FROM customers WHERE id > 0",
		},
		{
			name:     "hash_func_mask",
			query:    "SELECT id, ssn FROM customers",
			body:     "ssn AS sha256(ssn)",
			expected: "SELECT id, sha256(ssn) AS ssn FROM customers",
		},
		{
			name:     "multiple_masks",
			query:    "SELECT id, ssn, email, phone FROM customers",
			body:     "ssn AS '***', email AS 'redacted'",
			expected: "SELECT id, '***' AS ssn, 'redacted' AS email, phone FROM customers",
		},
		{
			name:     "mask_preserves_order_by",
			query:    "SELECT id, ssn FROM customers ORDER BY id",
			body:     "ssn AS '***'",
			expected: "SELECT id, '***' AS ssn FROM customers ORDER BY id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dialect.SpliceArtifact(tc.query, []byte(tc.body), store.KindColumnMask, sqlproxy.Claims{})
			require.NoError(t, err)
			require.Equal(t, normalize(tc.expected), normalize(got))
		})
	}
}

func TestSpliceArtifact_RejectUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name  string
		query string
		body  string
		kind  string
	}{
		{
			name:  "join",
			query: "SELECT * FROM a JOIN b ON a.id = b.id",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "inner_join",
			query: "SELECT * FROM a INNER JOIN b ON a.id = b.id",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "subquery_in_from",
			query: "SELECT * FROM (SELECT * FROM x) AS sub",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "cte_with",
			query: "WITH cte AS (SELECT * FROM x) SELECT * FROM cte",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "multi_table_comma",
			query: "SELECT * FROM a, b",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "union",
			query: "SELECT * FROM a UNION SELECT * FROM b",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "insert_dml",
			query: "INSERT INTO orders (id) VALUES (1)",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "update_dml",
			query: "UPDATE orders SET total = 0",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "delete_dml",
			query: "DELETE FROM orders",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dialect.SpliceArtifact(tc.query, []byte(tc.body), tc.kind, sqlproxy.Claims{})
			require.ErrorIs(t, err, sqlproxy.ErrUnsupportedQueryShape)
		})
	}
}

func TestSpliceArtifact_SpliceMismatch(t *testing.T) {
	cases := []struct {
		name  string
		query string
		body  string
	}{
		{
			name:  "select_star_with_column_mask",
			query: "SELECT * FROM customers",
			body:  "ssn AS '***'",
		},
		{
			name:  "column_not_in_projection",
			query: "SELECT id FROM customers",
			body:  "ssn AS '***'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dialect.SpliceArtifact(tc.query, []byte(tc.body), store.KindColumnMask, sqlproxy.Claims{})
			require.ErrorIs(t, err, sqlproxy.ErrSpliceMismatch)
		})
	}
}

func TestSpliceArtifact_MalformedSQL(t *testing.T) {
	cases := []struct {
		name  string
		query string
		body  string
		kind  string
	}{
		{
			name:  "empty_query",
			query: "",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "no_from",
			query: "SELECT id",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "no_table_after_from",
			query: "SELECT * FROM",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
		{
			name:  "missing_select",
			query: "FROM orders WHERE x=1",
			body:  "region = 'us-east'",
			kind:  store.KindRowFilter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dialect.SpliceArtifact(tc.query, []byte(tc.body), tc.kind, sqlproxy.Claims{})
			require.ErrorIs(t, err, sqlproxy.ErrInjectionFailed)
		})
	}
}

func TestSpliceArtifact_UnknownKind(t *testing.T) {
	_, err := dialect.SpliceArtifact("SELECT * FROM t", []byte("body"), "unknown-kind", sqlproxy.Claims{})
	require.ErrorIs(t, err, sqlproxy.ErrInjectionFailed)
}
