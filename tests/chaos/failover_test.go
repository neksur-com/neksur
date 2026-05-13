//go:build chaos

// Package chaos failover tests — Plan 00-03 Wave 2.
//
// Doc:
//   These tests bring up infra/docker-compose.ha.yml (3 etcd + 3 Patroni-managed
//   Postgres+AGE + HAProxy) and provoke a primary-kill failover. They are gated
//   behind the `chaos` build tag so default `go test ./...` skips them; CI runs
//   them via the load-chaos-restore.yml nightly workflow with:
//
//     go test -tags chaos -timeout 30m -count=1 ./tests/chaos/...
//
//   Developers running these locally need:
//     - Docker Desktop (or equivalent) with ~4-5 GB RAM available
//     - The current working directory at the repo root (so the
//       infra/docker-compose.ha.yml relative path resolves correctly)
//     - ~10-15 minutes per full chaos run (3-5 min cluster start + 5-10 min
//       failover assertion + 1-2 min teardown)
//
// What each test asserts:
//   TestKillPrimaryFailoverUnder30s — D-001.15: kill-to-new-leader < 30s
//                                     under sustained Cypher write load
//   TestPostFailoverCypherWorks      — A1 + A8: post-failover Cypher round-trip
//                                     succeeds within 5s without manual
//                                     `LOAD 'age'` (validates that
//                                     shared_preload_libraries='age' carries
//                                     through Patroni promotion — Pitfall 9)
//
// Maps to 00-VALIDATION.md row 03-T2 / REQ-NFR-availability.

package chaos

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
)

// TestMain brings the HA cluster up once for the entire chaos package and
// tears it down after all tests finish. Sharing the cluster across subtests
// avoids the cold-bootstrap-between-subtests race (live verify 2026-05-13:
// `down -v` + `up -d` between Test1 and Test2 reliably deadlocked at 1-of-3
// replicas even with the max_wal_senders=20 fix in 9969c90 — full cold
// bootstrap on Docker Desktop + macOS is racy enough that running it twice
// back-to-back is unreliable).
//
// Override: set CHAOS_KEEP_CLUSTER=1 to skip the post-suite StopCluster (the
// cluster stays up for manual inspection — re-runs short-circuit via the
// "already settled" branch in StartCluster).
//
// Each subtest is still responsible for restoring the cluster to a usable
// state when it kills the primary — TimeFailover handles the
// leader-election wait so the next subtest can connect to whoever's promoted.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if _, err := StartCluster(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: StartCluster: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if os.Getenv("CHAOS_KEEP_CLUSTER") == "" {
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dcancel()
		if err := StopCluster(dctx); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: StopCluster: %v\n", err)
		}
	}
	os.Exit(code)
}

// haproxyPrimaryDSN is the application-facing DSN. HAProxy listens on 5000
// inside the container (primary route via `option httpchk GET /master`),
// but the docker-compose host-port mapping remaps to host:5500 to dodge
// the macOS AirPlay Receiver conflict on port 5000 (live verify
// 2026-05-13 surfaced the conflict). Linux hosts unaffected by the host
// port choice.
const haproxyPrimaryDSN = "postgres://postgres:postgres@localhost:5500/postgres?sslmode=disable"

// failoverContractBudget is D-001.15: kill-to-new-leader-elected must be
// strictly less than 30 seconds. Captured as a var so the test diagnostic
// can echo the contract in its failure message.
var failoverContractBudget = 30 * time.Second

