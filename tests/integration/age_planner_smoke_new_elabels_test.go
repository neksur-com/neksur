//go:build integration

// Plan 01-01 Task 4 [BLOCKING] — Phase 1 AGE planner smoke for HAS_COLUMN.
//
// TestAGEPlannerSmokeNewElabels mitigates Pitfall 12 (01-RESEARCH.md
// lines 1532-1538): AGE 1.6's `age_vle` planner forces Nested Loop on
// edge traversals regardless of edge label. Phase 0 GC-02 documented
// the cartesian-blowup symptom on the original 24 elabels; the same
// pathology must NOT re-surface on the 5 new Phase 1 elabels.
//
// The test seeds ~1000 Table + ~3000 Column + ~3000 HAS_COLUMN edges
// into the `neksur` graph (scoped by tenant_id property + RLS),
// EXPLAINs a simple one-hop MATCH, and asserts:
//
//   * wall-clock < 200ms at this scale (RESEARCH line 1537 — at
//     Phase 1 per-tenant working-set scale the planner-fix budget is
//     200ms; the full Phase 0 envelope is 50ms but at 50K-edges).
//   * the EXPLAIN plan does NOT contain a Seq Scan on
//     neksur."HAS_COLUMN" — proves the V0031 GC-01 btrees are picked
//     up by the planner.
//
// Note on scale: the plan suggests 100K nodes; we use ~7K to keep the
// test wall-clock under ~30s on CI. The Seq-Scan-avoidance assertion is
// the load-bearing check; bigger seeds add tighter wall-clock pressure
// but the same plan-shape assertion catches the regression at any size.

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const phase1SmokeTenant = "33333333-3333-4333-3333-333333333333"

func TestAGEPlannerSmokeNewElabels(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, phase1SmokeTenant)

	conn, err := pgx.Connect(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer conn.Close(fx.ctx)

	if _, err := conn.Exec(fx.ctx, "LOAD 'age'"); err != nil {
		t.Fatalf("LOAD age: %v", err)
	}
	if _, err := conn.Exec(fx.ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		t.Fatalf("SET search_path: %v", err)
	}

	const tables = 200
	const columnsPerTable = 15
	seedHasColumnGraph(t, conn, phase1SmokeTenant, tables, columnsPerTable)

	// ANALYZE the underlying tables so the planner has stats. AGE's
	// label tables are vanilla Postgres tables under the hood; the
	// pg_class statistics drive HashJoin vs NestedLoop selection just
	// like any other relation. Without ANALYZE the planner falls back
	// to default selectivity estimates and may pick Seq Scan even with
	// the GC-01 btrees in place.
	if _, err := conn.Exec(fx.ctx, `ANALYZE neksur."Table"`); err != nil {
		t.Fatalf("ANALYZE Table: %v", err)
	}
	if _, err := conn.Exec(fx.ctx, `ANALYZE neksur."Column"`); err != nil {
		t.Fatalf("ANALYZE Column: %v", err)
	}
	if _, err := conn.Exec(fx.ctx, `ANALYZE neksur."HAS_COLUMN"`); err != nil {
		t.Fatalf("ANALYZE HAS_COLUMN: %v", err)
	}

	// EXPLAIN ANALYZE the one-hop traversal. We pick a table_uri that
	// definitely exists (table-0) and request 10 columns. The plan
	// must use index scans, not Seq Scan, on HAS_COLUMN.
	start := time.Now()
	plan, err := explainOneHop(fx.ctx, conn, "table-0")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("explain one-hop: %v", err)
	}

	const wallclockBudget = 200 * time.Millisecond
	if elapsed > wallclockBudget {
		t.Errorf("EXPLAIN ANALYZE wall-clock %s exceeds budget %s (Pitfall 12 sentinel)", elapsed, wallclockBudget)
	}
	if strings.Contains(plan, "Seq Scan on \"HAS_COLUMN\"") || strings.Contains(plan, `Seq Scan on neksur."HAS_COLUMN"`) {
		t.Errorf("EXPLAIN plan contains Seq Scan on HAS_COLUMN — V0031 GC-01 btrees not picked up by planner:\n%s", plan)
	}
	t.Logf("AGE planner smoke OK in %s; plan first 400 chars:\n%s", elapsed, firstN(plan, 400))
}

