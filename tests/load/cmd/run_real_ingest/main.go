// Command run_real_ingest is the Phase 1 bulk-backfill envelope runner.
// Per ROADMAP §Phase 1 success-criterion §4 / REQ-iceberg-rest-adapter-model:
// "10M-node-scale Iceberg metadata completes via COPY-then-Cypher-transform
// within the Phase 0 latency budgets, with all required property and edge
// timestamp indexes hot."
//
// Extends Phase 0 Plan 00-06's `cmd/seed/main.go` from synthetic-only data
// to REAL Polaris metadata pulls (the COPY-then-Cypher-transform algorithm
// is shared via `tests/load/real_ingest.go`). Always emits the JSON
// baseline artifact (PASS or FAIL) so SREs can dashboard trends; matches
// the cmd/footprint pattern.
//
// Usage:
//
//	go run ./tests/load/cmd/run_real_ingest \
//	    -dsn "$DATABASE_URL" \
//	    -tenant "<uuid>" \
//	    -polaris-endpoint "https://polaris.local/api/catalog" \
//	    -polaris-client-id test-admin -polaris-client-secret test-secret \
//	    -assert-completes-under 30m \
//	    -baseline-out tests/load/_real-ingest-baseline.json
//
// Required environment / flags:
//
//	-dsn            (or DATABASE_URL env) — libpq DSN to the Phase 0.5
//	                Pool A Postgres + AGE cluster.
//	-tenant         — canonical 36-char UUID for the tenant whose graph
//	                receives the ingest.
//	-polaris-endpoint, -polaris-client-id, -polaris-client-secret,
//	-polaris-warehouse — Polaris catalog wire props.
//
// Optional flags (Phase 1 envelope defaults per ROADMAP §Phase 1):
//
//	-target-tables 100000  -target-columns 5000000
//	-target-snapshots 4000000  -target-lineage 30000000
//	-cypher-batch-size 1000  -copy-batch-size 50000
//	-assert-completes-under 30m
//	-baseline-out tests/load/_real-ingest-baseline.json
//
// Exit codes:
//
//	0 — TotalDuration ≤ -assert-completes-under AND no errors.
//	1 — assertion miss OR runtime error (the baseline JSON is still
//	    written for triage).

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg/polaris"
	"github.com/neksur-com/neksur/tests/load"
)

// baselineEnvelope is the JSON-shaped artifact written ALWAYS (PASS or
// FAIL) per Phase 0 Plan 00-06's cmd/footprint pattern. SREs subscribe
// to the file path via dashboards; a missing data point would mask a
// regressed run.
type baselineEnvelope struct {
	MeasuredAt        string                `json:"measured_at"`
	Stats             load.RealIngestStats  `json:"stats"`
	Opts              load.RealIngestOpts   `json:"opts"`
	AssertUnder       string                `json:"assert_under"`
	AssertUnderMillis int64                 `json:"assert_under_ms"`
	Status            string                `json:"status"`
	Tenant            string                `json:"tenant"`
	PolarisEndpoint   string                `json:"polaris_endpoint"`
	Details           string                `json:"details,omitempty"`
}

