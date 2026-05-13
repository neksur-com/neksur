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
	"net/http"
	"os"
	"os/exec"
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
func composeFile() string {
	if v := os.Getenv("COMPOSE_FILE"); v != "" {
		return v
	}
	return "infra/docker-compose.ha.yml"
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

// StartCluster brings up the docker-compose HA cluster and polls Patroni's
// REST API until exactly 1 leader and >=2 replicas are visible. Returns
// the leader's name.
//
// Cold-start budget: bringing the cluster from scratch involves pulling
// quay.io/coreos/etcd:v3.5.13 (~30 MB), building neksur/postgres-age:phase0
// from infra/postgres/Dockerfile (~600 MB layered on apache/age — but
// cached after first pull), and running each Patroni container's lazy
// `pip install patroni[etcd3]` (one-time ~20-40s per node on a cold
// network). Subsequent runs see all images cached and complete in <60s.
//
// We poll for up to 180s (the cold-start ceiling) with 2s gaps; warm
// runs return well under 60s.
func StartCluster(ctx context.Context) (string, error) {
	// `up -d` is idempotent — if some containers are already running, it
	// only starts the missing ones. For tests using t.Cleanup → StopCluster
	// this matters only if a previous run died mid-teardown.
	if err := runCompose(ctx, "up", "-d"); err != nil {
		return "", fmt.Errorf("chaos: docker compose up: %w", err)
	}

	baseURL := fmt.Sprintf("http://localhost:%s", patroniRESTPort())
	deadline := time.Now().Add(180 * time.Second)
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
			return "", fmt.Errorf("chaos: StartCluster: timeout waiting for cluster to settle (last err: %v)", lastErr)
		}

		cl, err := fetchCluster(ctx, baseURL)
		if err != nil {
			lastErr = err
			continue
		}
		leader := findLeader(cl)
		replicas := countReplicas(cl)
		if leader != "" && replicas >= 2 {
			return leader, nil
		}
		lastErr = fmt.Errorf("not yet settled: leader=%q replicas=%d members=%d", leader, replicas, len(cl.Members))
	}
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
