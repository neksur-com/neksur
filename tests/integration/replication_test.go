//go:build integration

// Package integration replication test — Plan 00-03 Wave 2 / REQ-NFR-availability.
//
// TestReplicaLagUnderLoad asserts that under sustained 1K writes/sec for
// 5 minutes against the Patroni HA cluster, replica replay lag stays below
// 10 seconds at every sample. This is the read-path integrity gate for the
// HAProxy replica route — applications routing reads to followers must see
// recent writes within tolerance.
//
// Build tag: `//go:build integration`. Default `go test ./...` skips this.
// The test runs only when REPLICATION env var is set, because the 5-minute
// runtime is unsuitable for the fast `make test-integration` tier (which
// budgets <10 minutes total). Invocation:
//
//   REPLICATION=1 go test -tags integration -timeout 10m -run TestReplicaLagUnderLoad ./tests/integration/...
//
// On failure the lag time-series is written to a CSV file in the package
// directory for diagnostic investigation (see writeLagCSV).

package integration

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
)

const (
	// haproxyPrimaryDSNRepl is HAProxy's primary frontend. Inside the
	// container HAProxy listens on 5000; docker-compose maps host 5500
	// → container 5000 to dodge the macOS AirPlay Receiver conflict
	// (live verify 2026-05-13). All writes go here so HAProxy routes
	// to the current Patroni leader.
	haproxyPrimaryDSNRepl = "postgres://postgres:postgres@localhost:5500/postgres?sslmode=disable"

	// pg-node-1 direct DSN for primary-side pg_stat_replication queries.
	// docker-compose.ha.yml maps pg-node-1's 5432 → host 5411.
	pgNode1DirectDSN = "postgres://postgres:postgres@localhost:5411/postgres?sslmode=disable"

	// Plan-mandated test parameters.
	replWritesPerSecond = 1000
	replDuration        = 5 * time.Minute
	replSampleInterval  = 5 * time.Second
	replLagTolerance    = 10 * time.Second
)