func main() {
	var (
		dsn             = flag.String("dsn", "", "Postgres DSN (Phase 0.5 Pool A); falls back to DATABASE_URL env")
		tenant          = flag.String("tenant", "", "tenant UUID (canonical 36-char form)")
		polarisEndpoint = flag.String("polaris-endpoint", "", "Polaris catalog ROOT URL (e.g. http://host:8181/api/catalog)")
		polarisClientID = flag.String("polaris-client-id", "", "Polaris OAuth client id")
		polarisSecret   = flag.String("polaris-client-secret", "", "Polaris OAuth client secret")
		polarisWarehouse = flag.String("polaris-warehouse", "test", "Polaris warehouse name")
		targetTables    = flag.Int("target-tables", 100_000, "list-table cap")
		targetColumns   = flag.Int("target-columns", 5_000_000, "column-row cap")
		targetSnapshots = flag.Int("target-snapshots", 4_000_000, "snapshot-row cap")
		targetLineage   = flag.Int("target-lineage", 30_000_000, "lineage-edge cap")
		copyBatch       = flag.Int("copy-batch-size", 50_000, "rows per CopyFrom batch")
		cypherBatch     = flag.Int("cypher-batch-size", 1_000, "rows per Cypher MERGE batch")
		assertUnder     = flag.Duration("assert-completes-under", 30*time.Minute,
			"upper bound on TotalDuration; non-zero exit if exceeded")
		baselineOut = flag.String("baseline-out", "tests/load/_real-ingest-baseline.json",
			"JSON baseline artifact (always written — PASS or FAIL)")
	)
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("DATABASE_URL")
	}
	if *dsn == "" {
		failNoBaseline(baselineOut, *assertUnder, "DSN required (-dsn flag or DATABASE_URL env)")
	}
	if *tenant == "" {
		failNoBaseline(baselineOut, *assertUnder, "-tenant required")
	}
	tenantUUID, err := uuid.Parse(*tenant)
	if err != nil {
		failNoBaseline(baselineOut, *assertUnder, fmt.Sprintf("-tenant: %v", err))
	}
	if *polarisEndpoint == "" || *polarisClientID == "" || *polarisSecret == "" {
		failNoBaseline(baselineOut, *assertUnder,
			"-polaris-endpoint + -polaris-client-id + -polaris-client-secret all required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *assertUnder+5*time.Minute)
	defer cancel()

	// Build the AGE-aware GraphClient + share its pool — DO NOT
	// construct a second pool (Phase 0.5 must_have: BeforeAcquire
	// DISCARD ALL is the ONLY enforcement of session-bleed prevention).
	// We construct the GraphClient with the supplied DSN and reuse the
	// underlying pgxpool.Pool for the COPY phase.
	gc, err := graph.NewGraphClient(ctx, *dsn)
	if err != nil {
		failBaseline(baselineOut, *assertUnder, *tenant, *polarisEndpoint,
			fmt.Sprintf("graph.NewGraphClient: %v", err), load.RealIngestStats{}, load.RealIngestOpts{})
	}
	defer gc.Close()
	pool := gc.Pool()
	_ = pool

	// Construct the Polaris adapter against the supplied wire props.
	cfg := polaris.Config{
		Endpoint:     *polarisEndpoint,
		Warehouse:    *polarisWarehouse,
		ClientID:     *polarisClientID,
		ClientSecret: *polarisSecret,
	}
	adapter, err := polaris.New(ctx, cfg)
	if err != nil {
		failBaseline(baselineOut, *assertUnder, *tenant, *polarisEndpoint,
			fmt.Sprintf("polaris.New: %v", err), load.RealIngestStats{}, load.RealIngestOpts{})
	}

	tenantSchema := schemaNameFromUUID(tenantUUID.String())

	// Verify the per-tenant schema exists before launching the ingest
	// (otherwise the pre-check would error out with a confusing
	// "missing indexes" because the schema itself doesn't exist).
	if err := assertSchemaExists(ctx, pool, tenantSchema); err != nil {
		failBaseline(baselineOut, *assertUnder, *tenant, *polarisEndpoint,
			fmt.Sprintf("schema check: %v", err), load.RealIngestStats{}, load.RealIngestOpts{})
	}

	opts := load.RealIngestOpts{
		TargetTableCount:       *targetTables,
		TargetColumnCount:      *targetColumns,
		TargetSnapshotCount:    *targetSnapshots,
		TargetLineageEdgeCount: *targetLineage,
		CopyBatchSize:          *copyBatch,
		CypherBatchSize:        *cypherBatch,
	}

	stats, ingestErr := load.RealIngestEnvelope(ctx, adapter, gc, pool, tenantUUID.String(), tenantSchema, opts)

	status := "PASS"
	details := ""
	if ingestErr != nil {
		status = "FAIL"
		details = ingestErr.Error()
	} else if stats.TotalDuration > *assertUnder {
		status = "FAIL"
		details = fmt.Sprintf("TotalDuration %v exceeds -assert-completes-under=%v",
			stats.TotalDuration, *assertUnder)
	}

	bl := baselineEnvelope{
		MeasuredAt:        time.Now().UTC().Format(time.RFC3339Nano),
		Stats:             stats,
		Opts:              opts,
		AssertUnder:       assertUnder.String(),
		AssertUnderMillis: assertUnder.Milliseconds(),
		Status:            status,
		Tenant:            *tenant,
		PolarisEndpoint:   *polarisEndpoint,
		Details:           details,
	}
	if werr := writeBaseline(*baselineOut, bl); werr != nil {
		fmt.Fprintf(os.Stderr, "WARN: write baseline: %v\n", werr)
	}

	if status == "FAIL" {
		fmt.Fprintf(os.Stderr, "real-ingest FAIL details=%s tables=%d cols=%d snaps=%d lineage=%d total=%v\n",
			details, stats.TablesIngested, stats.ColumnsIngested,
			stats.SnapshotsIngested, stats.LineageEdgesIngested, stats.TotalDuration)
		os.Exit(1)
	}
	fmt.Printf("real-ingest PASS tables=%d cols=%d snaps=%d lineage=%d copy=%v cypher=%v total=%v\n",
		stats.TablesIngested, stats.ColumnsIngested, stats.SnapshotsIngested,
		stats.LineageEdgesIngested, stats.CopyPhaseDuration, stats.CypherPhaseDuration, stats.TotalDuration)
}

func writeBaseline(path string, bl baselineEnvelope) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(bl)
}

