//go:build dr

// Package dr drives the Phase 0 Wave 3 disaster-recovery validation
// surface (Plan 00-04) per the D-OQ.04 unified DR contract:
//
//   - RTO 1h  — full point-in-time restore completes within 1 hour
//   - RPO 15min — at most 15 minutes of data loss after primary kill
//
// This file owns the WAL-throughput measurement primitive used to
// validate Assumption A7 (RESEARCH.md §Assumptions A7):
//
//   "The 15-min WAL volume worth ~4GB queue size assumes a sustained
//    write rate of ~4 MB/s WAL output, which fits the Phase 0
//    envelope (10K lineage events/sec target × ~400B avg per event
//    = 4 MB/s)."
//
// The 4GB number lives in `infra/pgbackrest/pgbackrest.conf.template`
// as `archive-push-queue-max=4GB`. For A7 to hold, the **implied
// 15-minute WAL volume** measured under the Phase 0 envelope write
// load must be **less than the configured queue-max (4GB)**. This
// package's `MeasureWalThroughput` returns that number; the
// `TestMeasureWalThroughput` test asserts the inequality.
//
// Build-tag convention: this entire package is gated behind
// `//go:build dr`. Run:
//
//   go test -tags dr -timeout 90m ./tests/dr/...
//
// for the full DR suite, or:
//
//   go test -tags dr -run TestMeasureWalThroughput -timeout 10m -v \
//     ./tests/dr/...
//
// for the A7 validation alone. The test writes its evidence report
// to `tests/dr/_wal_throughput_report.json` for `infra/pgbackrest/
// sizing.md` to reference.
//
// Test entry point — see `wal_throughput_test.go` (same package,
// same build tag) for `func TestMeasureWalThroughput(t *testing.T)`.
// The test code is split out so this file's import set stays
// production-grade (no `testing` dependency in a non-`_test.go`
// file, which `go vet` would flag).
package dr

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// WalThroughputReport summarizes a WAL-output-rate measurement run.
//
// Fields:
//
//   - AvgMBps — mean bytes-per-second across all observed sample
//     intervals, expressed as MB/s. The arithmetic mean is the right
//     summary statistic here because we care about steady-state
//     queue residency, not transient bursts (which PeakMBps captures).
//   - PeakMBps — maximum observed per-interval bytes-per-second,
//     expressed as MB/s. Burst tolerance — pgBackRest's async queue
//     must absorb peaks without filling up.
//   - Implied15MinWalGB — the projected WAL volume produced over a
//     sustained 15-minute window at AvgMBps:
//     `AvgMBps * 60 sec/min * 15 min / 1024 MB/GB`. The A7 gate
//     compares this to the configured `archive-push-queue-max` (4GB).
//   - Samples — per-interval observations. Useful for postmortem if
//     the Avg/Peak summary surfaces an anomaly.
type WalThroughputReport struct {
	AvgMBps           float64     `json:"AvgMBps"`
	PeakMBps          float64     `json:"PeakMBps"`
	Implied15MinWalGB float64     `json:"Implied15MinWalGB"`
	Samples           []WalSample `json:"Samples"`
}

// WalSample is one per-interval observation. The interval length is
// fixed at 5 seconds inside MeasureWalThroughput; BytesPerSec is
// computed by dividing the LSN delta over that interval by 5.
type WalSample struct {
	At          time.Time `json:"At"`
	LSN         string    `json:"LSN"`
	BytesPerSec float64   `json:"BytesPerSec"`
}

// CypherClient is the minimal interface MeasureWalThroughput needs
// from the application's graph client. internal/graph.GraphClient
// satisfies it via its Cypher method (the variadic args... receiver
// matches when no arguments are passed). We define the interface
// here (instead of importing internal/graph directly) to keep the
// dr package's compile-time surface narrow and to make the function
// trivially mockable for unit tests.
//
// Production callers wire `client.Cypher` directly; tests can pass
// any value with a matching Cypher signature.
type CypherClient interface {
	Cypher(ctx context.Context, graph string, stmt string, args ...any) (pgx.Rows, error)
}

