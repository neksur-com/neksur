// Goroutine-pool-backed L3 detection dispatcher per D-1.10 + D-1.11.
//
// One in-process pool sized via `NEKSUR_L3_WORKERS` env (default 4
// workers). Three trigger sources (30s baseline poller / Polaris webhook /
// S3 ObjectCreated SNS+SQS) push Hit{} structs to the SAME channel.
// Per-worker dedup via sync.Map keyed on metadata_location — the second
// arrival of the same Hit is a no-op (cross-replica dedup is the V0062
// UNIQUE constraint's job; this in-process dedup is the cheap per-worker
// optimization).
//
// Architecture rationale (CONTEXT line 76):
//
//   - In-process worker pool (not separate Lambda / not separate worker
//     binary) — Phase 1 aims for the simplest deployment shape; the
//     gateway and detection share state (graph client, catalog adapter
//     cache). Phase 6 may extract per-tenant pools or split off a
//     `neksur-l3-worker` binary if scan latency dominates a request
//     thread.
//   - ONE pool, NOT one per source — keeps capacity consolidated; the
//     channel buffer (256) absorbs source bursts.
//   - Per-replica in-process dedup — protects against a single replica
//     receiving the same SNS event twice (rare but possible during SNS
//     retry).
//   - Cross-replica dedup is V0062 UNIQUE — this pool's sync.Map does
//     NOT prevent cross-replica races (different processes don't share
//     the map). The DB constraint catches the race; workers swallow the
//     SQLSTATE 23505 as "already in flight".
//
// Threading: Pool.Run blocks until ctx.Done. The internal channel close
// happens on ctx cancellation; producers should select on ctx.Done +
// channel send to avoid blocking on a full channel after shutdown.

package dispatch

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
)

// defaultWorkers is the D-1.10 default — 4 goroutines per neksur-server
// replica. Operators tune via `NEKSUR_L3_WORKERS` env (large fleets may
// raise; tight-budget runs may lower).
const defaultWorkers = 4

// channelCapacity is the per-pool channel buffer size — absorbs short
// trigger-source bursts. Phase 1 fixed; Phase 6 may make per-tenant.
const channelCapacity = 256

// Hit is one detection trigger emitted by a producer (poller / webhook /
// s3events). MetadataLocation is the natural key for dedup.
type Hit struct {
	// TenantID is the tenant UUID (string form for dispatch into
	// gc.ExecuteInTenant). The producer resolves the tenant from its
	// per-source mapping (poller iterates tenants; webhook resolves
	// via signed payload metadata; s3events extract from event source
	// path).
	TenantID string

	// MetadataLocation is the snapshot's metadata.json URI — the
	// V0062 detection_runs.snapshot_metadata_location UNIQUE key.
	// In-process dedup + cross-replica dedup both pivot on this value.
	MetadataLocation string

	// Source is one of "poller" / "polaris-webhook" / "s3-event" — the
	// audit-trail source label. Phase 1 records it on the DetectionRun
	// node so SecOps can trace which trigger surface produced each scan.
	Source string

	// TableNamespace + TableName identify the offending table; the
	// scanner uses these to fetch the table's IcebergCatalogClient
	// adapter for manifest sampling.
	TableNamespace []string
	TableName      string
}

// Scanner is the per-Hit work unit. The cmd/neksur-server bootstrap
// wires a regexScanner that loads the Iceberg manifest, samples files
// per StratifiedSample, runs regex.RegexClassifier, calls
// regex.EmitDetectionResults, and posts Slack alerts on confidence
// ≥0.85.
//
// Pool depends on this interface (NOT on the regex package directly)
// so tests can inject a recording stub without pulling the full
// detection substrate.
type Scanner interface {
	Scan(ctx context.Context, hit Hit) error
}

// Pool is the in-process goroutine pool that consumes Hit{} from `in`
// and dispatches to scanner.Scan with per-MetadataLocation dedup.
//
// Construct via NewPool; start the workers via Run; shutdown via
// ctx.Cancel.
type Pool struct {
	workers int
	in      chan Hit
	seen    sync.Map // MetadataLocation → struct{}; in-process dedup
	scanner Scanner
}

// NewPool constructs a Pool reading from `in` and dispatching to
// `scanner`. Worker count from NEKSUR_L3_WORKERS env (default 4 per
// D-1.10). The channel `in` MUST be created by the caller (cmd/neksur-server)
// and shared across the 3 producers; this lets the producers be
// constructed independently and pushed to the shared channel.
func NewPool(in chan Hit, scanner Scanner) *Pool {
	workers := defaultWorkers
	if v := os.Getenv("NEKSUR_L3_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			workers = n
		}
	}
	return &Pool{
		workers: workers,
		in:      in,
		scanner: scanner,
	}
}

// Workers returns the configured worker count. Exposed for diagnostics +
// tests that want to validate the env override fired correctly.
func (p *Pool) Workers() int {
	return p.workers
}

// Run spawns p.workers goroutines that consume from p.in until ctx is
// cancelled. Each worker:
//
//  1. Reads a Hit from the channel.
//  2. Checks p.seen via LoadOrStore — if already seen this run, log +
//     skip. Otherwise mark seen + invoke scanner.Scan.
//  3. On scanner.Scan error, log via slog.Error (does NOT terminate
//     the worker — one bad scan must not poison the pool).
//
// Run blocks until ctx is cancelled, then waits for all workers to
// drain in-flight scans (sync.WaitGroup). Returns when all workers
// exit.
//
// Note: Pool does NOT close p.in on shutdown — the caller (cmd/neksur-
// server) owns the channel lifecycle (producers may want to drain).
// Workers exit on ctx.Done regardless of channel state.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case h, ok := <-p.in:
					if !ok {
						return
					}
					p.processHit(ctx, h, workerID)
				}
			}
		}(i)
	}
	wg.Wait()
}

// processHit applies the per-MetadataLocation dedup + invokes the
// scanner. Extracted for testability — the dedup branch is the load-
// bearing in-process dedup invariant tested by
// TestPoolDedupsSameMetadataLocation.
func (p *Pool) processHit(ctx context.Context, h Hit, workerID int) {
	if h.MetadataLocation == "" {
		slog.Warn("dispatch: empty MetadataLocation; skipping",
			"worker", workerID, "source", h.Source)
		return
	}
	if _, alreadySeen := p.seen.LoadOrStore(h.MetadataLocation, struct{}{}); alreadySeen {
		slog.Debug("dispatch: in-process dedup hit",
			"worker", workerID, "source", h.Source,
			"meta_loc", h.MetadataLocation)
		return
	}
	if err := p.scanner.Scan(ctx, h); err != nil {
		slog.Error("dispatch: scanner failed",
			"worker", workerID, "source", h.Source,
			"meta_loc", h.MetadataLocation, "err", err)
	}
}

// ResetSeenForTest clears the in-process dedup cache. Tests use this to
// re-trigger the same MetadataLocation across test cases without
// constructing a new Pool. Production code should never call this (the
// dedup is the load-bearing invariant).
func (p *Pool) ResetSeenForTest() {
	p.seen.Range(func(k, _ any) bool {
		p.seen.Delete(k)
		return true
	})
}
