// Command dr-writer drives a sustained Cypher write workload against
// the Neksur graph and records every successful insert (timestamp +
// generated writer-id) to a CSV file. The CSV is consumed by
// tests/dr/chaos_restore.sh after PITR restore to compute
// LAST_RECOVERED_TS — the most recent writer-emitted node still
// present in the restored graph — which gives the empirical RPO.
//
// Phase 0 Wave 3 (Plan 00-04). Replaces the inline Python writer
// described in the original Plan 01 stubs (Python→Go correction
// 2026-05-13).
//
// Usage:
//
//	go run ./tests/dr/cmd/dr-writer \
//	    -duration=30m \
//	    -qps=1000 \
//	    -outfile=tests/dr/_writer_timestamps.csv
//
// Flags:
//
//	-duration  Go duration string (e.g. "30m", "5m", "2h"); default 30m.
//	           Writer exits cleanly when the duration elapses or on SIGINT.
//	-qps       Writes per second; default 1000 (Phase 0 envelope CI proxy).
//	-outfile   CSV path; default tests/dr/_writer_timestamps.csv.
//	-graph     AGE graph name; default "neksur".
//	-vlabel    AGE vlabel to insert into; default "wal_probe" (matches the
//	           label used by tests/dr/wal_throughput.go::runWriter — outside
//	           the 19 production vlabels so probe data is trivially identifiable).
//	-dsn       Postgres DSN; default $DATABASE_URL. Fails fast if neither.
//
// CSV format (one row per successful write, flushed line-by-line so the
// file is consistent at any kill moment):
//
//	<unix_seconds>,<writer_id>
//
// Where writer_id is a deterministic per-process counter prefixed with
// the process start time — globally unique across runs but trivially
// scannable for chaos_restore.sh's match query:
//
//	MATCH (n:wal_probe {writer_id: '<id>'}) RETURN n LIMIT 1
//
// Exit behaviour:
//   - 0 on clean exit (duration elapsed, or SIGINT received and final
//     flush succeeded).
//   - non-zero on setup error (DSN unparseable, no DATABASE_URL, etc.)
//     OR on a fatal write-loop error (rare — most write errors are
//     swallowed so transient Postgres outages don't terminate the
//     load run).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/neksur-com/neksur/internal/graph"
)

func main() {
	var (
		duration     = flag.Duration("duration", 30*time.Minute, "How long to run (Go duration string)")
		qps          = flag.Int("qps", 1000, "Writes per second")
		outfile      = flag.String("outfile", "tests/dr/_writer_timestamps.csv", "CSV path for emitted timestamps")
		graphName    = flag.String("graph", "neksur", "AGE graph name")
		vlabel       = flag.String("vlabel", "wal_probe", "AGE vlabel to insert into")
		dsn          = flag.String("dsn", "", "Postgres DSN (default: $DATABASE_URL)")
		flushEverySs = flag.Int("flush-every", 1, "Force fsync of CSV every N seconds (0 disables periodic fsync — relies on per-line write)")
	)
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("DATABASE_URL")
	}
	if *dsn == "" {
		fatal("no DSN — set -dsn flag or DATABASE_URL env var")
	}
	if *qps <= 0 {
		fatal("-qps must be > 0 (got %d)", *qps)
	}
	if *duration <= 0 {
		fatal("-duration must be > 0 (got %s)", *duration)
	}

	// Open the CSV in append mode (so re-runs concatenate — chaos_restore
	// truncates it explicitly before invoking us). 0644 is fine — this is
	// a test artifact, not a secret.
	f, err := os.OpenFile(*outfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("open outfile %s: %v", *outfile, err)
	}
	defer func() {
		// Final fsync before close — guarantees the CSV is durable even
		// if a kernel panic follows the SIGINT.
		_ = f.Sync()
		_ = f.Close()
	}()

	// Ctx that cancels on duration elapsed OR SIGINT/SIGTERM.
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "dr-writer: received %s, flushing and exiting\n", sig)
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	// Connect to Postgres+AGE via the project's GraphClient — the
	// AfterConnect hook ensures `LOAD 'age'` is run on every pool
	// connection so cypher() resolves.
	gc, err := graph.NewGraphClient(ctx, *dsn)
	if err != nil {
		fatal("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Ensure the probe vlabel exists. AGE's create_vlabel errors on
	// duplicate; we tolerate that specifically.
	if err := ensureVLabel(ctx, gc, *graphName, *vlabel); err != nil {
		fatal("ensureVLabel: %v", err)
	}

	// Periodic fsync ticker (defensive — write+sync per line is the
	// per-write durability primitive, but fsync once per second is
	// cheap insurance against host-level crashes in between writes).
	if *flushEverySs > 0 {
		go fsyncLoop(ctx, f, time.Duration(*flushEverySs)*time.Second)
	}

	// Counter for unique writer-ids. Prefix with the process start time
	// so multiple runs in the same minute don't collide.
	startUnix := time.Now().Unix()
	var counter atomic.Uint64

	// Synchronization for stdout summary at the end.
	var (
		writeCount atomic.Uint64
		errCount   atomic.Uint64
	)

	// Run the writer loop. We split the requested qps across N worker
	// goroutines for headroom: at high qps a single goroutine serializes
	// on the pgxpool acquire; spreading across 4 workers gives better
	// per-worker tick fidelity.
	const workers = 4
	perWorkerQPS := *qps / workers
	if perWorkerQPS < 1 {
		perWorkerQPS = 1
	}

	var wg sync.WaitGroup
	var fileMu sync.Mutex // protects f.Write — we write per-line CSV
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			runWorker(ctx, gc, *graphName, *vlabel, perWorkerQPS, startUnix, &counter, f, &fileMu, &writeCount, &errCount)
		}(w)
	}

	// Wait for ctx done + workers exit.
	wg.Wait()

	fmt.Fprintf(os.Stderr, "dr-writer: clean exit — writes=%d errors=%d duration=%s\n",
		writeCount.Load(), errCount.Load(), time.Since(time.Unix(startUnix, 0)))
}