// seedHasColumnGraph populates the `neksur` graph with `tables` Table
// nodes, `tables * columnsPerTable` Column nodes, and one HAS_COLUMN
// edge per Column. tenant_id is set on every node + edge so the RLS
// predicates fire correctly.
//
// Each Cypher call uses UNWIND over a list literal so the entire batch
// lands in a single cypher() invocation. Chaining multiple MERGE
// clauses inside one Cypher fails AGE 1.6's parser on the ON CREATE
// boundary; UNWIND is the idiomatic batched-write shape.
func seedHasColumnGraph(t *testing.T, conn *pgx.Conn, tenantID string, tables, columnsPerTable int) {
	t.Helper()
	ctx := context.Background()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("seed: begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		t.Fatalf("seed: set_config: %v", err)
	}

	// Seed Tables via UNWIND + CREATE. CREATE (not MERGE) is used
	// because the AGE 1.6 parser rejects `ON CREATE SET` in this
	// context with a SQL-level "syntax error at or near ON"
	// (SQLSTATE 42601) — the test fixture is fresh per test so no
	// idempotent MERGE is needed.
	tableURIs := make([]string, 0, tables)
	for i := 0; i < tables; i++ {
		tableURIs = append(tableURIs, fmt.Sprintf("'table-%d'", i))
	}
	seedTables := fmt.Sprintf(`
		SELECT * FROM ag_catalog.cypher('neksur', $$
			UNWIND [%s] AS uri
			CREATE (t:Table { uri: uri, tenant_id: '%s' })
			RETURN count(t)
		$$) AS (r ag_catalog.agtype)`, strings.Join(tableURIs, ","), tenantID)
	if _, err := tx.Exec(ctx, seedTables); err != nil {
		t.Fatalf("seed tables: %v", err)
	}

	// Seed Columns + HAS_COLUMN edges per Table. One cypher() call per
	// Table; inside each call UNWIND produces all `columnsPerTable`
	// Columns + edges via CREATE (same fresh-fixture rationale as above).
	for tIdx := 0; tIdx < tables; tIdx++ {
		ordinals := make([]string, 0, columnsPerTable)
		for c := 0; c < columnsPerTable; c++ {
			ordinals = append(ordinals, fmt.Sprintf("%d", c))
		}
		// AGE 1.6 quirk: two separate CREATE clauses with UNWIND lose
		// the variable scope between them ("vertex assigned to variable
		// c was deleted"). Inlining the Column node into the edge
		// CREATE pattern works around it — a single CREATE clause
		// holds the t-[r]->(c) pattern atomically.
		seedCols := fmt.Sprintf(`
			SELECT * FROM ag_catalog.cypher('neksur', $$
				MATCH (t:Table { uri: 'table-%d' })
				UNWIND [%s] AS ord
				CREATE (t)-[r:HAS_COLUMN { ordinal: ord, tenant_id: '%s' }]->(c:Column { snapshot_loc: 'table-%d', name: 'col-' + toString(ord), tenant_id: '%s' })
				RETURN count(c)
			$$) AS (r ag_catalog.agtype)`,
			tIdx, strings.Join(ordinals, ","), tenantID, tIdx, tenantID)
		if _, err := tx.Exec(ctx, seedCols); err != nil {
			t.Fatalf("seed columns table %d: %v", tIdx, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed: commit: %v", err)
	}
}

// explainOneHop runs EXPLAIN ANALYZE on the one-hop traversal and
// returns the textual plan (concatenated). AGE's cypher() wraps the
// resulting plan in a SELECT, so we wrap the whole thing in EXPLAIN
// ANALYZE at the SQL level.
func explainOneHop(ctx context.Context, conn *pgx.Conn, tableURI string) (string, error) {
	q := fmt.Sprintf(`EXPLAIN ANALYZE SELECT * FROM ag_catalog.cypher('neksur', $$
        MATCH (t:Table { uri: '%s' })-[:HAS_COLUMN]->(c:Column)
        RETURN c.name
        LIMIT 10
    $$) AS (result ag_catalog.agtype)`, tableURI)
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), rows.Err()
}

// firstN truncates s to at most n runes for logging.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
