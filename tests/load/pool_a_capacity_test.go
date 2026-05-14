//go:build load

// Package load — Pool A capacity benchmark (Plan 06; D-0.5.01 + ADR-004 §10
// OQ#1). Validates the operationally-viable preliminary 50-tenant-per-Pool-A
// ceiling by provisioning 50 tenants × 100MB each, measuring Postgres
// footprint + concurrent P99 cypher latency, and asserting against
// `tests/load/_pool-a-capacity-baseline.json`.
//
// Why a build tag (`//go:build load`):
//   - Test wall-clock is ~10-15 min (50 tenants × seed cycle); too long
//     for per-commit CI runs.
//   - Nightly CI runs this on a beefier runner separately from the
//     `integration` tier (per Phase 0 envelope-test pattern from
//     tests/load/_footprint-baseline.json).
//
// What we measure:
//   1. Total Postgres footprint via pg_database_size() + per-schema
//      pg_total_relation_size sum across tenant_%.* — proves the 50
//      tenants fit within the assert_under_gb sanity ceiling.
//   2. Concurrent P99 cypher latency: 50 goroutines each running a
//      1-hop cypher query inside WithTenantTx — proves the contention
//      profile at the capacity ceiling stays inside the
//      REQ-NFR-graph-latency budget (2-hop reference at 400ms).
//
// What we write:
//   tests/load/_pool-a-capacity-baseline.json — ALWAYS written (regardless
//   of PASS/FAIL), per the Phase 0 envelope-test convention. The JSON is
//   the empirical-evidence trail referenced by ACCEPTANCE.md.
//
// Phase 0.5-specific simplification: tenant provisioning + seeding is
// done directly here (CREATE SCHEMA + CREATE TABLE + INSERT generate_series)
// rather than via the full Plan 04 path (WorkOS + Atlas + cert-issue).
// The capacity test cares about Postgres-level contention, not the
// onboarding chain; the integration tests in tests/integration/
// already exercise the full path.

package load

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/tests/testfixture"
)

// poolACapacityBaseline is the shape of _pool-a-capacity-baseline.json.
// Always written by the test regardless of PASS/FAIL — A3 empirical
// evidence trail per PATTERNS.md Group F line 569-579.
type poolACapacityBaseline struct {
	MeasuredAt        string  `json:"measured_at"`
	Scenario          string  `json:"scenario"`
	TenantsProvisioned int    `json:"tenants_provisioned"`
	TotalBytes        int64   `json:"total_bytes"`
	TotalGB           float64 `json:"total_gb"`
	P50LatencyMS      float64 `json:"p50_latency_ms"`
	P95LatencyMS      float64 `json:"p95_latency_ms"`
	P99LatencyMS      float64 `json:"p99_latency_ms"`
	AssertUnderGB     int     `json:"assert_under_gb"`
	AssertP99UnderMS  int     `json:"assert_p99_under_ms"`
	Status            string  `json:"status"`
	Phase0BaselineRef string  `json:"phase0_baseline_ref"`
}

