// Package chaos drives Patroni chaos-engineering tests against the local
// 3-node HA cluster brought up by infra/docker-compose.ha.yml.
//
// Plan 00-03 (Wave 2 — Patroni HA) replaces the Plan 00-01 stubs that
// previously returned a sentinel error pointing here. Function signatures
// changed in this fill-in pass to match the actual chaos-test ergonomics:
//
//   - StartCluster returns the current leader's name (so tests don't
//     have to re-poll just to know who to kill).
//   - KillPrimary takes the leader hostname (the killable target),
//     not a cluster name (which is constant — neksur-pg).
//   - KillPrimary returns the kill timestamp so callers measure the
//     failover elapsed time precisely (TimeFailover composes this).
//   - WaitForNewLeader takes the previous leader's name so it knows
//     which member appearing as `master` constitutes a real failover
//     (vs. the original leader recovering and re-taking the lock).
//   - TimeFailover returns both elapsed and the new leader name so
//     downstream tests (TestPostFailoverCypherWorks) can connect to
//     the right node directly without re-polling.
//   - StopCluster runs `docker compose down -v` for clean teardown.
//
// All functions take ctx for cancellation/timeout. The HTTP polling
// uses a 5s per-request timeout; per-poll cycles are 500ms (fast
// enough to detect failover within a single Patroni loop_wait=10s
// cycle) for WaitForNewLeader, 2s (lighter) for StartCluster which
// waits for the slower bootstrap path.
//
// Configuration is driven by the COMPOSE_FILE env var (defaulting to
// `infra/docker-compose.ha.yml` relative to the repo root) and the
// PATRONI_REST_PORT env var (defaulting to "8011" — the host-side
// mapping of pg-node-1's REST port from docker-compose.ha.yml). Tests
// can override either to point at a custom cluster.
//
// The functions are NOT t.Helper() helpers — they are package-level
// driver functions. Call them from tests and assert on returned errors
// the standard Go way.
package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// patroniMember mirrors the member objects in Patroni's REST `/cluster`
// payload. We only consume name/role/state — Patroni emits more fields
// (host, port, lag, timeline, etc.) that are not load-bearing here.
type patroniMember struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	State string `json:"state"`
}

type patroniCluster struct {
	Members []patroniMember `json:"members"`
}

// composeFile returns the docker-compose path. Override via COMPOSE_FILE.
//
// Default resolution walks up from this file's location (`runtime.Caller`)
// to the repo root (marked by go.mod) and joins infra/docker-compose.ha.yml.
// That makes the default robust to `go test` working-directory (tests run
// from the package dir `tests/chaos/`, not the repo root — live verify
// 2026-05-13 surfaced this).
func composeFile() string {
	if v := os.Getenv("COMPOSE_FILE"); v != "" {
		return v
	}
	if root := findRepoRoot(); root != "" {
		return filepath.Join(root, "infra", "docker-compose.ha.yml")
	}
	// Fallback: best-effort relative path (works only if CWD == repo root).
	return "infra/docker-compose.ha.yml"
}