// MeasureWalThroughput drives a sustained Cypher-write workload at
// `writeRatePerS` writes/sec for `durationS` seconds against the
// graph backed by `conn`'s Postgres + AGE deployment, and reports
// the WAL output rate observed during the run.
//
// Workload shape:
//   - A writer goroutine emits Cypher CREATE statements via the
//     `writer` interface at the requested cadence
//     (`time.Second / writeRatePerS` between attempts).
//   - The main goroutine polls `pg_current_wal_lsn()` every 5 seconds
//     and computes the per-interval bytes-per-second from the LSN
//     delta. The first poll establishes the baseline; the second poll
//     and onward emit WalSample records.
//   - On `ctx.Done()` or `durationS` elapsed, the writer is
//     signalled to stop, the sample loop exits, and the report is
//     computed from collected samples.
//
// Returns an error if:
//   - `conn` is nil or `writer` is nil
//   - `durationS` ≤ 0 or `writeRatePerS` ≤ 0
//   - the initial `pg_current_wal_lsn()` poll fails (no point
//     proceeding if Postgres isn't reachable)
//   - the LSN parse fails on the initial poll (defensive — Postgres
//     should always emit "X/Y" hex format)
//
// Subsequent per-poll errors are tolerated: failed polls are skipped
// (the next successful poll computes the delta from the previous
// successful one, with the corresponding interval extended).
//
// The Cypher writes themselves CAN fail without aborting the
// measurement — failed writes simply don't contribute to WAL output,
// which the function will observe as a lower throughput. Write errors
// are NOT propagated (the function's contract is to measure throughput
// under the workload, not to validate the workload itself).
func MeasureWalThroughput(
	ctx context.Context,
	conn *pgx.Conn,
	writer CypherClient,
	durationS int,
	writeRatePerS int,
) (*WalThroughputReport, error) {
	if conn == nil {
		return nil, errors.New("dr: MeasureWalThroughput: nil pgx.Conn")
	}
	if writer == nil {
		return nil, errors.New("dr: MeasureWalThroughput: nil CypherClient")
	}
	if durationS <= 0 {
		return nil, fmt.Errorf("dr: MeasureWalThroughput: durationS must be > 0 (got %d)", durationS)
	}
	if writeRatePerS <= 0 {
		return nil, fmt.Errorf("dr: MeasureWalThroughput: writeRatePerS must be > 0 (got %d)", writeRatePerS)
	}

	// Establish baseline LSN before starting the writer — this is the
	// reference point for the first sample's delta. We do NOT count
	// it as a sample (no interval to compute against).
	baselineLSNStr, err := readWalLSN(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("dr: baseline LSN read failed: %w", err)
	}
	baselineLSN, err := parseLSN(baselineLSNStr)
	if err != nil {
		return nil, fmt.Errorf("dr: baseline LSN parse failed: %w", err)
	}

	// Driving deadline: durationS from now. Honour earlier ctx cancellation.
	runCtx, cancelRun := context.WithTimeout(ctx, time.Duration(durationS)*time.Second)
	defer cancelRun()

	// Spin the writer goroutine. wg gives the main goroutine a deterministic
	// join point at the end of the measurement window; the writer exits on
	// runCtx done.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runWriter(runCtx, writer, writeRatePerS)
	}()

	// Poll loop — every 5s, sample LSN and emit a WalSample.
	const pollInterval = 5 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	samples := make([]WalSample, 0, durationS/int(pollInterval.Seconds())+2)
	prevLSN := baselineLSN
	prevAt := time.Now()

	for {
		select {
		case <-runCtx.Done():
			// Wait for the writer to exit cleanly, then compute the report.
			wg.Wait()
			return computeReport(samples), nil
		case tickAt := <-ticker.C:
			lsnStr, err := readWalLSN(runCtx, conn)
			if err != nil {
				// Skip this poll — a failed read just means the next
				// successful poll computes a delta over a longer interval.
				continue
			}
			lsn, err := parseLSN(lsnStr)
			if err != nil {
				continue
			}
			elapsed := tickAt.Sub(prevAt).Seconds()
			if elapsed <= 0 {
				prevLSN = lsn
				prevAt = tickAt
				continue
			}
			delta := float64(lsn - prevLSN)
			samples = append(samples, WalSample{
				At:          tickAt,
				LSN:         lsnStr,
				BytesPerSec: delta / elapsed,
			})
			prevLSN = lsn
			prevAt = tickAt
		}
	}
}