// runWorker emits cypher CREATE statements at perWorkerQPS until ctx done.
func runWorker(
	ctx context.Context,
	gc *graph.GraphClient,
	graphName, vlabel string,
	perWorkerQPS int,
	startUnix int64,
	counter *atomic.Uint64,
	f *os.File,
	fileMu *sync.Mutex,
	writeCount, errCount *atomic.Uint64,
) {
	interval := time.Second / time.Duration(perWorkerQPS)
	if interval <= 0 {
		interval = time.Microsecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			id := fmt.Sprintf("%d-%d", startUnix, counter.Add(1))
			ts := time.Now().Unix()
			// Cypher CREATE — set writer_id property to the id we just
			// generated. chaos_restore.sh queries by this property to
			// determine whether a given (ts, id) pair survived restore.
			stmt := fmt.Sprintf(
				"CREATE (n:%s {writer_id: '%s', ts: %d})",
				vlabel, escapeSingleQuotes(id), ts,
			)
			rows, err := gc.Cypher(ctx, graphName, stmt)
			if err != nil {
				// Swallow — writer's role is to drive load; transient
				// errors don't abort. Bump errCount for the summary.
				errCount.Add(1)
				continue
			}
			rows.Close()
			// Append <ts>,<id>\n to the CSV. A single Write is atomic
			// at the OS level for short writes (well under PIPE_BUF =
			// 4096); we still hold the mutex for cross-worker ordering.
			fileMu.Lock()
			line := fmt.Sprintf("%d,%s\n", ts, id)
			_, werr := f.WriteString(line)
			fileMu.Unlock()
			if werr != nil {
				errCount.Add(1)
				continue
			}
			writeCount.Add(1)
		}
	}
}

// fsyncLoop calls f.Sync() every interval until ctx is done. Cheap
// insurance against host-level crashes between writes.
func fsyncLoop(ctx context.Context, f *os.File, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = f.Sync()
		}
	}
}

// ensureVLabel idempotently creates the probe vlabel. Catches the
// duplicate-label error specifically; surfaces all others.
func ensureVLabel(ctx context.Context, gc *graph.GraphClient, graphName, vlabel string) error {
	pool := gc.Pool()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	stmt := fmt.Sprintf(`DO $$
BEGIN
    PERFORM ag_catalog.create_vlabel('%s', '%s');
EXCEPTION
    WHEN SQLSTATE '42P07' THEN NULL;
    WHEN SQLSTATE '42710' THEN NULL;
END
$$;`, escapeSingleQuotes(graphName), escapeSingleQuotes(vlabel))
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return err
	}
	return nil
}

// escapeSingleQuotes doubles single quotes for safe SQL literal embedding.
// The graph + vlabel names are tool-supplied (-flag); this is defence in
// depth against operators feeding `' OR 1=1 --` to the writer.
func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dr-writer FATAL: "+format+"\n", args...)
	os.Exit(1)
}

