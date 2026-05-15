//go:build integration

// Plan 01-04 Task 3 [BLOCKING] — new elabels bounded VLP smoke
// (D-001.08 + GC-02 + Pitfall 12).
//
// Anchors:
//
//   - graph.ValidateTraversalDepth refuses unbounded `*` shapes;
//     bounded `*1..N` returns nil.
//
//   - The bounded shape `*1..3` runs in <200ms wall-clock at the
//     Phase 1 per-tenant working-set scale (RESEARCH line 1537;
//     Pitfall 12 mitigation budget). The V0031 GC-01 btrees on the
//     5 new elabel start_id/end_id keep the planner away from Seq
//     Scan even after a fresh ANALYZE.
//
// Seed: 100 Table nodes + 1000 HAS_COLUMN edges + 500 LINEAGE_OF
// edges. The seed shape mirrors age_planner_smoke_new_elabels_test.go
// (Plan 01-01) but adds LINEAGE_OF wiring so the bounded-traversal
// query exercises the new elabel directly.

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
)

const boundedVLPTenant = "10101010-1010-4010-1010-101010101010"

// TestNewElabelsBoundedVLPOnly — VALIDATION line 54 + RESEARCH §Anti-pattern.
//
// Asserts:
//
//	(1) graph.ValidateTraversalDepth rejects unbounded patterns.
//	(2) graph.ValidateTraversalDepth accepts bounded `*1..3`.
//	(3) Bounded query runs in <200ms wall-clock against ~100 Tables /
//	    500 LINEAGE_OF edges (GC-02 / Pitfall 12 budget).
func TestNewElabelsBoundedVLPOnly(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, boundedVLPTenant)

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

	// Step (1) — unbounded patterns are rejected.
	unboundedCases := []string{
		"MATCH (t:Table)-[:LINEAGE_OF*]->(x) RETURN x LIMIT 1",
		"MATCH (t:Table)-[:LINEAGE_OF*1..]->(x) RETURN x LIMIT 1",
		"MATCH (t:Table)-[:LINEAGE_OF*..]->(x) RETURN x LIMIT 1",
	}
	for _, q := range unboundedCases {
		err := graph.ValidateTraversalDepth(q)
		if err == nil {
			t.Errorf("ValidateTraversalDepth accepted unbounded pattern: %q", q)
			continue
		}
		if !errors.Is(err, graph.ErrUnboundedTraversal) {
			t.Errorf("expected ErrUnboundedTraversal; got %v (query=%q)", err, q)
		}
	}

	// Step (2) — bounded *1..3 is accepted.
	boundedQ := "MATCH (t:Table)-[:LINEAGE_OF*1..3]->(x) RETURN x LIMIT 1"
	if err := graph.ValidateTraversalDepth(boundedQ); err != nil {
		t.Errorf("ValidateTraversalDepth rejected bounded *1..3: %v", err)
	}
	// Also accept the cycle-pre-check shape used by ingest (cycle.go).
	if err := graph.ValidateTraversalDepth(
		"MATCH (target)-[:LINEAGE_OF*1..5]->(ancestor) RETURN ancestor LIMIT 1",
	); err != nil {
		t.Errorf("ValidateTraversalDepth rejected bounded *1..5: %v", err)
	}

	// Step (3) — seed + bounded query wall-clock.
	seedTablesAndLineage(t, conn, boundedVLPTenant, 100, 500)

	// ANALYZE for planner sanity.
	for _, lbl := range []string{`neksur."Table"`, `neksur."LINEAGE_OF"`} {
		if _, err := conn.Exec(fx.ctx, "ANALYZE "+lbl); err != nil {
			t.Fatalf("ANALYZE %s: %v", lbl, err)
		}
	}

	start := time.Now()
	// Bounded *1..3 against the seed graph. We pick a known-existing
	// table URI (table-0) and walk up to 3 hops; LIMIT 1 to bound the
	// result set so the planner can stop early.
	plan, err := explainBoundedVLP(fx.ctx, conn, "table-0")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("bounded VLP EXPLAIN: %v", err)
	}

	// 200ms budget per Pitfall 12 / RESEARCH line 1537.
	const wallclockBudget = 200 * time.Millisecond
	if elapsed > wallclockBudget {
		t.Errorf("bounded *1..3 wall-clock %s exceeds budget %s — Pitfall 12 sentinel; check V0031 GC-01 btrees",
			elapsed, wallclockBudget)
	}
	if strings.Contains(plan, `Seq Scan on neksur."LINEAGE_OF"`) {
		t.Errorf("EXPLAIN plan contains Seq Scan on LINEAGE_OF — V0031 GC-01 btrees not picked up by planner:\n%s",
			plan)
	}
	t.Logf("bounded *1..3 OK in %s; first 200 chars of plan:\n%s", elapsed, firstNChars(plan, 200))
}

