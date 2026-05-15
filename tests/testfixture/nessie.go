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
//
// Deviation note (Plan 01-01 Task 3 Rule 1): the plan referenced
// `projectnessie/nessie:0.100.0` (Docker Hub) but Project Nessie
// stopped publishing to Docker Hub after 0.76.6 and moved newer
// releases to ghcr.io. Image identity is unchanged; only the registry
// prefix differs.
const NessieImage = "ghcr.io/projectnessie/nessie:0.100.0"

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
// function is idempotent — a 409 response (branch already exists) is
// treated as success. Tests that want sub-branches per t.Name can
// call this directly with an arbitrary name.
//
// Nessie's REST v2 create-reference shape requires a two-step approach
// because the API splits the new-ref identity (query params) from the
// source-ref identity (body):
//
//	1. GET /api/v2/trees/main          → extract main's current hash
//	2. POST /api/v2/trees?type=BRANCH&name=<newname>
//	   with body {"type":"BRANCH","name":"main","hash":"<mainHash>"}
//
// The body is the SOURCE reference (where the new branch forks from),
// NOT the new branch's identity. This caught the Plan 01-01 BLOCKING
// run by surprise; the old single-step shape returned 400 with
// "createReference.type: must not be null". The split contract is
// documented in Nessie's OpenAPI but easy to miss because the body
// looks like it should describe the new branch.
func (n *NessieContainer) CreateBranch(ctx context.Context, name string) error {
	// (1) Read main's current head hash.
	mainReq, err := http.NewRequestWithContext(ctx, "GET", n.Endpoint+"/api/v2/trees/main", nil)
	if err != nil {
		return fmt.Errorf("nessie create_branch: build main req: %w", err)
	}
	mainResp, err := http.DefaultClient.Do(mainReq)
	if err != nil {
		return fmt.Errorf("nessie create_branch: read main: %w", err)
	}
	defer mainResp.Body.Close()
	if mainResp.StatusCode != 200 {
		rb, _ := io.ReadAll(mainResp.Body)
		return fmt.Errorf("nessie create_branch: GET main: status %d body %s", mainResp.StatusCode, string(rb))
	}
	var mainOut struct {
		Reference struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Hash string `json:"hash"`
		} `json:"reference"`
	}
	if err := json.NewDecoder(mainResp.Body).Decode(&mainOut); err != nil {
		return fmt.Errorf("nessie create_branch: decode main: %w", err)
	}
	if mainOut.Reference.Hash == "" {
		return fmt.Errorf("nessie create_branch: main reference returned empty hash")
	}

	// (2) Create the new branch with type+name in query, source in body.
	body, _ := json.Marshal(map[string]any{
		"type": "BRANCH",
		"name": "main",
		"hash": mainOut.Reference.Hash,
	})
	createURL := fmt.Sprintf("%s/api/v2/trees?type=BRANCH&name=%s", n.Endpoint, name)
	createReq, err := http.NewRequestWithContext(ctx, "POST", createURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("nessie create_branch: build create req: %w", err)
	}
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		return fmt.Errorf("nessie create_branch: do: %w", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode == 200 || createResp.StatusCode == 201 || createResp.StatusCode == 409 {
		return nil
	}
	rb, _ := io.ReadAll(createResp.Body)
	return fmt.Errorf("nessie create_branch(%s): status %d body %s", name, createResp.StatusCode, string(rb))
}