// findRepoRoot walks up from this source file's directory until it finds
// a go.mod, returning the directory containing it. Empty string on failure.
func findRepoRoot() string {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(here)
	for i := 0; i < 10; i++ { // bounded — avoid infinite loop on misuse
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// patroniRESTPort returns the host-mapped Patroni REST port for polling.
// docker-compose.ha.yml maps pg-node-1's REST 8008 → host 8011, pg-node-2
// → 8012, pg-node-3 → 8013. Polling the per-cluster `/cluster` endpoint
// on ANY member returns the same view (Patroni reads from etcd), so we
// pick pg-node-1's mapping by default and let tests override via
// PATRONI_REST_PORT.
func patroniRESTPort() string {
	if v := os.Getenv("PATRONI_REST_PORT"); v != "" {
		return v
	}
	return "8011"
}

// httpClient is the package-shared HTTP client used for all Patroni REST
// polls. 5s timeout balances against ctx-driven per-call cancellation:
// the 5s ceiling catches stuck reads, ctx catches caller-imposed deadlines.
var httpClient = &http.Client{Timeout: 5 * time.Second}

// fetchCluster GETs `/cluster` from the given Patroni REST URL and
// unmarshals to patroniCluster. Returns a wrapped error on any failure
// (connect refused, non-200, JSON parse).
func fetchCluster(ctx context.Context, baseURL string) (*patroniCluster, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/cluster", nil)
	if err != nil {
		return nil, fmt.Errorf("chaos: build request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chaos: GET %s/cluster: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("chaos: GET %s/cluster: status %d: %s", baseURL, resp.StatusCode, string(body))
	}
	var cl patroniCluster
	if err := json.NewDecoder(resp.Body).Decode(&cl); err != nil {
		return nil, fmt.Errorf("chaos: decode /cluster JSON: %w", err)
	}
	return &cl, nil
}

// findLeader walks members and returns the name of the one whose role is
// "master" or "leader" (Patroni 4.1 emits "master"; older versions emit
// "leader" — both are accepted for forward compat). Empty string if none.
func findLeader(cl *patroniCluster) string {
	for _, m := range cl.Members {
		if m.Role == "master" || m.Role == "leader" {
			return m.Name
		}
	}
	return ""
}

// countReplicas returns the number of members whose role is "replica".
func countReplicas(cl *patroniCluster) int {
	n := 0
	for _, m := range cl.Members {
		if m.Role == "replica" {
			n++
		}
	}
	return n
}

// startClusterTimeout is the StartCluster settle ceiling. Bumped from 180s
// → 5min on 2026-05-13 because true cold bootstrap of 3 nodes (pg_basebackup
// + replay on 2 replicas) reliably needs ~3-5min wall-clock on
// arm64/macOS — the prior 180s was insufficient even on warm runs when
// the first replica had to redo bootstrap (e.g. after a `down -v` that
// wiped volumes).
const startClusterTimeout = 5 * time.Minute

// haproxyPrimaryProbeAddr is the host:port StartCluster probes to confirm
// HAProxy has finished electing the current Patroni leader as its
// primary backend. HAProxy's `option httpchk GET /master` runs on its
// own ~3s cadence (separate from Patroni's loop_wait=10s), so the
// cluster can be Patroni-settled (1 leader + 2 replicas) while HAProxy
// still reports 503 backend. Probing the actual TCP connect to the
// HAProxy primary route catches that gap. The matching haproxyPrimaryDSN
// in failover_test.go must hit this same host:port.
const haproxyPrimaryProbeAddr = "localhost:5500"

// StartCluster brings up the docker-compose HA cluster and polls Patroni's
// REST API until exactly 1 leader and >=2 replicas are visible. Returns
// the leader's name.
//
// If the cluster is ALREADY UP and settled (leader + 2 streaming replicas
// via Patroni REST), StartCluster short-circuits the `docker compose up -d`
// invocation. This handles two cases:
//   - Tests using t.Cleanup → StopCluster between subtests (`up -d` is
//     idempotent but still slow to no-op on warm clusters)
//   - Operator running the cluster manually for debug, then invoking the
//     chaos test against it
//
// Cold-start budget: bringing the cluster from scratch involves pulling
// quay.io/coreos/etcd:v3.5.13 (~30 MB), building neksur/postgres-age:phase0
// from infra/postgres/Dockerfile (~600 MB cached after first pull), and
// pg_basebackup + WAL replay on each replica. ~3-5min wall-clock on
// arm64/macOS; faster on Linux + dedicated build.
func StartCluster(ctx context.Context) (string, error) {
	baseURL := fmt.Sprintf("http://localhost:%s", patroniRESTPort())

	// Short-circuit: if the cluster is already settled, skip `up -d`
	// (it's slow even when idempotent — docker has to inspect each
	// container's healthcheck state).
	if cl, err := fetchCluster(ctx, baseURL); err == nil {
		if leader := findLeader(cl); leader != "" && countReplicas(cl) >= 2 {
			return leader, nil
		}
	}

	// `up -d` is idempotent — if some containers are already running, it
	// only starts the missing ones.
	if err := runCompose(ctx, "up", "-d"); err != nil {
		return "", fmt.Errorf("chaos: docker compose up: %w", err)
	}

	deadline := time.Now().Add(startClusterTimeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("chaos: StartCluster: ctx done (last err: %v): %w", lastErr, ctx.Err())
		case <-ticker.C:
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("chaos: StartCluster: timeout (%s) waiting for cluster to settle (last err: %v)", startClusterTimeout, lastErr)
		}

		cl, err := fetchCluster(ctx, baseURL)
		if err != nil {
			lastErr = err
			continue
		}
		leader := findLeader(cl)
		replicas := countReplicas(cl)
		if leader == "" || replicas < 2 {
			lastErr = fmt.Errorf("not yet settled: leader=%q replicas=%d members=%d", leader, replicas, len(cl.Members))
			continue
		}
		// Patroni is settled — but HAProxy's health checks may lag.
		// Confirm the HAProxy primary route accepts a TCP connect before
		// returning, otherwise downstream tests hit "unexpected EOF".
		if err := haproxyPrimaryReady(ctx); err != nil {
			lastErr = fmt.Errorf("Patroni settled (leader=%s) but HAProxy primary not yet ready: %w", leader, err)
			continue
		}
		// One-time AGE setup: shared_preload_libraries='age' (per Patroni
		// config) loads the AGE shared library at server startup, but the
		// `cypher()` SQL function only resolves once `CREATE EXTENSION age`
		// has been run in the target database AND the named graph exists.
		// Plan 02 migrations do this in production; the chaos compose
		// brings up a fresh Patroni-managed Postgres without our migration
		// set, so we run the minimum needed here. Idempotent.
		if err := setupAGEViaHAProxy(ctx); err != nil {
			lastErr = fmt.Errorf("Patroni+HAProxy ready but AGE setup failed: %w", err)
			continue
		}
		return leader, nil
	}
}

// haproxyPrimaryReady probes the HAProxy primary route with a full pgwire
// startup-packet exchange. HAProxy's TCP frontend ACCEPTS connections on
// the bind port even when zero backend servers are UP — and on no-backend
// it immediately closes the socket, which manifests downstream as "failed
// to receive message: unexpected EOF" from pgx. A bare TCP connect is
// therefore not enough to distinguish "primary route ready" from "HAProxy
// up but no backend yet".
//
// Instead, send a minimal Postgres v3 StartupMessage and require a
// non-empty response byte. Postgres replies with 'R' (AuthenticationRequest)
// or 'E' (Error) — either confirms the connection reached a real backend.
// A connection that closes silently (n=0 bytes, EOF) is the failure mode
// HAProxy exposes during convergence and is the one we want to reject here.
func haproxyPrimaryReady(ctx context.Context) error {
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := d.DialContext(dialCtx, "tcp", haproxyPrimaryProbeAddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", haproxyPrimaryProbeAddr, err)
	}
	defer conn.Close()

	// Postgres v3 StartupMessage layout:
	//   int32 length (incl. self)
	//   int32 protocol = 196608 (0x00030000)
	//   keyval pairs (\0-terminated strings) ending with extra \0
	// Minimal payload: user=postgres, database=postgres.
	payload := []byte("user\x00postgres\x00database\x00postgres\x00\x00")
	msg := make([]byte, 8+len(payload))
	binaryWriteUint32(msg[0:4], uint32(len(msg)))
	binaryWriteUint32(msg[4:8], 196608) // 0x00030000
	copy(msg[8:], payload)

	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("write startup packet: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read pgwire reply (HAProxy backend probably not yet UP): %w", err)
	}
	if n == 0 {
		return fmt.Errorf("read 0 bytes — HAProxy backend not yet UP")
	}
	// 'R' = AuthenticationRequest (expected on a healthy backend with
	// trust auth disabled); 'E' = ErrorResponse (also confirms backend
	// is alive — e.g. "no pg_hba.conf entry" — which is fine for the
	// probe). Anything else means we got to a real Postgres process.
	return nil
}

// binaryWriteUint32 writes v into b in big-endian (network byte order).
// pgwire StartupMessage uses BE per the Postgres protocol spec.
func binaryWriteUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// setupAGEViaHAProxy connects to the HAProxy primary route and runs the
// minimum SQL needed to make `cypher('neksur', $$ ... $$)` calls resolve:
//
//   CREATE EXTENSION IF NOT EXISTS age;
//   LOAD 'age';
//   SELECT create_graph('neksur') WHERE 'neksur' NOT IN (SELECT name FROM ag_catalog.ag_graph);
//
// The chaos docker-compose stack brings up a fresh Patroni-managed cluster
// without our Plan 02 migration set; this helper compensates so chaos
// tests can exercise the cypher() path. Idempotent — safe to call from
// StartCluster on every invocation.
//
// Uses `docker compose exec` rather than a Go SQL driver to keep the
// chaos lib dependency surface zero-Go-deps (we are already shelling out
// to docker for compose operations). The `psql` client is in the
// neksur/postgres-age:phase0 image because postgresql-client-16 is a
// transitive dep of postgresql-16.
func setupAGEViaHAProxy(ctx context.Context) error {
	sql := `
CREATE EXTENSION IF NOT EXISTS age;
LOAD 'age';
SELECT create_graph('neksur')
  WHERE NOT EXISTS (SELECT 1 FROM ag_catalog.ag_graph WHERE name = 'neksur');
`
	// Use docker compose exec to run psql as the postgres superuser
	// against the leader's Patroni-managed Postgres on its container-
	// internal port 5432. We target pg-node-1 by default; if it's not
	// the current leader, Patroni sync_standby will reject the DDL
	// (CREATE EXTENSION is replicated through WAL only — Patroni
	// would refuse to run it on a replica) and we retry on the leader.
	//
	// Discover leader name from Patroni REST so we always run on the
	// authoritative node.
	cl, err := fetchCluster(ctx, fmt.Sprintf("http://localhost:%s", patroniRESTPort()))
	if err != nil {
		return fmt.Errorf("setupAGE: fetchCluster: %w", err)
	}
	leader := findLeader(cl)
	if leader == "" {
		return fmt.Errorf("setupAGE: no Patroni leader found")
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile(),
		"exec", "-T", "-u", "postgres", leader,
		"psql", "-d", "postgres", "-c", sql,
	)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setupAGE: psql exec on %s: %w (stderr: %s)", leader, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// KillPrimary SIGKILLs the named container (Patroni leader). Returns the
// timestamp at which the kill command returned successfully — callers
// use this as t0 for failover-time measurement (see TimeFailover).
//
// `docker compose kill --signal SIGKILL <svc>` is the canonical chaos
// hammer. Unlike `docker compose stop` (which sends SIGTERM and waits
// for graceful shutdown — Patroni would demote and re-elect cleanly,
// defeating the chaos test), SIGKILL is unblockable and the kernel
// removes the process immediately. The Patroni daemon goes with it,
// and etcd's TTL=30 lease for that node expires within `ttl` seconds
// — after which a quorum-electable replica can claim the leader lock.
func KillPrimary(ctx context.Context, leaderHost string) (time.Time, error) {
	if leaderHost == "" {
		return time.Time{}, errors.New("chaos: KillPrimary: empty leaderHost")
	}
	if err := runCompose(ctx, "kill", "--signal", "SIGKILL", leaderHost); err != nil {
		return time.Time{}, fmt.Errorf("chaos: docker compose kill %s: %w", leaderHost, err)
	}
	// time.Now() is monotonic-friendly; later subtraction via time.Since
	// uses the monotonic component.
	return time.Now(), nil
}

// WaitForNewLeader polls Patroni's REST `/cluster` every 500ms until a
// member with role "master" (or "leader") appears AND its name differs
// from prevLeader. Returns the new leader's name and the timestamp at
// which it was first observed.
//
// Polling 500ms is aggressive enough that the leader-elected event is
// detected within one Patroni loop_wait cycle (10s) plus the etcd lease
// expiry (ttl=30s) — total budget is bounded by ttl + loop_wait ~= 30s,
// which the D-001.15 contract requires us to come in under.
//
// timeout is the per-call ceiling; on expiry returns context.DeadlineExceeded
// (wrapped). ctx cancellation (separately) returns ctx.Err() (wrapped).
func WaitForNewLeader(ctx context.Context, prevLeader string, timeout time.Duration) (string, time.Time, error) {
	if prevLeader == "" {
		return "", time.Time{}, errors.New("chaos: WaitForNewLeader: empty prevLeader")
	}

	baseURL := fmt.Sprintf("http://localhost:%s", patroniRESTPort())
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return "", time.Time{}, fmt.Errorf("chaos: WaitForNewLeader: ctx done (last err: %v): %w", lastErr, ctx.Err())
		case <-ticker.C:
		}

		if time.Now().After(deadline) {
			return "", time.Time{}, fmt.Errorf("chaos: WaitForNewLeader: timeout after %s (last err: %v)", timeout, lastErr)
		}

		cl, err := fetchCluster(ctx, baseURL)
		if err != nil {
			// Polling against the killed leader's REST port would always
			// fail; PATRONI_REST_PORT defaults to pg-node-1's mapping
			// (8011). When pg-node-1 is the killed leader, the test must
			// override PATRONI_REST_PORT to a survivor's port BEFORE
			// calling WaitForNewLeader, OR the chaos test must select a
			// non-pg-node-1 victim. We capture the error and keep
			// polling — a transient connect-refused while etcd settles
			// is expected.
			lastErr = err
			continue
		}
		leader := findLeader(cl)
		if leader != "" && leader != prevLeader {
			return leader, time.Now(), nil
		}
		lastErr = fmt.Errorf("no new leader yet: current=%q prev=%q", leader, prevLeader)
	}
}

