//go:build integration

// Plan 01-01 Task 4 [BLOCKING] — Phase 1 graph migrations end-to-end.
//
// TestPhase1GraphSchemaAppliedPerTenant verifies V0030-V0032 applied
// correctly through internal/migrate.ApplyTenantGraph:
//
//   1. ag_catalog.ag_label contains the 4 new vlabels + 5 new elabels
//      under the `neksur` graph.
//   2. Total vlabel count = 23 (19 from Phase 0 + 4 from V0030).
//      Total elabel count = 29 (24 from Phase 0 + 5 from V0030).
//   3. <schema>.graph_schema_revisions records versions 0030, 0031,
//      0032 for the provisioned tenant.
//   4. GC-01 carryover btrees on each new elabel's start_id / end_id
//      exist in the neksur namespace (V0031).
//
// PASS exit-0 proves the ApplyTenantGraph runner walks the embedded
// migrations and lands V0030-V0032 cleanly.

package integration

import (
	"fmt"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Phase1NewVlabels lists the 4 vlabels added by V0030.
var Phase1NewVlabels = []string{"Classification", "LifecyclePolicy", "RetentionPolicy", "ScheduledAction"}

// Phase1NewElabels lists the 5 elabels added by V0030.
var Phase1NewElabels = []string{"DETECTED_BY", "HAS_COLUMN", "RETAINS", "SCHEMA_GOVERNS", "WRITE_GOVERNS"}

func TestPhase1GraphSchemaAppliedPerTenant(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	schema := fx.ProvisionTenant(t, phase1TenantA)

	conn, err := pgx.Connect(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer conn.Close(fx.ctx)

	assertNewLabelsPresent(t, conn)
	assertTotalLabelCounts(t, conn)
	assertGraphSchemaRevisions(t, conn, schema)
	assertGC01Indexes(t, conn)
}

// assertNewLabelsPresent verifies the 4 vlabels + 5 elabels added by
// V0030 exist in the ag_catalog catalog under the `neksur` graph.
// Phase 1 graph migrations target the global `neksur` graph (the
// per-tenant graph_schema_revisions table tracks which tenants have
// requested the application — actual labels are global because they
// share label-table backing storage across tenants).
func assertNewLabelsPresent(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	got := readGraphLabels(t, conn, "v")
	for _, want := range Phase1NewVlabels {
		if !contains(got, want) {
			t.Errorf("missing vlabel %q in neksur graph; got %v", want, got)
		}
	}
	got = readGraphLabels(t, conn, "e")
	for _, want := range Phase1NewElabels {
		if !contains(got, want) {
			t.Errorf("missing elabel %q in neksur graph; got %v", want, got)
		}
	}
}

// assertTotalLabelCounts verifies the post-V0030 inventory totals.
// Phase 0 contributes 19 vlabels + 24 elabels; Phase 1 V0030 adds 4 +
// 5 → 23 + 29.
func assertTotalLabelCounts(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	const expectedVcount = 23
	const expectedEcount = 29
	var vcount, ecount int
	if err := conn.QueryRow(t.Context(), `
		SELECT count(*) FROM ag_catalog.ag_label
		 WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
		   AND kind = 'v'
		   AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'`).Scan(&vcount); err != nil {
		t.Fatalf("count vlabels: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `
		SELECT count(*) FROM ag_catalog.ag_label
		 WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
		   AND kind = 'e'
		   AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'`).Scan(&ecount); err != nil {
		t.Fatalf("count elabels: %v", err)
	}
	if vcount != expectedVcount {
		t.Errorf("vlabel count: got %d; expected %d (19 Phase 0 + 4 Phase 1)", vcount, expectedVcount)
	}
	if ecount != expectedEcount {
		t.Errorf("elabel count: got %d; expected %d (24 Phase 0 + 5 Phase 1)", ecount, expectedEcount)
	}
}

// assertGraphSchemaRevisions verifies <schema>.graph_schema_revisions
// records V0030, V0031, V0032 for this tenant.
func assertGraphSchemaRevisions(t *testing.T, conn *pgx.Conn, schema string) {
	t.Helper()
	qSchema := pgx.Identifier{schema}.Sanitize()
	rows, err := conn.Query(t.Context(),
		fmt.Sprintf(`SELECT version FROM %s.graph_schema_revisions ORDER BY version`, qSchema))
	if err != nil {
		t.Fatalf("query graph_schema_revisions: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan revision: %v", err)
		}
		got = append(got, v)
	}
	want := []string{"0030", "0031", "0032"}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("schema %s: missing graph revision %q; got %v", schema, w, got)
		}
	}
}

// assertGC01Indexes verifies each new elabel got the start_id + end_id
// structural btrees (V0031 GC-01 carryover). Reads pg_indexes for the
// `neksur` namespace.
func assertGC01Indexes(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	for _, label := range Phase1NewElabels {
		var count int
		if err := conn.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_indexes
			 WHERE schemaname = 'neksur'
			   AND tablename  = $1
			   AND (indexdef LIKE '%start_id%' OR indexdef LIKE '%end_id%')`,
			label).Scan(&count); err != nil {
			t.Fatalf("query pg_indexes for %s: %v", label, err)
		}
		if count < 2 {
			t.Errorf("elabel %q: expected >=2 start_id/end_id indexes (GC-01 carryover); got %d", label, count)
		}
	}
}

// readGraphLabels returns the sorted list of label names of the given
// kind ('v' or 'e') in the neksur graph, excluding AGE's synthetic
// `_ag_label_*` placeholders.
func readGraphLabels(t *testing.T, conn *pgx.Conn, kind string) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), `
		SELECT name FROM ag_catalog.ag_label
		 WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
		   AND kind = $1
		   AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
		 ORDER BY name`, kind)
	if err != nil {
		t.Fatalf("query ag_label kind=%s: %v", kind, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan label name: %v", err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// contains is a tiny helper used by the assertions above to keep the
// failure messages readable.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
