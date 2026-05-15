package testfixture

// polaris.go — Apache Polaris 1.4.0 testcontainer wrapper.
//
// Plan 01-01 introduces the Polaris fixture so Plan 01-02's Polaris
// adapter (and the Plan 01-06 gateway commit-proxy tests) can exercise
// real Iceberg REST flows against a real Polaris instance — not a mock.
// Polaris is the reference catalog per D-1.02 (PLAN 01-CONTEXT.md).
//
// The container uses Polaris's "bootstrap credentials" env var to
// pre-seed an OAuth client (`test-admin` / `test-secret` under realm
// `root`); CreateNamespace + CreateTable obtain a bearer token via the
// client_credentials grant against `/v1/oauth/tokens` then call the
// Iceberg REST catalog API at `/v1/test/...`.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PolarisImage is the canonical Apache Polaris release image for Phase 1.
//
// Deviation note (Plan 01-01 Task 3 Rule 1): the plan referenced the
// tag `apache/polaris:1.4.0-incubating` but Apache Polaris graduated
// from the Incubator before that release was published, so the actual
// Docker Hub tag is `apache/polaris:1.4.0` (the `-incubating` suffix
// was dropped). 1.3.0-incubating is the last incubating-suffixed
// build; 1.4.0+ ships without the suffix.
const PolarisImage = "apache/polaris:1.4.0"

// polarisHTTPPort is the Iceberg REST + admin API port inside the container.
const polarisHTTPPort = "8181/tcp"

// PolarisContainer wraps a running Polaris instance plus the OAuth
// bootstrap credentials needed to drive the Iceberg REST API.
type PolarisContainer struct {
	Container    testcontainers.Container
	Endpoint     string // e.g., "http://127.0.0.1:53251/api/v1" — base for /v1/oauth/tokens, /v1/test/namespaces
	ClientID     string // bootstrap principal — "test-admin"
	ClientSecret string // bootstrap secret — "test-secret"
}

// StartPolaris spins up an apache/polaris:1.4.0-incubating container,
// bootstraps a single OAuth client via the POLARIS_BOOTSTRAP_CREDENTIALS
// env var, and waits for /api/v1/health to return 200. Total cold start
// is ~30-60s the first time the image is pulled, ~5-15s subsequently.
//
// Wait strategy: HTTP probe at /api/v1/health with a 120s startup
// budget. Polaris's JVM bootstrap is the dominant cost; the timeout is
// generous to absorb CI cold-start variance.
func StartPolaris(ctx context.Context) (*PolarisContainer, error) {
	req := testcontainers.ContainerRequest{
		Image:        PolarisImage,
		ExposedPorts: []string{polarisHTTPPort},
		Env: map[string]string{
			// realm,clientId,clientSecret — Polaris parses this comma-list
			// at boot and pre-seeds the principal. The credentials are
			// test-only and never reach a production runtime.
			"POLARIS_BOOTSTRAP_CREDENTIALS": "root,test-admin,test-secret",
		},
		// Polaris's `/api/catalog/v1/config` endpoint requires an OAuth
		// bearer token (401 unauthenticated). 401 means the server is
		// fully routing requests — the OAuth token endpoint at
		// /api/catalog/v1/oauth/tokens is also up. We treat 401 (and
		// 200, just in case Polaris ever relaxes auth on /config) as
		// "ready". The Polaris bundle does expose a health endpoint
		// on the separate management port 8182, but that requires
		// publishing a second port to the host — the 401-tolerant probe
		// on 8181 is simpler and equally diagnostic.
		WaitingFor: wait.ForHTTP("/api/catalog/v1/config").
			WithPort(polarisHTTPPort).
			WithStartupTimeout(180 * time.Second).
			WithStatusCodeMatcher(func(status int) bool {
				return status == 200 || status == 401
			}),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start polaris: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: polaris host: %w", err)
	}
	port, err := c.MappedPort(ctx, polarisHTTPPort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: polaris port: %w", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s/api/v1", host, port.Port())
	return &PolarisContainer{
		Container:    c,
		Endpoint:     endpoint,
		ClientID:     "test-admin",
		ClientSecret: "test-secret",
	}, nil
}

// Terminate shuts down the Polaris container. Safe to call multiple times.
func (p *PolarisContainer) Terminate(ctx context.Context) error {
	if p == nil || p.Container == nil {
		return nil
	}
	return p.Container.Terminate(ctx)
}

// token obtains an OAuth bearer token via the client_credentials grant
// against Polaris's `/v1/oauth/tokens` endpoint. Polaris's Iceberg REST
// catalog (and admin API) require this token in the Authorization
// header for every subsequent call. The token TTL is short (a few
// minutes); tests should call this per logical operation.
func (p *PolarisContainer) token(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.ClientID)
	form.Set("client_secret", p.ClientSecret)
	form.Set("scope", "PRINCIPAL_ROLE:ALL")
	req, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/oauth/tokens", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("polaris token: build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("polaris token: do: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("polaris token: status %d body %s", resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("polaris token: decode: %w body: %s", err, string(body))
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("polaris token: empty access_token in %s", string(body))
	}
	return out.AccessToken, nil
}

// CreateNamespace creates an Iceberg namespace via the REST catalog API.
// Polaris pre-creates a `test` catalog at boot; the namespace lands
// under that catalog. The function is idempotent — a 409 response
// (namespace exists) is treated as success.
func (p *PolarisContainer) CreateNamespace(ctx context.Context, ns string) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"namespace": []string{ns},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/test/namespaces", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("polaris create_namespace: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("polaris create_namespace: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
		return nil
	}
	rb, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("polaris create_namespace(%s): status %d body %s", ns, resp.StatusCode, string(rb))
}

// CreateTable creates a minimal Iceberg table under the given namespace
// via the REST catalog API. `schemaJSON` is the raw Iceberg schema (the
// `schema` field of the create-table request); callers can either build
// a full schema struct or pass a hand-rolled JSON body. The function is
// idempotent — a 409 response (table exists) is treated as success.
//
// Phase 1 callers (Plan 01-02 adapter tests, Plan 01-06 gateway tests)
// drive minimal schemas. Plan 01-02 will introduce typed builders in
// internal/iceberg/; until then this signature accepts the raw bytes.
func (p *PolarisContainer) CreateTable(ctx context.Context, ns, name string, schemaJSON json.RawMessage) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"name":   name,
		"schema": schemaJSON,
	})
	url := fmt.Sprintf("%s/test/namespaces/%s/tables", p.Endpoint, ns)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("polaris create_table: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("polaris create_table: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
		return nil
	}
	rb, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("polaris create_table(%s.%s): status %d body %s", ns, name, resp.StatusCode, string(rb))
}
