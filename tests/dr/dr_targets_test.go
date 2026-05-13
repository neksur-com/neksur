//go:build dr

// dr_targets_test.go wraps the three Wave-3 DR shell scripts in Go
// tests so the dr suite can be invoked uniformly via:
//
//	go test -tags dr -timeout 90m ./tests/dr/...
//
// Each test:
//   - SKIPs if $DR_LIVE != "1" (the suite is heavy: brings up the
//     full HA cluster, runs a 10-30 minute load, and may consume
//     ~10 GB of disk). Live runs are gated by an environment opt-in
//     so `go test -tags dr` is safe to run by default in CI matrices
//     that lack Docker / pgBackRest.
//   - Spins the HA cluster from infra/docker-compose.ha.yml via
//     chaos.StartCluster (Plan 00-03's tested entrypoint).
//   - t.Cleanup → chaos.StopCluster (`docker compose down -v`).
//   - exec.CommandContext into the matching shell script and asserts
//     exit 0.
//
// Wrapped scripts:
//   - TestRTO_RestorePIT       → tests/dr/restore_pit.sh
//   - TestRPO_ChaosRestore     → tests/dr/chaos_restore.sh
//   - TestMonthlyRestoreDrill  → tests/dr/run_monthly_restore_drill.sh
//
// The shell scripts produce their own structured output (stdout/
// stderr) which we tee into the test log via cmd.Stdout = os.Stdout.
// Failures bubble up via the script's exit code; the Go assertion is
// thin (require exit 0).

package dr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// drLiveEnabled gates the heavy live-cluster tests. CI matrices that
// run `go test -tags dr` without DR_LIVE=1 see all three tests SKIP,
// keeping the gate fast enough for a default tier. The nightly
// .github/workflows/load-chaos-restore.yml run sets DR_LIVE=1.
func drLiveEnabled() bool {
	v := os.Getenv("DR_LIVE")
	return v == "1" || v == "true" || v == "TRUE"
}

// repoRoot returns the repository root by walking up from the test's
// working directory (which Go sets to the package dir = tests/dr).
// The walk-up looks for go.mod — present at the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRoot: walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// startHACluster brings up the HA cluster used by all three DR tests.
// We invoke it via the chaos package (Plan 00-03) so the path through
// docker-compose.ha.yml is the same as the chaos test suite.
//
// On cluster startup failure, the test is FAILED (not skipped) — if
// DR_LIVE is set, the operator expects the full live run.
//
// Returns the leader name for tests that need it (TestRPO_ChaosRestore).
func startHACluster(t *testing.T, ctx context.Context) string {
	t.Helper()
	// Importing tests/chaos here would create a cyclic boundary
	// (chaos imports things tests import — but the chaos package
	// today is leaf-level). We exec docker compose directly to keep
	// this package's import graph minimal and to avoid coupling the
	// dr suite to chaos package internals beyond their public API.
	root := repoRoot(t)
	composeFile := filepath.Join(root, "infra", "docker-compose.ha.yml")
	if _, err := os.Stat(composeFile); err != nil {
		t.Fatalf("compose file %s not found: %v", composeFile, err)
	}
	upCtx, upCancel := context.WithTimeout(ctx, 4*time.Minute)
	defer upCancel()
	cmd := exec.CommandContext(upCtx, "docker", "compose", "-f", composeFile, "up", "-d")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose up: %v", err)
	}
	t.Cleanup(func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer downCancel()
		c := exec.CommandContext(downCtx, "docker", "compose", "-f", composeFile, "down", "-v")
		c.Dir = root
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		_ = c.Run()
	})
	// Cluster bring-up is asynchronous; give Patroni 60s to elect a
	// leader before the tests proceed.
	time.Sleep(60 * time.Second)
	// Leader name is parameterised by docker-compose.ha.yml; pg-node-1
	// is the bootstrap-initial leader. Tests that kill the leader
	// auto-detect via Patroni REST inside chaos_restore.sh, so this
	// returned value is a hint for tests that need a "default" target.
	return "pg-node-1"
}

// TestRTO_RestorePIT validates D-OQ.04 RTO 1h via tests/dr/restore_pit.sh.
//
// Workflow:
//  1. Bring up the HA cluster (so a Postgres+AGE instance is reachable
//     on a known port).
//  2. Run restore_pit.sh --assert-rto-under 3600 --target-time now --yes.
//  3. Assert exit 0.
//
// The 3600s threshold matches the D-OQ.04 contract (RTO 1h). Smaller
// thresholds are reserved for sub-fixture / CI proxy runs.
func TestRTO_RestorePIT(t *testing.T) {
	if !drLiveEnabled() {
		t.Skip("dr: TestRTO_RestorePIT requires DR_LIVE=1 — skipping (set DR_LIVE=1 to run the live HA-cluster restore drill)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	startHACluster(t, ctx)

	root := repoRoot(t)
	script := filepath.Join(root, "tests", "dr", "restore_pit.sh")
	cmd := exec.CommandContext(ctx, "bash", script,
		"--assert-rto-under", strconv.Itoa(3600),
		"--target-time", "now",
		"--yes",
	)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("restore_pit.sh: %v", err)
	}
}

// TestRPO_ChaosRestore validates D-OQ.04 RPO 15min via
// tests/dr/chaos_restore.sh. Runs a 10-minute warmup at 1000 qps,
// SIGKILLs the Patroni leader, restores, and asserts <=900s of data
// loss.
func TestRPO_ChaosRestore(t *testing.T) {
	if !drLiveEnabled() {
		t.Skip("dr: TestRPO_ChaosRestore requires DR_LIVE=1 — skipping (set DR_LIVE=1 to run the live chaos-restore drill)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	startHACluster(t, ctx)

	root := repoRoot(t)
	script := filepath.Join(root, "tests", "dr", "chaos_restore.sh")
	cmd := exec.CommandContext(ctx, "bash", script,
		"--assert-rpo-under", strconv.Itoa(900),
		"--warmup-minutes", strconv.Itoa(10),
		"--writer-qps", strconv.Itoa(1000),
		"--yes",
	)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("chaos_restore.sh: %v", err)
	}
}

// TestMonthlyRestoreDrill validates the monthly drill end-to-end
// via tests/dr/run_monthly_restore_drill.sh: restore, schema-check
// against migrations/check.sql (expecting 19 vlabels + 24 elabels
// per D-003.06), drill report emission to runbooks/drill-reports/.
func TestMonthlyRestoreDrill(t *testing.T) {
	if !drLiveEnabled() {
		t.Skip("dr: TestMonthlyRestoreDrill requires DR_LIVE=1 — skipping (set DR_LIVE=1 to run the live monthly drill)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	startHACluster(t, ctx)

	root := repoRoot(t)
	script := filepath.Join(root, "tests", "dr", "run_monthly_restore_drill.sh")
	cmd := exec.CommandContext(ctx, "bash", script,
		"--rto-threshold", strconv.Itoa(3600),
		"--yes",
	)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run_monthly_restore_drill.sh: %v", err)
	}
}