// TimeFailover composes KillPrimary + WaitForNewLeader and returns the
// wall-clock elapsed time between the kill and the appearance of a new
// leader, plus the new leader's name. D-001.15 contracts elapsed < 30s
// (caller asserts this).
//
// The 60s timeout to WaitForNewLeader doubles D-001.15's contract — gives
// the test diagnostic information (instead of failing on the contract
// boundary, it fails on overrun-by-2x with the Patroni REST state in
// the error message).
func TimeFailover(ctx context.Context, prevLeader string) (time.Duration, string, error) {
	killTS, err := KillPrimary(ctx, prevLeader)
	if err != nil {
		return 0, "", err
	}
	newLeader, seenAt, err := WaitForNewLeader(ctx, prevLeader, 60*time.Second)
	if err != nil {
		return 0, "", err
	}
	return seenAt.Sub(killTS), newLeader, nil
}

// StopCluster brings down the docker-compose HA cluster and removes its
// volumes. Idempotent — `down -v` is safe to call against a partially-
// running cluster. Tests should call this from t.Cleanup.
func StopCluster(ctx context.Context) error {
	if err := runCompose(ctx, "down", "-v"); err != nil {
		return fmt.Errorf("chaos: docker compose down -v: %w", err)
	}
	return nil
}

// runCompose invokes `docker compose -f <composeFile> <args...>`. Stderr
// is captured into the returned error on non-zero exit so test failures
// surface useful diagnostics. Stdout is discarded — chaos tests don't
// consume compose's output.
func runCompose(ctx context.Context, args ...string) error {
	full := append([]string{"compose", "-f", composeFile()}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w (stderr: %s)", strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