// TestReplicaLagUnderLoad runs 1K writes/sec for 5 minutes and samples
// `replay_lag` from pg_stat_replication every 5 seconds. Asserts max <10s.
//
// Skips unless REPLICATION env var is set (5-min runtime).
func TestReplicaLagUnderLoad(t *testing.T) {
	if os.Getenv("REPLICATION") == "" {
		t.Skip("REPLICATION not set — skipping 5-minute replica-lag test (set REPLICATION=1 to run)")
	}
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping testcontainers-based test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Bring up the HA cluster ourselves — TestMain's testcontainer is a
	// single-node AGE container (not the HA shape this test needs).
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dcancel()
		stopHACluster(dctx, t)
	})
	if err := startHACluster(ctx, t); err != nil {
		t.Fatalf("startHACluster: %v", err)
	}
	// Provision AGE in the fresh cluster: CREATE EXTENSION + create_graph.
	// docker-compose.ha.yml brings up Patroni-managed Postgres without our
	// Plan 02 migrations; this is the minimum needed for cypher() calls to
	// resolve. Idempotent.
	if err := setupAGEForReplicationTest(ctx, t); err != nil {
		t.Fatalf("setupAGEForReplicationTest: %v", err)
	}

	// Background write load — multiple workers because a single goroutine
	// at 1K inserts/sec is bottlenecked by per-tx round-trip latency
	// (typical ~1-3ms even on localhost = 300-1000 inserts/sec ceiling
	// per goroutine). 4 workers at ~250 inserts/sec each gets us to the
	// 1K target with comfortable headroom.
	loadCtx, loadCancel := context.WithCancel(ctx)
	defer loadCancel()
	const workers = 4
	const perWorkerRate = replWritesPerSecond / workers
	tickInterval := time.Second / time.Duration(perWorkerRate)

	var loadWG sync.WaitGroup
	var (
		totalInserts atomic.Int64
		totalErrors  atomic.Int64
	)
	for w := 0; w < workers; w++ {
		loadWG.Add(1)
		go func(workerID int) {
			defer loadWG.Done()
			runReplWriteWorker(loadCtx, t, workerID, tickInterval, &totalInserts, &totalErrors)
		}(w)
	}

	// Sample lag every 5 seconds.
	samples := make([]lagSample, 0, int(replDuration/replSampleInterval)+2)

	primaryConn, err := pgx.Connect(ctx, pgNode1DirectDSN)
	if err != nil {
		t.Fatalf("connect to pg-node-1 direct: %v", err)
	}
	defer primaryConn.Close(context.Background())

	loadStart := time.Now()
	deadline := loadStart.Add(replDuration)
	sampleTicker := time.NewTicker(replSampleInterval)
	defer sampleTicker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done during sampling loop: %v", ctx.Err())
		case sampleTS := <-sampleTicker.C:
			lag, err := sampleReplayLag(ctx, primaryConn)
			if err != nil {
				// Sampling errors during a brief leader change (none expected
				// in this test — no kill — but defensive) get logged and the
				// sample skipped.
				t.Logf("sample lag err: %v", err)
				continue
			}
			samples = append(samples, lagSample{t: sampleTS, lag: lag})
			t.Logf("sample @ %s: lag=%s inserts=%d errs=%d",
				sampleTS.Sub(loadStart).Round(time.Second), lag, totalInserts.Load(), totalErrors.Load())
		}
	}

	loadCancel()
	loadWG.Wait()
	t.Logf("write load complete: total inserts=%d errors=%d duration=%s",
		totalInserts.Load(), totalErrors.Load(), time.Since(loadStart).Round(time.Second))

	if len(samples) == 0 {
		t.Fatalf("no lag samples collected — replication may have failed")
	}

	maxLag := time.Duration(0)
	for _, s := range samples {
		if s.lag > maxLag {
			maxLag = s.lag
		}
	}
	t.Logf("REPLICATION LAG SUMMARY: %d samples, max=%s, tolerance=%s", len(samples), maxLag, replLagTolerance)

	if maxLag >= replLagTolerance {
		// Diagnostic: dump the lag time-series so the operator can see
		// where the spike occurred.
		csvPath := writeLagCSV(t, samples)
		t.Fatalf("replica lag exceeded tolerance: max=%s tolerance=%s (lag time-series written to %s)", maxLag, replLagTolerance, csvPath)
	}
}

// runReplWriteWorker runs a single write worker at the requested
// inter-insert interval. Errors are counted but not logged per-tick to
// avoid flooding test output during a transient HAProxy backend swap.
func runReplWriteWorker(ctx context.Context, t *testing.T, workerID int, interval time.Duration, inserts, errCount *atomic.Int64) {
	t.Helper()

	gc, err := graph.NewGraphClient(ctx, haproxyPrimaryDSNRepl)
	if err != nil {
		t.Logf("worker %d: NewGraphClient: %v", workerID, err)
		return
	}
	defer gc.Close()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	idx := int64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		idx++

		cctx, ccancel := context.WithTimeout(ctx, 1*time.Second)
		stmt := fmt.Sprintf("CREATE (n:Tag {id: 'repl-w%d-%d-%d'})", workerID, time.Now().UnixNano(), idx)
		err := gc.ExecuteInTenant(cctx, "repl-tenant", func(c context.Context, tx pgx.Tx) error {
			_, qerr := tx.Exec(c, fmt.Sprintf("SELECT * FROM cypher('neksur', $$ %s $$) AS (r ag_catalog.agtype)", stmt))
			return qerr
		})
		ccancel()
		if err != nil {
			errCount.Add(1)
			continue
		}
		inserts.Add(1)
	}
}

