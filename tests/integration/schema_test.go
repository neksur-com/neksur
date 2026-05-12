package integration

import (
	"sort"
	"testing"

	"github.com/neksur-com/neksur/internal/graph"
)

// TestAllLabelsPresent asserts the V0010 migration produced the canonical
// 19 vlabels + 24 elabels (per D-001.05/.06 amended by D-003.06). The
// `name NOT LIKE` filter excludes AGE's auto-created synthetic labels
// `_ag_label_vertex` / `_ag_label_edge` — see Deviation #1 (the Python
// tier discovered the AGE 1.6.0 behaviour during Wave 1 testing).
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-knowledge-graph-foundation /
// "All 19 vlabels + 24 elabels present after migration (per D-003.06)".
func TestAllLabelsPresent(t *testing.T) {
	// Count assertion — the headline contract.
	var vcount, ecount int
	err := fix.superPool.QueryRow(fix.ctx, `
		SELECT count(*) FROM ag_catalog.ag_label
		WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
		  AND kind = 'v'
		  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`).Scan(&vcount)
	if err != nil {
		t.Fatalf("count vlabels: %v", err)
	}
	if vcount != 19 {
		t.Errorf("vlabel count %d != 19 — D-001.05 amended by D-003.06 requires 19", vcount)
	}

	err = fix.superPool.QueryRow(fix.ctx, `
		SELECT count(*) FROM ag_catalog.ag_label
		WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
		  AND kind = 'e'
		  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`).Scan(&ecount)
	if err != nil {
		t.Fatalf("count elabels: %v", err)
	}
	if ecount != 24 {
		t.Errorf("elabel count %d != 24 — D-001.06 amended by D-003.06 requires 24", ecount)
	}

	// Exact-set assertion: every canonical label is present, no drift.
	// `kind` is Postgres type "char" (OID 18) — pgx cannot scan it
	// directly into *string in binary mode, so cast to text in SQL.
	rows, err := fix.superPool.Query(fix.ctx, `
		SELECT name, kind::text FROM ag_catalog.ag_label
		WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
		  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`)
	if err != nil {
		t.Fatalf("query labels: %v", err)
	}
	defer rows.Close()

	vnames := map[string]struct{}{}
	enames := map[string]struct{}{}
	for rows.Next() {
		var name string
		var kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatalf("scan label row: %v", err)
		}
		switch kind {
		case "v":
			vnames[name] = struct{}{}
		case "e":
			enames[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Compare against the Go LabelWhitelist (the application gateway's
	// single source of truth).
	expectedV := map[string]struct{}{}
	for _, n := range graph.NodeLabels {
		expectedV[n] = struct{}{}
	}
	expectedE := map[string]struct{}{}
	for _, n := range graph.MandatoryEdgeLabels {
		expectedE[n] = struct{}{}
	}
	for _, n := range graph.SupplementEdgeLabels {
		expectedE[n] = struct{}{}
	}

	if !setEqual(vnames, expectedV) {
		t.Errorf("vlabel set mismatch.\nExtra in DB: %v\nMissing in DB: %v",
			setDiff(vnames, expectedV), setDiff(expectedV, vnames))
	}
	if !setEqual(enames, expectedE) {
		t.Errorf("elabel set mismatch.\nExtra in DB: %v\nMissing in DB: %v",
			setDiff(enames, expectedE), setDiff(expectedE, enames))
	}
}

// TestWhitelistMatchesCatalog is the drift gate: graph.LabelWhitelist
// must equal the union of the DB's vlabel + elabel names. Any addition
// to the schema MUST be reflected in the whitelist, or the application
// gateway will reject legitimate label-bearing queries.
func TestWhitelistMatchesCatalog(t *testing.T) {
	rows, err := fix.superPool.Query(fix.ctx, `
		SELECT name FROM ag_catalog.ag_label
		WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
		  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`)
	if err != nil {
		t.Fatalf("query labels: %v", err)
	}
	defer rows.Close()

	dbSet := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		dbSet[name] = struct{}{}
	}
	if !setEqual(dbSet, graph.LabelWhitelist) {
		extra := setDiff(graph.LabelWhitelist, dbSet)
		missing := setDiff(dbSet, graph.LabelWhitelist)
		t.Errorf("LabelWhitelist drift.\nIn whitelist not in DB: %v\nIn DB not in whitelist: %v",
			extra, missing)
	}
}

// TestExtensionsPresent asserts V0001 enabled the required extensions.
// pgaudit is conditional (Deviation #5): the base apache/age image does
// not bundle it, so V0001 uses a pg_available_extensions check and emits
// a NOTICE on absence. age + pg_stat_statements MUST be present.
func TestExtensionsPresent(t *testing.T) {
	rows, err := fix.superPool.Query(fix.ctx,
		`SELECT extname FROM pg_extension WHERE extname IN ('age', 'pgaudit', 'pg_stat_statements')`,
	)
	if err != nil {
		t.Fatalf("query extensions: %v", err)
	}
	defer rows.Close()

	got := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = struct{}{}
	}
	if _, ok := got["age"]; !ok {
		t.Error("age extension missing — V0001 failed")
	}
	if _, ok := got["pg_stat_statements"]; !ok {
		t.Error("pg_stat_statements missing — V0001 failed")
	}
	// pgaudit conditional: verify the image advertises it (if so, V0001
	// should have installed it).
	var pgauditAvailable bool
	if err := fix.superPool.QueryRow(fix.ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'pgaudit')`,
	).Scan(&pgauditAvailable); err != nil {
		t.Fatalf("check pgaudit availability: %v", err)
	}
	if pgauditAvailable {
		if _, ok := got["pgaudit"]; !ok {
			t.Error("pgaudit advertised as available but not installed by V0001")
		}
	}
}

// ----- small set helpers -------------------------------------------------

func setEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func setDiff(a, b map[string]struct{}) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