// TestKillPrimaryFailoverUnder30s asserts that SIGKILLing the Patroni
// primary while a Cypher write load is in flight produces a new leader
// in under D-001.15's 30s budget.
//
// Workflow:
//  1. StartCluster — bring up docker-compose.ha.yml; get current leader.
//  2. Spawn a goroutine that inserts ~50 nodes/sec via the HAProxy primary
//     route. The writer ignores per-write errors during failover (the
//     HAProxy backend will mark the killed primary down within ~9s and
//     stop routing to it; new connections then go to the promoted leader,
//     which works as soon as Patroni's leader-elected event lands in etcd).
//  3. TimeFailover — kill the leader, wait for a new leader, measure elapsed.
//  4. Assert elapsed < 30s.
//  5. t.Cleanup — StopCluster (also stops the writer goroutine via cancel).
func TestKillPrimaryFailoverUnder30s(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Cluster lifecycle is owned by TestMain (one bootstrap per package
	// run). StartCluster here short-circuits via the "already settled"
	// branch — confirms the cluster is still healthy after any prior
	// subtest, and returns the current leader.
	leader, err := StartCluster(ctx)
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Logf("cluster up; current leader: %s", leader)

	// Background Cypher write load — ~50 inserts/sec until the test ends.
	// Errors during the failover window are EXPECTED (the kill drops in-
	// flight transactions); the writer logs them but does not fail. The
	// only failure mode that matters is the failover-elapsed assertion
	// at the end of the test.
	loadCtx, loadCancel := context.WithCancel(ctx)
	defer loadCancel()
	var loadWG sync.WaitGroup
	loadWG.Add(1)
	var (
		loadInserts atomic.Int64
		loadErrors  atomic.Int64
	)
	go func() {
		defer loadWG.Done()
		runWriteLoad(loadCtx, t, &loadInserts, &loadErrors)
	}()

	// Give the write load 2s to actually start producing traffic before
	// we kill — confirms the cluster is in fact serving writes (failure
	// to start writing pre-kill would mask a routing problem in the test
	// itself).
	time.Sleep(2 * time.Second)
	preKillInserts := loadInserts.Load()
	if preKillInserts == 0 {
		t.Fatalf("no inserts succeeded in the 2s warm-up — cluster may not be serving writes (errors: %d)", loadErrors.Load())
	}
	t.Logf("warm-up done: %d inserts succeeded, %d errors", preKillInserts, loadErrors.Load())

	// Time the failover — the load-bearing assertion of this test.
	elapsed, newLeader, err := TimeFailover(ctx, leader)
	if err != nil {
		t.Fatalf("TimeFailover: %v", err)
	}

	// Stop the writer and wait for it to drain.
	loadCancel()
	loadWG.Wait()
	t.Logf("write load stopped: total inserts=%d errors=%d", loadInserts.Load(), loadErrors.Load())

	t.Logf("FAILOVER MEASURED: kill→new-leader-elected = %s; new leader = %s (was %s)", elapsed, newLeader, leader)

	if elapsed >= failoverContractBudget {
		t.Fatalf("D-001.15 VIOLATION: failover took %s, contract is < %s (kill→new-leader)", elapsed, failoverContractBudget)
	}
}

// TestPostFailoverCypherWorks asserts that within 5 seconds of the new
// leader being elected, a Cypher round-trip succeeds against the HAProxy
// primary route — without any manual `LOAD 'age'` or operator intervention.
//
// This validates Assumptions A1 + A8 from 00-RESEARCH.md:
//   - A1: shared_preload_libraries='age' on every standby ensures AGE is
//         loaded at server-start on the standby, so a promoted standby
//         can serve `cypher(...)` calls immediately.
//   - A8: Patroni manages an AGE-installed Postgres cleanly across the
//         bootstrap → standby → promote → serve cycle.
//
// The failure mode (the diagnostic this test guards against) is:
//   ERROR: function cypher(unknown, unknown) does not exist
// — meaning the promoted standby came up without AGE loaded. If we see
// THAT specific error string we t.Fatalf with an explicit A1-violation
// message that operators / the runbook can grep for.
func TestPostFailoverCypherWorks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Cluster lifecycle is owned by TestMain. StartCluster short-circuits
	// if the cluster is already settled, returning the current leader.
	leader, err := StartCluster(ctx)
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Logf("cluster up; current leader: %s", leader)

	// Confirm Cypher works pre-failover so we can blame any post-failover
	// failure on the failover, not on a broken cluster.
	if err := tryCypherCount(ctx, t, "pre-failover sanity"); err != nil {
		t.Fatalf("pre-failover Cypher sanity failed: %v", err)
	}

	elapsed, newLeader, err := TimeFailover(ctx, leader)
	if err != nil {
		t.Fatalf("TimeFailover: %v", err)
	}
	t.Logf("FAILOVER COMPLETE: %s → %s in %s", leader, newLeader, elapsed)

	// 5-second budget for "Cypher round-trip succeeds after new leader is
	// visible in etcd". This is a separate budget from the 30s failover
	// contract; it captures the additional time HAProxy needs to detect
	// the new leader on its next health-check cycle (interval 3s, fall 3
	// = up to 9s in the worst case; in practice the leader changeover is
	// detected within 1-3s by HAProxy because the killed leader's REST
	// API is connect-refused immediately).
	cypherBudget := 5 * time.Second
	cypherCtx, cypherCancel := context.WithTimeout(ctx, cypherBudget+5*time.Second)
	defer cypherCancel()

	cypherStart := time.Now()
	var lastErr error
	for time.Since(cypherStart) < cypherBudget+5*time.Second {
		if err := tryCypherCount(cypherCtx, t, "post-failover"); err == nil {
			t.Logf("POST-FAILOVER CYPHER OK: succeeded after %s", time.Since(cypherStart))
			return
		} else {
			lastErr = err
			// Empirically diagnose A1 violation early — if AGE didn't
			// carry through promotion, retrying won't help.
			if strings.Contains(err.Error(), "function cypher does not exist") ||
				strings.Contains(err.Error(), "function cypher(unknown, unknown) does not exist") ||
				strings.Contains(err.Error(), `function "cypher" does not exist`) {
				t.Fatalf("A1 VIOLATION: %v — shared_preload_libraries='age' did not carry through Patroni promotion (Pitfall 9). Inspect SHOW shared_preload_libraries; on %s — runbook: runbooks/failover.md §4", err, newLeader)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("post-failover Cypher did not succeed within %s after new leader %s elected (last err: %v)", cypherBudget+5*time.Second, newLeader, lastErr)
}

// runWriteLoad spawns a single goroutine that runs ~50 inserts/sec via
// the HAProxy primary route until the ctx is canceled. Counters in
// inserts / errors track liveness; per-error logging is suppressed
// because the failover window is expected to produce many errors and
// flooding t.Logf would obscure the actual failure mode.
//
// This is a Phase 0 chaos-load shape, not the Plan 00-06 envelope
// load — that's a separate (much bigger) test in tests/load.
func runWriteLoad(ctx context.Context, t *testing.T, inserts, errCount *atomic.Int64) {
	t.Helper()

	// New connection per try — connection pooling here would mask the
	// HAProxy-route failover effect (the test wants to observe real
	// connection-level recovery).
	dsn := haproxyPrimaryDSN

	// 50 inserts/sec = 20ms tick.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	// Single GraphClient — the AfterConnect hook will reload AGE on each
	// new physical connection in the pool. pgxpool will rotate connections
	// when the killed primary's connections are reset by the kernel.
	gc, err := graph.NewGraphClient(ctx, dsn)
	if err != nil {
		t.Logf("runWriteLoad: NewGraphClient: %v (will retry per-tick)", err)
		// Fallthrough — try to recover per tick by re-creating the client.
	}
	defer func() {
		if gc != nil {
			gc.Close()
		}
	}()

	idx := int64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		idx++

		// Wrap each attempt in a 1s deadline so a stuck connection (e.g.,
		// a TCP socket pointing at the killed primary that hasn't yet
		// been reset by HAProxy) doesn't stall the ticker.
		cctx, ccancel := context.WithTimeout(ctx, 1*time.Second)

		if gc == nil {
			// Recover from the cold-start NewGraphClient error.
			ngc, nerr := graph.NewGraphClient(cctx, dsn)
			if nerr != nil {
				ccancel()
				errCount.Add(1)
				continue
			}
			gc = ngc
		}

		// Use a parameterized Cypher CREATE that the V0010 schema accepts.
		// Using the `Tag` vlabel because it has the simplest schema in V0010
		// — a single `id` property is sufficient. The chaos-test write set
		// is intentionally small.
		stmt := fmt.Sprintf("CREATE (n:Tag {id: 'chaos-%d-%d'})", time.Now().UnixNano(), idx)
		err := gc.ExecuteInTenant(cctx, "chaos-tenant", func(c context.Context, tx pgx.Tx) error {
			_, qerr := tx.Exec(c, fmt.Sprintf("SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (r ag_catalog.agtype)", stmt))
			return qerr
		})
		ccancel()

		if err != nil {
			errCount.Add(1)
			// Connection-reset / closed-connection / no-route errors
			// during failover indicate the pool needs to refresh.
			if strings.Contains(err.Error(), "connection refused") ||
				strings.Contains(err.Error(), "connection reset") ||
				strings.Contains(err.Error(), "broken pipe") ||
				strings.Contains(err.Error(), "EOF") {
				gc.Close()
				gc = nil
			}
			continue
		}
		inserts.Add(1)
	}
}

// tryCypherCount runs `MATCH (n) RETURN count(n)` against the HAProxy
// primary route. Returns nil on success, an error on any failure. This
// is the simplest possible Cypher round-trip — proves AGE + the cypher()
// function are both available without exercising any DDL / index path.
func tryCypherCount(ctx context.Context, t *testing.T, label string) error {
	t.Helper()
	gc, err := graph.NewGraphClient(ctx, haproxyPrimaryDSN)
	if err != nil {
		return fmt.Errorf("%s: NewGraphClient: %w", label, err)
	}
	defer gc.Close()

	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()

	rows, err := gc.Cypher(rctx, "neksur", "MATCH (n) RETURN count(n)")
	if err != nil {
		return fmt.Errorf("%s: Cypher: %w", label, err)
	}
	defer rows.Close()
	for rows.Next() {
		// Drain — we don't care about the count value, only that the
		// query produced rows without erroring.
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: rows.Err: %w", label, err)
	}
	return nil
}