// sampleReplayLag queries pg_stat_replication on the primary and returns
// the maximum replay_lag across all visible standbys. If no standbys are
// streaming (e.g., right after a kill — but this test does not kill)
// returns 0 and a nil error.
//
// `replay_lag` is a Postgres `interval` representing the elapsed time
// between the primary writing a WAL record and the standby applying it.
// We extract it as float8 seconds and convert to time.Duration.
func sampleReplayLag(ctx context.Context, conn *pgx.Conn) (time.Duration, error) {
	qctx, qcancel := context.WithTimeout(ctx, 3*time.Second)
	defer qcancel()

	rows, err := conn.Query(qctx, `
		SELECT COALESCE(MAX(EXTRACT(EPOCH FROM replay_lag)::float8), 0.0)
		FROM pg_stat_replication
	`)
	if err != nil {
		return 0, fmt.Errorf("query pg_stat_replication: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, fmt.Errorf("no row from pg_stat_replication aggregate")
	}
	var lagSeconds float64
	if err := rows.Scan(&lagSeconds); err != nil {
		return 0, fmt.Errorf("scan replay_lag: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rows.Err: %w", err)
	}
	return time.Duration(lagSeconds * float64(time.Second)), nil
}

// lagSample captures a single replay-lag measurement at a wall-clock time.
type lagSample struct {
	t   time.Time
	lag time.Duration
}

// writeLagCSV writes the lag time-series to `_replication_lag_<TS>.csv`
// in the package directory and returns the path. Used only on test
// failure to give the operator a diagnostic artifact.
func writeLagCSV(t *testing.T, samples []lagSample) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Logf("writeLagCSV: getwd: %v", err)
		return ""
	}
	path := filepath.Join(wd, fmt.Sprintf("_replication_lag_%d.csv", time.Now().Unix()))
	f, err := os.Create(path)
	if err != nil {
		t.Logf("writeLagCSV: create %s: %v", path, err)
		return ""
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"timestamp", "lag_seconds"})
	for _, s := range samples {
		_ = w.Write([]string{
			s.t.Format(time.RFC3339Nano),
			strconv.FormatFloat(s.lag.Seconds(), 'f', 6, 64),
		})
	}
	return path
}

// startHACluster brings up the docker-compose HA cluster and waits for
// Patroni to settle (1 leader + 2 replicas via REST poll on host port
// 8011 — pg-node-1's exposed REST mapping per docker-compose.ha.yml).
//
// We replicate StartCluster's polling loop locally because the chaos
// package is not importable from the integration package (build tags
// don't compose: chaos tests use `//go:build chaos` and integration
// tests use `//go:build integration`; importing `chaos` here would
// require the chaos package's tag, which is not what we want).
func startHACluster(ctx context.Context, t *testing.T) error {
	t.Helper()
	repoRoot, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("find repo root: %w", err)
	}
	composePath := filepath.Join(repoRoot, "infra", "docker-compose.ha.yml")
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "up", "-d")
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	deadline := time.Now().Add(180 * time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("HA cluster did not settle in 180s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8011/cluster", nil)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := readAndClose(resp)
		if resp.StatusCode != 200 {
			t.Logf("startHACluster poll: status=%d body=%s", resp.StatusCode, body)
			continue
		}
		// Look for "role": "master" in the body — cheap textual check
		// avoids decoding the full payload here. With synchronous_mode=true
		// one follower reports role="sync_standby"; count both forms so we
		// see all >=2 followers in the 3-node cluster (bug #17 — without
		// sync_standby in the counter the settle check times out at 180s).
		hasLeader := strings.Contains(body, `"role": "master"`) || strings.Contains(body, `"role": "leader"`)
		followers := strings.Count(body, `"role": "replica"`) +
			strings.Count(body, `"role": "sync_standby"`) +
			strings.Count(body, `"role": "synchronous_standby"`)
		if hasLeader && followers >= 2 {
			t.Logf("HA cluster settled")
			return nil
		}
	}
}