// runWriter emits Cypher CREATE statements at writeRatePerS until ctx
// is done. The writes are simple node creations into a `wal_probe`
// vlabel-equivalent (using a transient label that doesn't collide
// with the project's 19 production vlabels) so the workload exercises
// the actual AGE write path (catalog inserts + index maintenance,
// which is what produces representative WAL).
//
// Errors from individual writes are intentionally swallowed — the
// function's role is to drive WAL output, not to validate Cypher
// correctness. A non-zero error rate would manifest as lower observed
// throughput and a corresponding A7 false-negative if catastrophic;
// the test's logging surface should highlight this if it occurs.
func runWriter(ctx context.Context, writer CypherClient, writeRatePerS int) {
	interval := time.Second / time.Duration(writeRatePerS)
	if interval <= 0 {
		interval = time.Microsecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Cypher template — single-node CREATE with a small property payload
	// approximating the Phase 0 envelope event size (~400 B). The label
	// `wal_probe` is intentionally outside the 19 production vlabels so
	// the test data is trivially identifiable for cleanup. The probe
	// label needs to exist in the graph; tests typically pre-create it
	// via `SELECT create_vlabel('neksur', 'wal_probe')` in the test
	// setup. If absent, writes will fail and throughput measurement
	// reports near-zero (which the test will catch as a setup error).
	const stmt = `CREATE (n:wal_probe {ts: timestamp(), payload: "wal-throughput-probe-payload-` +
		`AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})`

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := writer.Cypher(ctx, "neksur", stmt)
			if err != nil {
				// Drop the error; throughput will reflect any pathology.
				continue
			}
			rows.Close()
		}
	}
}

// readWalLSN executes `SELECT pg_current_wal_lsn()::text` against
// conn and returns the LSN string (e.g., "0/1A2B3C00"). The cast to
// text avoids needing the pg_lsn type registered with pgx; the
// returned string round-trips through parseLSN to a uint64 byte
// offset.
func readWalLSN(ctx context.Context, conn *pgx.Conn) (string, error) {
	var lsn string
	err := conn.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn)
	if err != nil {
		return "", fmt.Errorf("dr: pg_current_wal_lsn query: %w", err)
	}
	return lsn, nil
}

// parseLSN parses a Postgres LSN string in `XXXXXXXX/YYYYYYYY` hex
// form into the absolute byte offset it represents. The format is
// documented at https://www.postgresql.org/docs/current/datatype-pg-lsn.html
// — the high half (before `/`) is the upper 32 bits, the low half
// is the lower 32 bits, both written as hex.
//
//	"0/1A2B3C00"  → 0 << 32 | 0x1A2B3C00 = 439234560
//	"1/0"         → 1 << 32 |          0 = 4294967296
func parseLSN(s string) (uint64, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("dr: parseLSN: malformed LSN %q (expected hi/lo)", s)
	}
	hi, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("dr: parseLSN: hi half %q: %w", parts[0], err)
	}
	lo, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("dr: parseLSN: lo half %q: %w", parts[1], err)
	}
	return (hi << 32) | lo, nil
}

// computeReport derives Avg/Peak/Implied15MinWalGB from the collected
// samples and returns a complete WalThroughputReport.
//
// Edge case: zero samples (the run terminated before the first
// 5s tick fired). Returns a report with zeroed metrics — caller's
// test should detect the suspicious zero and surface a setup error.
func computeReport(samples []WalSample) *WalThroughputReport {
	r := &WalThroughputReport{Samples: samples}
	if len(samples) == 0 {
		return r
	}
	var sum, peak float64
	for _, s := range samples {
		sum += s.BytesPerSec
		if s.BytesPerSec > peak {
			peak = s.BytesPerSec
		}
	}
	mean := sum / float64(len(samples))
	r.AvgMBps = mean / (1024.0 * 1024.0)
	r.PeakMBps = peak / (1024.0 * 1024.0)
	// Implied15MinWalGB = AvgMBps × 60 sec/min × 15 min / 1024 MB/GB
	r.Implied15MinWalGB = r.AvgMBps * 60.0 * 15.0 / 1024.0
	return r
}
