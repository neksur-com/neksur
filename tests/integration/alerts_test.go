//go:build integration

// Plan 00-05 Wave 4 — synthetic latency → CypherP99LatencyBreach end-to-end alert firing test.
//
// TestP99BreachPages brings up infra/otel/docker-compose.observability.yml,
// drives synthetic slow Cypher queries at 5/sec for 6 minutes (>5min
// observation window required by the PromQL `for: 5m` clause in
// ops/prometheus/alerts/cypher-latency.yaml), then polls the
// AlertManager API at http://localhost:9093/api/v2/alerts every 30s
// asserting that the CypherP99LatencyBreach alert appears in the
// active alerts list within the window.
//
// Cost: 6+ minutes runtime, ~500 MB RAM for the docker-compose stack,
// plus the AGE testcontainer the fixture already brings up. NOT
// suitable for the fast integration tier — gated behind CHAOS=1 env
// var on top of the `//go:build integration` tag, matching the
// replication_test.go convention.
//
// Invocation (nightly CI / manual):
//   CHAOS=1 go test -tags integration -timeout 15m \
//     -run TestP99BreachPages ./tests/integration/...
//
// The test cleans up the docker-compose stack via t.Cleanup regardless
// of pass / fail outcome.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/graph"
)

const (
	composeFile         = "../../infra/otel/docker-compose.observability.yml"
	alertManagerURL     = "http://localhost:9093/api/v2/alerts"
	targetAlertName     = "CypherP99LatencyBreach"
	observationWindow   = 8 * time.Minute  // PromQL `for: 5m` + margin
	pollInterval        = 30 * time.Second
	slowQueryInterval   = 200 * time.Millisecond // 5 calls/sec
	syntheticSlowQuery  = `MATCH (n) RETURN n, pg_sleep(2.5) LIMIT 1`
	stackUpReadyTimeout = 90 * time.Second
)

// alertManagerAlert is a minimal projection of the AlertManager v2
// /api/v2/alerts response. The full schema is documented at
// https://github.com/prometheus/alertmanager/blob/main/api/v2/openapi.yaml
// but we only need the labels for the assertion.
type alertManagerAlert struct {
	Labels map[string]string `json:"labels"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

// TestP99BreachPages exercises the synthetic-latency → CypherP99LatencyBreach
// → AlertManager firing path end-to-end. Skipped unless CHAOS=1.
func TestP99BreachPages(t *testing.T) {
	if os.Getenv("CHAOS") == "" {
		t.Skip("CHAOS=1 required — TestP99BreachPages spins up the observability stack (6+ minute runtime)")
	}
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — this test requires Docker for both the AGE fixture and the observability stack")
	}

	// Bring up the observability stack. Done up-front so the AGE fixture
	// container (started by main_test.go's TestMain) and the
	// docker-compose stack can co-exist.
	upCmd := exec.Command("docker", "compose", "-f", composeFile, "up", "-d")
	upOut, err := upCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, upOut)
	}
	t.Cleanup(func() {
		downCmd := exec.Command("docker", "compose", "-f", composeFile, "down", "-v")
		downOut, downErr := downCmd.CombinedOutput()
		if downErr != nil {
			t.Logf("docker compose down (non-fatal in cleanup): %v\n%s", downErr, downOut)
		}
	})

	// Wait for AlertManager to be ready before driving load.
	if err := waitForAlertManager(t, stackUpReadyTimeout); err != nil {
		t.Fatalf("AlertManager not ready: %v", err)
	}

	gc, err := graph.NewGraphClient(fix.ctx, fix.container.SuperuserDSN)
	if err != nil {
		t.Fatalf("NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Start synthetic-load goroutine.
	loadCtx, loadCancel := context.WithCancel(fix.ctx)
	defer loadCancel()
	go driveSyntheticLoad(t, loadCtx, gc)

	// Poll AlertManager for CypherP99LatencyBreach.
	deadline := time.Now().Add(observationWindow)
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	for time.Now().Before(deadline) {
		alerts, err := fetchActiveAlerts(loadCtx)
		if err != nil {
			t.Logf("alertmanager fetch (will retry): %v", err)
		} else {
			for _, a := range alerts {
				if a.Labels["alertname"] == targetAlertName && a.Status.State == "active" {
					t.Logf("PASS — %s observed in AlertManager %s after %v",
						targetAlertName, a.Status.State, time.Since(deadline.Add(-observationWindow)))
					return
				}
			}
		}
		select {
		case <-pollTicker.C:
		case <-loadCtx.Done():
			t.Fatalf("load context cancelled before alert observed: %v", loadCtx.Err())
		}
	}
	t.Fatalf("FAIL — %s did not fire in AlertManager within %v", targetAlertName, observationWindow)
}

// driveSyntheticLoad runs slow queries at slowQueryInterval until ctx
// is cancelled. Logs errors but does NOT t.Fatal — a single slow
// query failing should not abort the whole observation window.
func driveSyntheticLoad(t *testing.T, ctx context.Context, gc *graph.GraphClient) {
	t.Helper()
	tick := time.NewTicker(slowQueryInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			rows, err := graph.ExecuteCypher(ctx, gc, "neksur", syntheticSlowQuery)
			if err != nil {
				t.Logf("synthetic slow query (non-fatal): %v", err)
				continue
			}
			drainRows(t, rows)
		}
	}
}

// waitForAlertManager polls AlertManager's /-/healthy endpoint until
// it returns 200 or timeout elapses.
func waitForAlertManager(t *testing.T, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://localhost:9093/-/healthy")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("alertmanager /-/healthy not 200 within %v", timeout)
}

// fetchActiveAlerts hits AlertManager's v2 /api/v2/alerts endpoint and
// unmarshals the array projection.
func fetchActiveAlerts(ctx context.Context) ([]alertManagerAlert, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", alertManagerURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("alertmanager status %d: %s", resp.StatusCode, string(body))
	}
	var alerts []alertManagerAlert
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return nil, fmt.Errorf("decode alerts: %w", err)
	}
	return alerts, nil
}
