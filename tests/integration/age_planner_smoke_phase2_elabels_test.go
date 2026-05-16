//go:build integration

// Plan 02-01 Wave-0 BLOCKING — Phase 2 AGE planner smoke for the 10 new
// elabels.
//
// TestAGEPlannerSmokePhase2Elabels carries Phase 1's GC-01 closure into
// Phase 2: AGE 1.6's `age_vle` planner forces Nested Loop on edge
// traversals regardless of edge label. The V0041 structural btrees on
// each new elabel's start_id/end_id must keep the planner off Seq Scan
// on the 10 new edge types.
//
// The test seeds a small CompiledPolicy -> Engine subgraph via the
// GOVERNED_BY edge (the canonical D-2.04 traversal pattern), EXPLAINs
// the one-hop MATCH, and asserts no Seq Scan on GOVERNED_BY. We pick
// GOVERNED_BY as the representative because it's the most-hit edge in
// the Plan 02-04 read-path "find the active CompiledPolicy for engine X
// on table T" query.
//
// Scale is small (50 CompiledPolicy nodes + 5 Engine nodes + 50 edges)
// — sufficient to surface a Seq-Scan regression without paying full
// 200ms wall-clock budget. The Seq-Scan-avoidance assertion is the
// load-bearing check; a bigger seed adds tighter wall-clock pressure
// but the same plan-shape assertion catches the regression at any
// size.

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const phase2SmokeTenant = "55555555-5555-5555-5555-555555555555"

func TestAGEPlannerSmokePhase2Elabels(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, phase2SmokeTenant)

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

	const compiledPolicies = 50
	const engines = 5
	seedCompiledPolicyEngineGraph(t, conn, phase2SmokeTenant, compiledPolicies, engines)

	for _, label := range []string{"CompiledPolicy", "Engine", "GOVERNED_BY"} {
		stmt := fmt.Sprintf(`ANALYZE neksur.%q`, label)
		if _, err := conn.Exec(fx.ctx, stmt); err != nil {
			t.Fatalf("ANALYZE %s: %v", label, err)
		}
	}

	plan, err := explainCompiledPolicyHop(fx.ctx, conn, "engine-0")
	if err != nil {
		t.Fatalf("explain one-hop: %v", err)
	}
	if strings.Contains(plan, `Seq Scan on "GOVERNED_BY"`) || strings.Contains(plan, `Seq Scan on neksur."GOVERNED_BY"`) {
		t.Errorf("EXPLAIN plan contains Seq Scan on GOVERNED_BY — V0041 GC-01 btrees not picked up by planner:\n%s", plan)
	}
	t.Logf("Phase 2 GC-01 carryover OK; plan first 400 chars:\n%s", firstNPhase2(plan, 400))
}

// seedCompiledPolicyEngineGraph populates the `neksur` graph with
// `engines` Engine nodes, `compiledPolicies` CompiledPolicy nodes, and
// one GOVERNED_BY edge from each CompiledPolicy to a round-robin
// Engine. tenant_id is set on every node + edge so the V0042 RLS
// predicates fire correctly.
//
// Uses CREATE (not MERGE) per Phase 1's seedHasColumnGraph rationale:
// AGE 1.6 rejects ON CREATE SET in this position and the fixture is
// fresh per test.
func seedCompiledPolicyEngineGraph(t *testing.T, conn *pgx.Conn, tenantID string, compiledPolicies, engines int) {
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

	// Seed Engine nodes.
	engineNames := make([]string, 0, engines)
	for i := 0; i < engines; i++ {
		engineNames = append(engineNames, fmt.Sprintf("'engine-%d'", i))
	}
	seedEngines := fmt.Sprintf(`
		SELECT * FROM ag_catalog.cypher('neksur', $$
			UNWIND [%s] AS name
			CREATE (e:Engine { name: name, tenant_id: '%s' })
			RETURN count(e)
		$$) AS (r ag_catalog.agtype)`, strings.Join(engineNames, ","), tenantID)
	if _, err := tx.Exec(ctx, seedEngines); err != nil {
		t.Fatalf("seed engines: %v", err)
	}

	// Seed CompiledPolicy + GOVERNED_BY edges (one per CompiledPolicy
	// round-robin to Engines). One cypher() call per CompiledPolicy
	// inlining the edge CREATE pattern (same AGE 1.6 workaround as
	// Phase 1 seedHasColumnGraph).
	for i := 0; i < compiledPolicies; i++ {
		engineName := fmt.Sprintf("engine-%d", i%engines)
		seedOne := fmt.Sprintf(`
			SELECT * FROM ag_catalog.cypher('neksur', $$
				MATCH (e:Engine { name: '%s' })
				CREATE (cp:CompiledPolicy {
					source_policy_id: 'p-%d',
					engine_kind: 'trino',
					status: 'active',
					tenant_id: '%s'
				})-[r:GOVERNED_BY { tenant_id: '%s' }]->(e)
				RETURN count(cp)
			$$) AS (r ag_catalog.agtype)`,
			engineName, i, tenantID, tenantID)
		if _, err := tx.Exec(ctx, seedOne); err != nil {
			t.Fatalf("seed compiled-policy %d: %v", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed: commit tx: %v", err)
	}
}

// explainCompiledPolicyHop runs EXPLAIN ANALYZE on the one-hop
// "find all CompiledPolicy attached to a given Engine via GOVERNED_BY"
// query — the canonical D-2.04 read-path traversal. Returns the
// concatenated textual plan.
func explainCompiledPolicyHop(ctx context.Context, conn *pgx.Conn, engineName string) (string, error) {
	q := fmt.Sprintf(`EXPLAIN ANALYZE SELECT * FROM ag_catalog.cypher('neksur', $$
        MATCH (cp:CompiledPolicy)-[:GOVERNED_BY]->(e:Engine { name: '%s' })
        RETURN cp.source_policy_id
        LIMIT 10
    $$) AS (result ag_catalog.agtype)`, engineName)
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

// firstNPhase2 truncates s to at most n runes for logging. (Local
// helper to avoid colliding with Phase 1's firstN — separate file,
// separate scope.)
func firstNPhase2(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