// failNoBaseline is the early-flag-validation path: writes a minimal
// FAIL baseline so monitors still see a data point, then exits 1.
func failNoBaseline(baselineOut *string, assertUnder time.Duration, msg string) {
	bl := baselineEnvelope{
		MeasuredAt:  time.Now().UTC().Format(time.RFC3339Nano),
		AssertUnder: assertUnder.String(),
		Status:      "FAIL",
		Details:     msg,
	}
	_ = writeBaseline(*baselineOut, bl)
	fmt.Fprintf(os.Stderr, "FAIL: %s\n", msg)
	os.Exit(1)
}

// failBaseline writes the baseline JSON with whatever stats / opts are
// available, then exits 1.
func failBaseline(baselineOut *string, assertUnder time.Duration,
	tenant, polarisEndpoint, msg string,
	stats load.RealIngestStats, opts load.RealIngestOpts,
) {
	bl := baselineEnvelope{
		MeasuredAt:        time.Now().UTC().Format(time.RFC3339Nano),
		Stats:             stats,
		Opts:              opts,
		AssertUnder:       assertUnder.String(),
		AssertUnderMillis: assertUnder.Milliseconds(),
		Status:            "FAIL",
		Tenant:            tenant,
		PolarisEndpoint:   polarisEndpoint,
		Details:           msg,
	}
	_ = writeBaseline(*baselineOut, bl)
	fmt.Fprintf(os.Stderr, "FAIL: %s\n", msg)
	os.Exit(1)
}

// schemaNameFromUUID maps the canonical 36-char UUID to its per-tenant
// Postgres schema name per Phase 0.5 D-0.5.04. Mirrors
// tests/integration/saas_fixtures.go::schemaNameFromUUID.
func schemaNameFromUUID(uuidString string) string {
	return "tenant_" + strings.ReplaceAll(uuidString, "-", "_")
}

// assertSchemaExists pre-checks the per-tenant schema is present; if
// not, the runner refuses to start (the index pre-check inside
// RealIngestEnvelope would otherwise produce a confusing "all indexes
// missing" message).
func assertSchemaExists(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("tenant schema %q not found (provision tenant first)", schema)
	}
	return nil
}

// Compile-time guard: keep the pgx import alive even if a refactor
// drops the only direct call site (we use it via pool.Query / pool.Exec
// today; this var keeps Bazel-style import discipline correct under
// future refactors).
var _ = pgx.QueryExecModeDescribeExec
