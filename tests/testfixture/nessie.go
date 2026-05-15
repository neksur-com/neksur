package testfixture

// nessie.go — Project Nessie 0.100 testcontainer wrapper.
//
// Plan 01-01 introduces Nessie alongside Polaris because Nessie is the
// catalog with the MOST divergent model (branches), so an adapter that
// works against Nessie almost certainly works against Glue / Unity
// (D-1.02 in PLAN 01-CONTEXT.md). Plan 01-03 builds the Nessie adapter
// on this fixture.
//
// Pitfall 2 (01-RESEARCH.md): parallel test runs that all write to
// `main` race. We mitigate by auto-creating a dedicated `neksur-test`
// branch on container startup (CONTEXT line 173); per-test subroutines
// can fork additional sub-branches for further isolation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NessieImage is the canonical Project Nessie image for Phase 1 — pinned
// at 0.100.0 (D-1.02 reference; Nessie's API v2 is stable from 0.80+).
const NessieImage = "projectnessie/nessie:0.100.0"

// nessieHTTPPort is the REST API port inside the Nessie container.
const nessieHTTPPort = "19120/tcp"

// DefaultNessieBranch is the auto-created branch used by Phase 1 tests
// to avoid Pitfall 2 (parallel writes to `main`).
const DefaultNessieBranch = "neksur-test"

// NessieContainer wraps a running Nessie instance with branch helpers
// for the Plan 01-03 adapter tests.
type NessieContainer struct {
	Container testcontainers.Container
	Endpoint  string // e.g., "http://127.0.0.1:53251" — base for /api/v2/...
	Branch    string // default "neksur-test"
}

// StartNessie spins up a Nessie container, waits for /api/v2/config to
// return 200, then forks the `neksur-test` branch off `main`. Total
// cold start is ~15-30s on a warm-image laptop, ~45-90s on a cold-image
// CI runner.
func StartNessie(ctx context.Context) (*NessieContainer, error) {
	req := testcontainers.ContainerRequest{
		Image:        NessieImage,
		ExposedPorts: []string{nessieHTTPPort},
		// Nessie defaults to in-memory storage out of the box, which is
		// what Phase 1 tests want (per-container clean slate).
		WaitingFor: wait.ForHTTP("/api/v2/config").
			WithPort(nessieHTTPPort).
			WithStartupTimeout(120 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start nessie: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: nessie host: %w", err)
	}
	port, err := c.MappedPort(ctx, nessieHTTPPort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: nessie port: %w", err)
	}
	n := &NessieContainer{
		Container: c,
		Endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
		Branch:    DefaultNessieBranch,
	}
	if err := n.CreateBranch(ctx, DefaultNessieBranch); err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: nessie default branch: %w", err)
	}
	return n, nil
}

// Terminate shuts down the Nessie container. Safe to call multiple times.
func (n *NessieContainer) Terminate(ctx context.Context) error {
	if n == nil || n.Container == nil {
		return nil
	}
	return n.Container.Terminate(ctx)
}

// CreateBranch forks a new branch off `main` at its current HEAD. The
// function is idempotent — a 409 response (branch exists) is treated
// as success. Tests that want sub-branches per t.Name can call this
// directly with an arbitrary name.
//
// Uses Nessie's REST v2: POST /api/v2/trees with body
// `{"type":"BRANCH","name":"<branch>"}`. The `sourceRefName` /
// `sourceHash` defaults to main / head when omitted.
func (n *NessieContainer) CreateBranch(ctx context.Context, name string) error {
	body, _ := json.Marshal(map[string]any{
		"type": "BRANCH",
		"name": name,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", n.Endpoint+"/api/v2/trees", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("nessie create_branch: build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("nessie create_branch: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
		return nil
	}
	rb, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("nessie create_branch(%s): status %d body %s", name, resp.StatusCode, string(rb))
}
