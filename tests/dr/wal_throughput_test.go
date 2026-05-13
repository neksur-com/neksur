//go:build dr

package dr

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
)

// TestMeasureWalThroughput drives the A7 validation: it runs a
// 5-minute Cypher write workload at the Phase 0 envelope rate
// (1000 writes/sec — the single-host CI proxy for the production
// 10K events/sec target; production sizing extrapolates linearly)
// and asserts that the implied 15-minute WAL volume stays under
// the configured archive-push-queue-max (4 GB).
//
// The test writes a JSON report to `tests/dr/_wal_throughput_report.json`
// for `infra/pgbackrest/sizing.md` to reference as A7 evidence.
//
// Preconditions:
//   - Postgres + AGE 1.6.0 reachable at $DATABASE_URL.
//   - The `wal_probe` vlabel exists in the `neksur` graph (test
//     setup creates it if absent — see ensureProbeLabel).
//
// Skip behaviour: if $DATABASE_URL is empty, the test SKIPs (not
// fails). The CI matrix that includes the `dr` tag is responsible
// for setting $DATABASE_URL; running the dr suite locally without
// $DATABASE_URL is expected and should not block on a missing
// service.
//
// Build tag: this file is gated behind `//go:build dr`. Run via:
//
//	go test -tags dr -run TestMeasureWalThroughput -timeout 10m -v ./tests/dr/...
func TestMeasureWalThroughput(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("dr: TestMeasureWalThroughput requires DATABASE_URL — skipping (set DATABASE_URL=postgres://... to run)")
	}

	// Per-test context — 9 minutes, leaving 1 min headroom under the
	// recommended `-timeout 10m` invocation.
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	// Single direct connection for the LSN poller — pgx.Connect (not
	// pool) gives us a stable session for `pg_current_wal_lsn()`.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("dr: pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Separate GraphClient (uses a pool) for the writer goroutine.
	// AfterConnect hook in NewGraphClient runs `LOAD 'age'` on each
	// pool connection — required for cypher() to resolve.
	gc, err := graph.NewGraphClient(ctx, dsn)
	if err != nil {
		t.Fatalf("dr: NewGraphClient: %v", err)
	}
	defer gc.Close()

	if err := ensureProbeLabel(ctx, gc); err != nil {
		t.Fatalf("dr: ensureProbeLabel: %v", err)
	}

	// 300 seconds × 1000 writes/sec = 300K probe inserts. The 5s LSN
	// poll cadence yields ~60 samples — enough for a stable mean.
	const (
		durationS     = 300
		writeRatePerS = 1000
	)

	report, err := MeasureWalThroughput(ctx, conn, gc, durationS, writeRatePerS)
	if err != nil {
		t.Fatalf("dr: MeasureWalThroughput: %v", err)
	}
	if len(report.Samples) == 0 {
		t.Fatalf("dr: MeasureWalThroughput returned 0 samples — the run terminated before the first 5s tick; investigate context / writer setup")
	}

	// Persist the evidence report for sizing.md to reference.
	if err := writeReport(report, "_wal_throughput_report.json"); err != nil {
		t.Logf("dr: warning — failed to write report: %v", err)
	}

	t.Logf("dr: WAL throughput observed — Avg=%.2f MB/s Peak=%.2f MB/s Implied15MinWalGB=%.2f",
		report.AvgMBps, report.PeakMBps, report.Implied15MinWalGB)

	// A7 gate: the implied 15-minute WAL volume must be strictly
	// less than the configured `archive-push-queue-max=4GB` for the
	// pgBackRest async queue to absorb a 15-minute archive outage
	// without dropping segments.
	const archivePushQueueMaxGB = 4.0
	if report.Implied15MinWalGB >= archivePushQueueMaxGB {
		t.Fatalf("dr: A7 GATE FAILED — Implied15MinWalGB=%.2f >= archive-push-queue-max=%.0fGB; raise the queue-max in infra/pgbackrest/pgbackrest.conf.template AND update infra/pgbackrest/sizing.md with the new sized value before declaring Plan 00-04 complete",
			report.Implied15MinWalGB, archivePushQueueMaxGB)
	}
}

// ensureProbeLabel idempotently creates the `wal_probe` AGE vlabel
// used by runWriter's CREATE statement. AGE's create_vlabel returns
// an error if the label already exists; we tolerate that error
// specifically and surface any other error to the caller.
func ensureProbeLabel(ctx context.Context, gc *graph.GraphClient) error {
	pool := gc.Pool()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// LOAD 'age' is already done by the pool's AfterConnect hook.
	// SELECT create_vlabel('neksur', 'wal_probe') — wrapped in a
	// DO block that swallows the duplicate-label error so re-runs
	// are idempotent.
	const stmt = `DO $$
BEGIN
    PERFORM ag_catalog.create_vlabel('neksur', 'wal_probe');
EXCEPTION
    WHEN SQLSTATE '42P07' THEN
        -- duplicate_table — vlabel already exists, fine.
        NULL;
    WHEN SQLSTATE '42710' THEN
        -- duplicate_object — also a benign already-exists shape.
        NULL;
END
$$;`
	_, err = conn.Exec(ctx, stmt)
	return err
}

// writeReport marshals the report to pretty JSON and writes it
// atomically (write-to-temp + rename) to the given relative path
// under tests/dr/. Caller is the test, so the cwd is tests/dr/.
func writeReport(r *WalThroughputReport, name string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