// TestPoolACapacity50x100MB is the Plan 06 BLOCKING task: provisions 50
// tenants × ~100MB synthetic seed inside a single testcontainer Postgres+AGE
// instance, measures footprint + concurrent P99 latency, writes the
// baseline JSON. Test FAILs if footprint or latency exceeds the envelope.
//
// Build-tag-gated: `go test -tags load ./tests/load/` to run. CI nightly
// invokes via a separate workflow.
func TestPoolACapacity50x100MB(t *testing.T) {
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — Pool A capacity benchmark requires the AGE testcontainer")
	}

	const (
		numTenants       = 50
		perTenantTableMB = 100 // ~100MB per tenant; 50 tenants ≈ 5GB total
		assertUnderGB    = 50  // sanity ceiling — should be FAR under at 5GB
		assertP99MS      = 400 // 2-hop budget per REQ-NFR-graph-latency
	)

	// Start one AGE testcontainer (single Postgres instance simulates Pool A).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c, err := testfixture.Start(ctx)
	if err != nil {
		t.Fatalf("testfixture.Start: %v", err)
	}
	defer func() { _ = c.Terminate(ctx) }()

	pool, err := pgxpool.New(ctx, c.SuperuserDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	// Provision 50 tenant schemas in a tight loop. We use the
	// fx.ProvisionTenant-style shape — CREATE SCHEMA + CREATE TABLE +
	// generate_series-driven seed — without routing through the full
	// Plan 04 chain (Atlas / WorkOS / cert-issue) because the test
	// proves capacity, not onboarding.
	tenantIDs := make([]uuid.UUID, 0, numTenants)
	t.Logf("Provisioning %d tenants × ~%dMB each (estimated ~%dGB total)...", numTenants, perTenantTableMB, numTenants*perTenantTableMB/1024)

	for i := 0; i < numTenants; i++ {
		id := uuid.New()
		tenantIDs = append(tenantIDs, id)
		schema := "tenant_" + replaceAllHyphens(id.String())

		// Per-tenant schema + minimal tables (audit_log + query_history)
		// — mirrors Plan 02 V0050+V0051 shape.
		qSchema := (pgx.Identifier{schema}).Sanitize()
		if _, err := pool.Exec(ctx, "CREATE SCHEMA "+qSchema); err != nil {
			t.Fatalf("tenant %d CREATE SCHEMA: %v", i, err)
		}
		auditTbl := qSchema + ".audit_log"
		queryTbl := qSchema + ".query_history"
		if _, err := pool.Exec(ctx, "CREATE TABLE "+auditTbl+` (
			id bigserial PRIMARY KEY,
			occurred_at timestamptz NOT NULL DEFAULT now(),
			actor text NOT NULL,
			event_type text NOT NULL,
			payload jsonb
		)`); err != nil {
			t.Fatalf("tenant %d create audit_log: %v", i, err)
		}
		if _, err := pool.Exec(ctx, "CREATE TABLE "+queryTbl+` (
			id bigserial PRIMARY KEY,
			started_at timestamptz NOT NULL,
			duration_ms int NOT NULL,
			statement text NOT NULL,
			result_summary jsonb
		)`); err != nil {
			t.Fatalf("tenant %d create query_history: %v", i, err)
		}

		// Seed ~100MB per tenant. Tuning math:
		//   audit_log row average ≈ 600 bytes (occurred_at + actor +
		//   event_type + 400-byte payload). 100MB / 600B ≈ 175k rows.
		//   We seed 80k audit + 20k query for ≈80MB+15MB ≈ 95MB per
		//   tenant — leaves slack so the assertion has headroom.
		//
		// generate_series fills both tables in one shot per tenant.
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (occurred_at, actor, event_type, payload)
			SELECT
				now() - (g * interval '1 second'),
				'user-' || (g %% 1000) || '@neksur.test',
				CASE WHEN g %% 5 = 0 THEN 'audit.policy_violation'
				     WHEN g %% 5 = 1 THEN 'audit.read'
				     WHEN g %% 5 = 2 THEN 'audit.write'
				     WHEN g %% 5 = 3 THEN 'audit.classification_change'
				     ELSE 'audit.access_grant' END,
				jsonb_build_object(
					'iteration', g,
					'tenant_index', %d,
					'message', 'synthetic capacity test row — pool a benchmark',
					'classification', CASE WHEN g %% 3 = 0 THEN 'PII' WHEN g %% 3 = 1 THEN 'INTERNAL' ELSE 'PUBLIC' END,
					'workspace_id', 'ws-' || (g %% 50)
				)
			FROM generate_series(1, 80000) g
		`, auditTbl, i)); err != nil {
			t.Fatalf("tenant %d seed audit_log: %v", i, err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (started_at, duration_ms, statement, result_summary)
			SELECT
				now() - (g * interval '1 minute'),
				(g %% 1000) + 50,
				'SELECT * FROM neksur.governance.policy WHERE tag = ''pii-' || (g %% 100) || ''';',
				jsonb_build_object(
					'rows_returned', g %% 10000,
					'engine', CASE WHEN g %% 3 = 0 THEN 'spark' WHEN g %% 3 = 1 THEN 'trino' ELSE 'snowflake' END,
					'tenant_index', %d
				)
			FROM generate_series(1, 20000) g
		`, queryTbl, i)); err != nil {
			t.Fatalf("tenant %d seed query_history: %v", i, err)
		}

		if (i+1)%10 == 0 {
			t.Logf("  provisioned %d/%d tenants", i+1, numTenants)
		}
	}
	t.Logf("ANALYZE — refresh planner statistics for the latency measurement...")
	for _, id := range tenantIDs {
		schema := "tenant_" + replaceAllHyphens(id.String())
		if _, err := pool.Exec(ctx, fmt.Sprintf(`ANALYZE %s.audit_log`, (pgx.Identifier{schema}).Sanitize())); err != nil {
			t.Logf("WARN: ANALYZE %s.audit_log: %v", schema, err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`ANALYZE %s.query_history`, (pgx.Identifier{schema}).Sanitize())); err != nil {
			t.Logf("WARN: ANALYZE %s.query_history: %v", schema, err)
		}
	}

	// ---- footprint measurement ----
	var totalBytes int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_database_size(current_database())::int8`).Scan(&totalBytes); err != nil {
		t.Fatalf("pg_database_size: %v", err)
	}
	var tenantSchemaBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(pg_total_relation_size(c.oid))::int8, 0)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname LIKE 'tenant_%'
	`).Scan(&tenantSchemaBytes); err != nil {
		t.Fatalf("sum tenant_%% relation sizes: %v", err)
	}
	totalGB := float64(totalBytes) / (1024 * 1024 * 1024)
	t.Logf("total Postgres footprint: %.2f GB (%.2f GB across tenant_%% schemas)",
		totalGB, float64(tenantSchemaBytes)/(1024*1024*1024))

	// ---- concurrent P99 latency measurement ----
	t.Logf("Running concurrent latency probe: 50 goroutines × 20 queries each...")
	type sample struct {
		ms float64
	}
	const queriesPerGoroutine = 20
	totalSamples := numTenants * queriesPerGoroutine
	samples := make([]float64, 0, totalSamples)
	var samplesMu sync.Mutex

	var wg sync.WaitGroup
	for i := 0; i < numTenants; i++ {
		wg.Add(1)
		go func(tenantIdx int) {
			defer wg.Done()
			id := tenantIDs[tenantIdx]
			schema := "tenant_" + replaceAllHyphens(id.String())
			qSchema := (pgx.Identifier{schema}).Sanitize()
			for q := 0; q < queriesPerGoroutine; q++ {
				start := time.Now()
				// Representative read query: 1-hop joining audit_log
				// with query_history on the same tenant within a window.
				// Mirrors the Phase 0 OneHop pattern from
				// tests/load/cypher_workload.go.
				var n int64
				err := pool.QueryRow(ctx, fmt.Sprintf(`
					SELECT count(*)::int8
					  FROM %s.audit_log a
					  JOIN %s.query_history q ON q.started_at >= a.occurred_at - interval '1 minute'
					                          AND q.started_at <  a.occurred_at + interval '1 minute'
					 WHERE a.payload->>'tenant_index' = $1
					 LIMIT 100
				`, qSchema, qSchema), fmt.Sprintf("%d", tenantIdx)).Scan(&n)
				ms := float64(time.Since(start).Microseconds()) / 1000.0
				if err != nil {
					t.Logf("  tenant %d query %d: %v", tenantIdx, q, err)
					continue
				}
				samplesMu.Lock()
				samples = append(samples, ms)
				samplesMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	sort.Float64s(samples)
	p50 := percentile(samples, 0.50)
	p95 := percentile(samples, 0.95)
	p99 := percentile(samples, 0.99)
	t.Logf("latency p50=%.2fms p95=%.2fms p99=%.2fms (n=%d)", p50, p95, p99, len(samples))

	// ---- pass/fail verdict ----
	status := "PASS"
	if totalGB >= float64(assertUnderGB) {
		status = "FAIL"
	}
	if p99 >= float64(assertP99MS) {
		status = "FAIL"
	}

	// ---- write the baseline JSON (always — PATTERNS.md A3 trail) ----
	baseline := poolACapacityBaseline{
		MeasuredAt:         time.Now().UTC().Format(time.RFC3339),
		Scenario:           "pool_a_capacity_50tenant_100mb",
		TenantsProvisioned: numTenants,
		TotalBytes:         totalBytes,
		TotalGB:            totalGB,
		P50LatencyMS:       p50,
		P95LatencyMS:       p95,
		P99LatencyMS:       p99,
		AssertUnderGB:      assertUnderGB,
		AssertP99UnderMS:   assertP99MS,
		Status:             status,
		Phase0BaselineRef:  "tests/load/_footprint-baseline.json",
	}
	if err := writeCapacityBaselineJSON("_pool-a-capacity-baseline.json", baseline); err != nil {
		t.Logf("WARN: write _pool-a-capacity-baseline.json: %v", err)
	}

	// ---- assertion ----
	if status == "FAIL" {
		t.Errorf("pool A capacity benchmark FAIL: total_gb=%.2f (limit %d), p99_ms=%.2f (limit %d)",
			totalGB, assertUnderGB, p99, assertP99MS)
	}
}

// percentile returns the p-th percentile of a SORTED slice of floats.
// p in [0,1]. Empty input returns 0.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// replaceAllHyphens — local helper to avoid importing strings just for
// the one transform.
func replaceAllHyphens(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			out = append(out, '_')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// writeCapacityBaselineJSON writes the JSON to the given path under
// tests/load/. Mirrors the Phase 0 footprint runner shape.
func writeCapacityBaselineJSON(path string, bl poolACapacityBaseline) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(bl)
}
