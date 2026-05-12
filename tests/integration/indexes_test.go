package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRequiredIndexes verifies the D-001.07 property indexes + edge
// timestamp indexes + 2 Postgres functional indexes exist on the
// neksur.* underlying tables. The polyfilled create_property_index*
// functions (Deviation #2) emit indexes with deterministic names
// `idx_<Label>_<Property>` (vlabel) and `idx_<Label>_<Property>_edge`
// (elabel), so we can grep by exact name.
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-knowledge-graph-foundation /
// "All D-001.07 indexes exist".
func TestRequiredIndexes(t *testing.T) {
	// AGE property indexes — 11 from V0020.
	expectedProperty := []struct{ label, prop string }{
		{"Table", "uri"},
		{"Table", "catalog_id"},
		{"Column", "uri"},
		{"Column", "parent_table_uri"},
		{"Snapshot", "snapshot_id"},
		{"Snapshot", "table_uri"},
		{"Snapshot", "committed_at"},
		{"Metric", "name"},
		{"Person", "email"},
		{"Tag", "id"},
		{"Query", "query_id"},
	}
	// Edge timestamp indexes — 3 from V0020.
	expectedEdge := []struct{ label, prop string }{
		{"LINEAGE_OF", "created_at"},
		{"READ", "at"},
		{"WROTE", "at"},
	}
	// Functional indexes — 2 from V0020.
	expectedFunctional := []string{"idx_table_namespace", "idx_snapshot_time"}

	// Pull all neksur.* index names once.
	rows, err := fix.superPool.Query(fix.ctx,
		`SELECT indexname FROM pg_indexes WHERE schemaname = 'neksur'`,
	)
	if err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	defer rows.Close()
	have := map[string]struct{}{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan indexname: %v", err)
		}
		have[n] = struct{}{}
	}

	for _, p := range expectedProperty {
		want := "idx_" + p.label + "_" + p.prop
		if _, ok := have[want]; !ok {
			t.Errorf("D-001.07 property index missing: %s", want)
		}
	}
	for _, e := range expectedEdge {
		want := "idx_" + e.label + "_" + e.prop + "_edge"
		if _, ok := have[want]; !ok {
			t.Errorf("D-001.07 edge timestamp index missing: %s", want)
		}
	}
	for _, n := range expectedFunctional {
		if _, ok := have[n]; !ok {
			t.Errorf("functional index missing: %s", n)
		}
	}
}

// TestPerVlabelTenantAndGinIndexes verifies V0025 created per-vlabel
// tenant btree and GIN-on-properties indexes for all 19 vlabels — the
// BEFORE-load ordering mitigation for AGE issue #1010.
func TestPerVlabelTenantAndGinIndexes(t *testing.T) {
	var tenantCount, ginCount int
	if err := fix.superPool.QueryRow(fix.ctx, `
		SELECT count(*) FROM pg_indexes WHERE schemaname = 'neksur'
		  AND indexname ~ '^idx_[A-Za-z]+_tenant$'
	`).Scan(&tenantCount); err != nil {
		t.Fatalf("count tenant indexes: %v", err)
	}
	if tenantCount != 19 {
		t.Errorf("tenant index count = %d; expected 19", tenantCount)
	}
	if err := fix.superPool.QueryRow(fix.ctx, `
		SELECT count(*) FROM pg_indexes WHERE schemaname = 'neksur'
		  AND indexname ~ '^idx_[A-Za-z]+_props_gin$'
	`).Scan(&ginCount); err != nil {
		t.Fatalf("count GIN indexes: %v", err)
	}
	if ginCount != 19 {
		t.Errorf("GIN index count = %d; expected 19", ginCount)
	}
}

// TestIndexesUsedInExplain is the AGE issue #1010 smoking gun: with
// V0025's BEFORE-load GIN, disabling seqscan forces the planner to
// pick an Index Scan on the freshly-inserted row. If the GIN had been
// created AFTER load (the AGE bug), no row in the existing data would
// be reachable through the index and Postgres would still fall back
// to Seq Scan even with enable_seqscan = off (because there'd be no
// rows to find).
//
// We insert one row inside a tenant context, then run EXPLAIN (FORMAT
// JSON, ANALYZE) on a Cypher MATCH that should use a property index,
// and assert the plan text contains "Index Scan" or a related family
// of node ("Bitmap Index Scan", "Index Only Scan", "Bitmap Heap Scan").
func TestIndexesUsedInExplain(t *testing.T) {
	tid := "test-tenant-explain"

	// Seed one row.
	{
		tx, commit := tenantTxCommit(t, tid)
		props := `{"uri":"iceberg://test/explain/probe","name":"probe","tenant_id":"` + tid + `"}`
		if _, err := tx.Exec(fix.ctx,
			`INSERT INTO neksur."Table" (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
			"281474976710657", props,
		); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		if err := commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}

	// EXPLAIN with seqscan disabled. Single-row scale always biases the
	// planner toward Seq Scan absent enable_seqscan = off; the contract
	// is "is the index usable" not "is it chosen at scale".
	tx, release := tenantTx(t, tid)
	defer release()
	if _, err := tx.Exec(fix.ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := tx.Query(fix.ctx, `
		EXPLAIN (FORMAT JSON, ANALYZE)
		SELECT * FROM cypher('neksur', $$
			MATCH (t:Table {uri: 'iceberg://test/explain/probe'})
			RETURN t
		$$) AS (t ag_catalog.agtype)
	`)
	if err != nil {
		t.Fatalf("EXPLAIN query: %v", err)
	}
	defer rows.Close()

	var dump strings.Builder
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		dump.Write(raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows.Err: %v", err)
	}
	planText := dump.String()

	hasIndexScan := strings.Contains(planText, "Index Scan") ||
		strings.Contains(planText, "Index Only Scan") ||
		strings.Contains(planText, "Bitmap Index Scan") ||
		strings.Contains(planText, "Bitmap Heap Scan")
	if !hasIndexScan {
		head := planText
		if len(head) > 2000 {
			head = head[:2000]
		}
		t.Fatalf("EXPLAIN does not show Index Scan even with enable_seqscan=off — AGE #1010 may be biting. Plan dump (first 2KB): %s", head)
	}
}
