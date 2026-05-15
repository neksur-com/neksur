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
//
// Endpoint is the Iceberg REST catalog ROOT (per the Iceberg REST
// OpenAPI: the URL "uri" prop callers pass to iceberg-go's
// rest.NewCatalog). For Polaris 1.4.0 this resolves to
// `http://<host>:<port>/api/catalog`. Callers building their own
// REST requests (or using iceberg-go directly) MUST append
// `/v1/...` to reach the versioned API surface — this matches both
// iceberg-go's URL convention and Polaris's actual routing.
//
// (Plan 01-02 Task 2 deviation Rule 3: the previous Endpoint
// shape was `…/api/v1` and the OAuth helper hit `…/api/v1/oauth/tokens`,
// which Polaris 1.4.0 routes to 404. Switched to `…/api/catalog`
// + helper-side `/v1/oauth/tokens` to match the live routing
// confirmed by direct curl probe — see SUMMARY for details.)
type PolarisContainer struct {
	Container    testcontainers.Container
	Endpoint     string // e.g., "http://127.0.0.1:53251/api/catalog" — Iceberg REST catalog ROOT; helpers + iceberg-go append /v1/...
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
			//
			// Polaris 1.4.0 default realm is `POLARIS` (not `root` —
			// confirmed by reading the auto-bootstrap log line:
			// "Bootstrapping realm(s) 'POLARIS', if necessary…"). If
			// the realm name in this env var doesn't match the default
			// realm, Polaris silently auto-bootstraps with random
			// credentials, our pre-seeded ones never apply, and OAuth
			// returns 401 unauthorized_client. (Plan 01-02 Task 2
			// deviation Rule 3 — see SUMMARY for full root-cause.)
			"POLARIS_BOOTSTRAP_CREDENTIALS": "POLARIS,test-admin,test-secret",
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
	endpoint := fmt.Sprintf("http://%s:%s/api/catalog", host, port.Port())
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
// against Polaris's `<endpoint>/v1/oauth/tokens`. Polaris's Iceberg REST
// catalog (and admin API) require this token in the Authorization
// header for every subsequent call. The token TTL is short (a few
// minutes); tests should call this per logical operation.
//
// Endpoint shape: `<endpoint>` is the Iceberg REST catalog ROOT
// (i.e., `http://host:port/api/catalog` for Polaris 1.4.0). The
// `/v1/oauth/tokens` suffix matches both iceberg-go's URL convention
// and Polaris's actual routing — confirmed by direct curl probe
// during Plan 01-02 Task 2 (see SUMMARY for the deviation note).
func (p *PolarisContainer) token(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.ClientID)
	form.Set("client_secret", p.ClientSecret)
	form.Set("scope", "PRINCIPAL_ROLE:ALL")
	req, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/v1/oauth/tokens", strings.NewReader(form.Encode()))
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

// BootstrapDefaultCatalog creates the `test` catalog (INTERNAL type, S3
// storage) and grants the bootstrap principal full management rights
// on it. Polaris 1.4.0 does NOT pre-create any catalog at boot — every
// catalog must be explicitly created via the management API before
// namespaces or tables can be written under it. The Polaris management
// API path is `/api/management/v1/catalogs` (distinct from the Iceberg
// REST catalog API at `/api/catalog/v1/...`).
//
// Storage shape: S3 with a placeholder roleArn — Polaris 1.4.0
// rejected `FILE` storage type as `Unsupported storage type: FILE`,
// so we declare S3 with a dummy `arn:aws:iam::000000000000:role/test`.
// This is sufficient for namespace creation + table-not-found wire
// tests; full CreateTable + LoadTable round-trips need a working STS
// endpoint (e.g., LocalStack with sts service) — Phase 1 deferred to
// Plan 01-04 (ingestion materializes the storage stack).
//
// (Plan 01-02 Task 2 deviation Rule 3 — the testfixture as 01-01 left
// it tried to call CreateNamespace under `test` without first creating
// the catalog. 01-01's smoke tests never exercised that path so the
// gap remained latent. See SUMMARY for the full root-cause.)
//
// Idempotent: a 409 response from the catalog-create call is treated
// as success; the principal-role grant uses PUT (which is idempotent).
func (p *PolarisContainer) BootstrapDefaultCatalog(ctx context.Context) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}

	createCatalogBody, _ := json.Marshal(map[string]any{
		"catalog": map[string]any{
			"name": "test",
			"type": "INTERNAL",
			"properties": map[string]any{
				"default-base-location": "s3://test-bucket/test",
			},
			"storageConfigInfo": map[string]any{
				"storageType":      "S3",
				"allowedLocations": []string{"s3://test-bucket/test"},
				"roleArn":          "arn:aws:iam::000000000000:role/test",
			},
		},
	})
	mgmtURL := managementURL(p.Endpoint)
	req, err := http.NewRequestWithContext(ctx, "POST", mgmtURL+"/v1/catalogs", bytes.NewReader(createCatalogBody))
	if err != nil {
		return fmt.Errorf("polaris bootstrap_catalog: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("polaris bootstrap_catalog: do: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 && resp.StatusCode != 409 {
		return fmt.Errorf("polaris bootstrap_catalog: status %d body %s", resp.StatusCode, string(body))
	}

	// Assign the service_admin principal-role to the catalog_admin
	// catalog-role on the `test` catalog. Without this grant, the
	// bootstrap principal can issue tokens but cannot CreateNamespace
	// (Polaris returns 404 NoSuchNamespace because the principal
	// has no catalog-level visibility).
	grantBody, _ := json.Marshal(map[string]any{
		"catalogRole": map[string]any{"name": "catalog_admin"},
	})
	grantURL := mgmtURL + "/v1/principal-roles/service_admin/catalog-roles/test"
	req, err = http.NewRequestWithContext(ctx, "PUT", grantURL, bytes.NewReader(grantBody))
	if err != nil {
		return fmt.Errorf("polaris bootstrap_catalog: grant build: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("polaris bootstrap_catalog: grant do: %w", err)
	}
	gbody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	// 200/201/204 success; 409 idempotent re-grant.
	if resp2.StatusCode != 200 && resp2.StatusCode != 201 && resp2.StatusCode != 204 && resp2.StatusCode != 409 {
		return fmt.Errorf("polaris bootstrap_catalog: grant status %d body %s", resp2.StatusCode, string(gbody))
	}
	return nil
}

// managementURL returns the Polaris management API base URL given
// the catalog endpoint root. Both APIs sit under `/api/...` on the
// same host:port — catalog at `/api/catalog`, management at
// `/api/management`. We strip `/catalog` from the endpoint suffix
// and append `/management` to derive the management base.
func managementURL(catalogEndpoint string) string {
	return strings.TrimSuffix(catalogEndpoint, "/catalog") + "/management"
}

// CreateNamespace creates an Iceberg namespace via the REST catalog API.
// The `test` catalog must exist first — call BootstrapDefaultCatalog
// before this. The function is idempotent — a 409 response
// (namespace exists) is treated as success.
func (p *PolarisContainer) CreateNamespace(ctx context.Context, ns string) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"namespace": []string{ns},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/v1/test/namespaces", bytes.NewReader(body))
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
	url := fmt.Sprintf("%s/v1/test/namespaces/%s/tables", p.Endpoint, ns)
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