// stopHACluster runs `docker compose down -v` against the HA compose file.
func stopHACluster(ctx context.Context, t *testing.T) {
	t.Helper()
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Logf("stopHACluster: %v", err)
		return
	}
	composePath := filepath.Join(repoRoot, "infra", "docker-compose.ha.yml")
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "down", "-v")
	if err := cmd.Run(); err != nil {
		t.Logf("docker compose down -v: %v", err)
	}
}

// setupAGEForReplicationTest runs the minimum SQL to make `cypher('neksur', ...)`
// calls resolve against the fresh Patroni-managed cluster:
//
//	CREATE EXTENSION IF NOT EXISTS age;
//	LOAD 'age';
//	SELECT create_graph('neksur') WHERE NOT EXISTS (...);
//
// Mirrors tests/chaos/patroni_chaos.go::setupAGEViaHAProxy but lives here
// because build-tag composition prevents importing the chaos package from
// the integration tier. Idempotent — safe to call on every test run.
//
// Discovers the Patroni leader via REST and execs psql against it (the
// DDL only succeeds on the leader; Patroni would reject CREATE EXTENSION
// on a replica).
func setupAGEForReplicationTest(ctx context.Context, t *testing.T) error {
	t.Helper()
	repoRoot, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("find repo root: %w", err)
	}
	composePath := filepath.Join(repoRoot, "infra", "docker-compose.ha.yml")

	leader, err := findReplicationLeader(ctx)
	if err != nil {
		return fmt.Errorf("find leader: %w", err)
	}

	// AGE functions live in ag_catalog; psql -c does not auto-extend the
	// search_path after CREATE EXTENSION, so qualify create_graph
	// explicitly (bug #18 — without it psql errors with "function
	// create_graph(unknown) does not exist").
	sql := `CREATE EXTENSION IF NOT EXISTS age;
LOAD 'age';
SELECT ag_catalog.create_graph('neksur')
  WHERE NOT EXISTS (SELECT 1 FROM ag_catalog.ag_graph WHERE name = 'neksur');`

	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath,
		"exec", "-T", "-u", "postgres", leader,
		"psql", "-d", "postgres", "-c", sql,
	)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql exec on %s: %w (stderr: %s)", leader, err, strings.TrimSpace(stderr.String()))
	}
	t.Logf("AGE setup OK on leader %s", leader)
	return nil
}

// findReplicationLeader queries Patroni REST and returns the current
// leader's name (master/leader role).
func findReplicationLeader(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8011/cluster", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := readAndClose(resp)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("patroni REST: status %d body=%s", resp.StatusCode, body)
	}
	// Cheap regex-free parse: find each member block and check role.
	// Format: {"name": "pg-node-X", "role": "master", ...}
	// We need: ("name": "pg-node-X") that precedes ("role": "master")
	// within the same member object.
	idx := 0
	for {
		nameStart := strings.Index(body[idx:], `"name": "`)
		if nameStart < 0 {
			break
		}
		nameStart += idx + len(`"name": "`)
		nameEnd := strings.Index(body[nameStart:], `"`)
		if nameEnd < 0 {
			break
		}
		memberName := body[nameStart : nameStart+nameEnd]
		// Find the role within this member's object (until next `}` or end).
		memberEnd := strings.Index(body[nameStart+nameEnd:], "}")
		if memberEnd < 0 {
			break
		}
		memberBlock := body[nameStart+nameEnd : nameStart+nameEnd+memberEnd]
		if strings.Contains(memberBlock, `"role": "master"`) || strings.Contains(memberBlock, `"role": "leader"`) {
			return memberName, nil
		}
		idx = nameStart + nameEnd + memberEnd
	}
	return "", fmt.Errorf("no leader found in Patroni cluster")
}

// findRepoRoot walks up from CWD looking for go.mod.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cur := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(cur, "go.mod")); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", fmt.Errorf("go.mod not found from %s", wd)
}

// readAndClose drains the response body and closes it.
func readAndClose(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