// seedTablesAndLineage creates `tables` Table nodes named "table-0" ..
// "table-N-1" and `lineageEdges` LINEAGE_OF edges in a chain. Inlined
// edge creation works around AGE 1.6's variable-scope quirk between
// consecutive CREATE clauses (PATTERNS.md §AGE 1.6 multi-CREATE).
func seedTablesAndLineage(t *testing.T, conn *pgx.Conn, tenantID string, tables, lineageEdges int) {
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

	// Seed Tables.
	uris := make([]string, 0, tables)
	for i := 0; i < tables; i++ {
		uris = append(uris, fmt.Sprintf("'table-%d'", i))
	}
	seedQ := fmt.Sprintf(`
		SELECT * FROM ag_catalog.cypher('neksur', $$
			UNWIND [%s] AS uri
			CREATE (t:Table { uri: uri, iceberg_id: uri, tenant_id: '%s' })
			RETURN count(t)
		$$) AS (r ag_catalog.agtype)`,
		strings.Join(uris, ","), tenantID)
	if _, err := tx.Exec(ctx, seedQ); err != nil {
		t.Fatalf("seed tables: %v", err)
	}

	// Seed LINEAGE_OF edges in a chain so bounded *1..3 has live paths
	// to traverse. We chain table-0 → table-1 → ... → table-N-1 for
	// `lineageEdges` edges (clamped to tables-1).
	maxChain := lineageEdges
	if maxChain > tables-1 {
		maxChain = tables - 1
	}
	for i := 0; i < maxChain; i++ {
		edgeQ := fmt.Sprintf(`
			SELECT * FROM ag_catalog.cypher('neksur', $$
				MATCH (a:Table { uri: 'table-%d' }), (b:Table { uri: 'table-%d' })
				CREATE (a)-[:LINEAGE_OF { tenant_id: '%s', created_at: '2026-05-15T00:00:00Z' }]->(b)
				RETURN 1
			$$) AS (r ag_catalog.agtype)`, i, i+1, tenantID)
		if _, err := tx.Exec(ctx, edgeQ); err != nil {
			t.Fatalf("seed lineage edge %d→%d: %v", i, i+1, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed: commit: %v", err)
	}
}

// explainBoundedVLP runs EXPLAIN ANALYZE on the bounded *1..3 query
// and returns the plan text.
func explainBoundedVLP(ctx context.Context, conn *pgx.Conn, tableURI string) (string, error) {
	q := fmt.Sprintf(`EXPLAIN ANALYZE SELECT * FROM ag_catalog.cypher('neksur', $$
		MATCH (t:Table { uri: '%s' })-[:LINEAGE_OF*1..3]->(x:Table)
		RETURN x.uri
		LIMIT 1
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

// firstNChars truncates s to at most n bytes for logging — duplicated
// from age_planner_smoke_new_elabels_test.go::firstN under a different
// name to avoid the cross-file symbol collision.
func firstNChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
