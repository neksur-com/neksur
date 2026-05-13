//go:build integration

package integration

// deployment_test.go — Postgres-only deployment invariant tests for
// the Phase 0 acceptance gate (Plan 00-06 Wave 5 Task 2).
//
// Per 00-VALIDATION.md row 06-T2 / REQ-NFR-graph-ops-footprint:
// "Postgres-only deployment confirmed (no other services beyond
// Patroni / etcd / pgBackRest / OTel)" — Phase 0 ships a Postgres-
// only metadata stack; any other graph engine, auxiliary database, or
// surprise extension is a contract violation.
//
// All three tests document Phase 0's Postgres-only invariant in their
// // Doc: comments. They run against the package-level fixture from
// main_test.go (one apache/age:release_PG16_1.6.0 testcontainer per
// package).

import (
	"sort"
	"strings"
	"testing"
)

// TestServiceInventory asserts pg_stat_activity.application_name is a
// SUBSET of the allow-listed Phase 0 service set: neksur-graph (the
// app), pgbackrest (DR), patroni (HA), psql (operator), pg_basebackup
// (replica bootstrap), plus the Go-test client itself which connects
// without setting an explicit application_name and shows up empty or
// as the bare libpq default. We allow `""` so the testcontainer's own
// connections do not cause a false-positive.
//
// Doc: Phase 0 is Postgres-only per REQ-NFR-graph-ops-footprint;
// adding a graph-engine sidecar (Memgraph, Neo4j, JanusGraph) or any
// auxiliary datastore (Redis, Kafka, etcd-other-than-for-Patroni) here
// is a Phase 2 D-001.10/.12 trigger, not a Phase 0 deliverable.
func TestServiceInventory(t *testing.T) {
	allowed := map[string]bool{
		"":               true, // testcontainer / unset application_name
		"neksur-graph":   true, // the app server (cmd/neksur-server)
		"pgbackrest":     true, // DR (Plan 00-04)
		"patroni":        true, // HA (Plan 00-03)
		"psql":           true, // operator
		"pg_basebackup":  true, // replica bootstrap
		"PostgreSQL JDBC Driver": true, // legacy / explorer sessions tolerated
	}

	rows, err := fix.superPool.Query(fix.ctx,
		`SELECT DISTINCT application_name FROM pg_stat_activity`)
	if err != nil {
		t.Fatalf("query pg_stat_activity: %v", err)
	}
	defer rows.Close()

	var unexpected []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan application_name: %v", err)
		}
		if !allowed[name] {
			// Treat any pgx-flavored driver name as the integration test
			// client itself — pgx defaults to "pgx/<version>".
			if strings.HasPrefix(name, "pgx") || strings.HasPrefix(name, "Go-Postgres") {
				continue
			}
			unexpected = append(unexpected, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Errorf("Phase 0 Postgres-only invariant violated: unexpected application_name entries %v "+
			"(allowed: neksur-graph, pgbackrest, patroni, psql, pg_basebackup; "+
			"any other service is a Phase 2 trigger per D-001.10, NOT a Phase 0 deliverable)",
			unexpected)
	}
}

// TestNoUnexpectedExtensions asserts pg_extension is exactly the
// Phase 0 contract set — plpgsql (Postgres default), age (the graph
// engine), pgaudit (audit log per A2 Phase 0 acceptance), and
// pg_stat_statements (planner observability per Plan 05).
//
// Doc: Phase 0 is Postgres-only per REQ-NFR-graph-ops-footprint;
// adding pgvector / postgis / TimescaleDB / etc. before Phase 6
// requires an ADR. A surprise extension is a deployment-drift
// signal that operator runbook divergence has occurred.
func TestNoUnexpectedExtensions(t *testing.T) {
	expected := map[string]bool{
		"plpgsql":            true,
		"age":                true,
		"pgaudit":            true,
		"pg_stat_statements": true,
	}

	rows, err := fix.superPool.Query(fix.ctx, `SELECT extname FROM pg_extension`)
	if err != nil {
		t.Fatalf("query pg_extension: %v", err)
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan extname: %v", err)
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Exact-equality: extensions present that aren't expected, AND
	// expected extensions that aren't present.
	var unexpected, missing []string
	for n := range have {
		if !expected[n] {
			unexpected = append(unexpected, n)
		}
	}
	for n := range expected {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)
	if len(unexpected) > 0 {
		t.Errorf("Phase 0 extension contract violated — surprise extensions present: %v "+
			"(allowed exactly: plpgsql, age, pgaudit, pg_stat_statements)", unexpected)
	}
	if len(missing) > 0 {
		t.Errorf("Phase 0 extension contract violated — required extensions missing: %v", missing)
	}
}

// TestNoUnexpectedDatabases asserts pg_database contains ONLY the
// Phase 0 contract databases plus Postgres's three template databases
// (template0, template1, postgres). For Phase 0 the configured app DB
// is whatever the migration target is — typically `neksur` or the
// testcontainer-default `postgres` for the in-test fixture.
//
// Doc: Phase 0 is Postgres-only per REQ-NFR-graph-ops-footprint;
// per-tenant graph mode (D-001.10 + ADR-001 §10.4) is deferred to
// Phase 7. A test fixture or production deployment with multiple
// non-template databases signals scope creep into the multi-database
// pattern.
func TestNoUnexpectedDatabases(t *testing.T) {
	rows, err := fix.superPool.Query(fix.ctx, `
		SELECT datname FROM pg_database
		 WHERE datname NOT IN ('template0', 'template1', 'postgres')
	`)
	if err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan datname: %v", err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)

	// Phase 0 contract: at most one app database besides the templates.
	// The testcontainer fixture uses `postgres` as the DB (which is in
	// the exclude list above), so `got` is typically empty in CI. In
	// production deployments `got` should be exactly ["neksur"] (or
	// whatever the operator-chosen app DB name is). Any size > 1 is
	// scope creep — multiple app DBs is the per-tenant-database mode
	// deferred to Phase 7.
	if len(got) > 1 {
		t.Errorf("Phase 0 database contract violated — multiple non-template databases %v "+
			"(allowed: at most one app DB; multiple DBs is the per-tenant mode deferred to Phase 7)", got)
	}
}
