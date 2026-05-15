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

// DefaultNessieWarehouse is the warehouse name configured on the
// Nessie testcontainer's catalog REST endpoint. Nessie 0.100 requires
// a non-empty warehouse declared via
// `nessie.catalog.warehouses.<name>.location`; otherwise
// `/iceberg/v1/config` returns 500 "No default-warehouse configured".
// "warehouse" matches Nessie's documented default name and the value
// the nessie adapter (Plan 01-03) forwards as the Iceberg REST
// `warehouse` property.
const DefaultNessieWarehouse = "warehouse"

// NessieContainer wraps a running Nessie instance with branch helpers
// for the Plan 01-03 adapter tests.
type NessieContainer struct {
	Container testcontainers.Container
	Endpoint  string // e.g., "http://127.0.0.1:53251" — base for /api/v2/... (native Nessie REST)
	// IcebergEndpoint is the Iceberg REST URL the nessie adapter
	// (Plan 01-03) consumes — Nessie 0.100 mounts the Iceberg REST
	// API at `<base>/iceberg`. iceberg-go appends `/v1/...` to this.
	IcebergEndpoint string
	Branch          string // default "neksur-test"
	// Warehouse is the Nessie warehouse name configured on the
	// container; pass through to nessie.Config.Warehouse (or the
	// adapter's hardcoded `nessieWarehouse` constant if the field
	// hasn't been added yet — Plan 01-03 currently uses the
	// constant).
	Warehouse string
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
		//
		// Configure a default warehouse so the Iceberg REST endpoint
		// (`/iceberg/v1/config`) returns 200 instead of 500
		// "No default-warehouse configured". This is required by
		// the Plan 01-03 nessie adapter live test (TestNessieAdapterLoadTable);
		// previously this fixture only exercised the native Nessie
		// REST API (`/api/v2/...`) which doesn't require a warehouse.
		// Plan 01-03 deviation note (Rule 3 — blocking): the Iceberg
		// REST endpoint requires server-side warehouse config; the
		// Nessie 0.100 default container ships without any.
		// "file:///tmp/warehouse" is an in-container POSIX path —
		// Nessie writes table metadata there. The path lives only
		// inside the container and is destroyed at Terminate.
		Env: map[string]string{
			"nessie.catalog.default-warehouse":             DefaultNessieWarehouse,
			"nessie.catalog.warehouses.warehouse.location": "file:///tmp/warehouse",
		},
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
	base := fmt.Sprintf("http://%s:%s", host, port.Port())
	n := &NessieContainer{
		Container:       c,
		Endpoint:        base,
		IcebergEndpoint: base + "/iceberg",
		Branch:          DefaultNessieBranch,
		Warehouse:       DefaultNessieWarehouse,
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
//  1. GET /api/v2/trees/main          → extract main's current hash
//  2. POST /api/v2/trees?type=BRANCH&name=<newname>
//     with body {"type":"BRANCH","name":"main","hash":"<mainHash>"}
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
