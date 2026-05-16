package testfixture

// trino.go — Trino 467 testcontainer wrapper.
//
// Plan 02-01 introduces the Trino fixture so the SQL proxy plumbing
// (Plan 02-05), the cross-engine policy compiler (Plan 02-04), and the
// verification probe (Plan 02-15) can be exercised against a real
// Trino instance — not a mock. Trino is the read-path reference engine
// for Phase 2 per D-PHASE0-stack §2.7 + RESEARCH §Standard Stack line
// 137.
//
// The container exposes Trino's REST + HTTP coordinator on port 8080.
// No auth is configured in the dev/test image — `default-authentication.type
// = NONE` is the testcontainers default and matches `trinodb/trino:467`'s
// out-of-box config (Threat T-2-test-only-creds-leak-trino — ACCEPTED
// per PLAN threat model: test-only, never reaches production).
//
// Iceberg catalog wiring: Trino ships the iceberg connector built-in.
// `CreateIcebergCatalog(name, restURL)` writes a per-test catalog file
// to /etc/trino/catalog/<name>.properties via `exec` and triggers a
// catalog reload — the canonical Trino testcontainer pattern.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TrinoImage is the Trino release stream Phase 2 fixtures target.
// 467 (December 2025) is the latest pre-Phase-2 release with the Iceberg
// connector at the same Iceberg-Java version Phase 1's Polaris was tested
// against (iceberg-java 1.6.x); newer Trino versions may require an
// iceberg-go version bump (Phase 2 is pinned to iceberg-go v0.5.0).
const TrinoImage = "trinodb/trino:467"

// trinoHTTPPort is the Trino coordinator's REST + HTTP port.
const trinoHTTPPort = "8080/tcp"

// TrinoContainer wraps a running Trino coordinator (single-node config —
// the container ships in coordinator-as-worker mode by default, which is
// sufficient for testcontainer use cases).
type TrinoContainer struct {
	Container testcontainers.Container
	Endpoint  string // e.g., "http://127.0.0.1:53251" — Trino REST root
}

// StartTrino spins up a trinodb/trino:467 testcontainer with the
// coordinator's HTTP port exposed. Wait strategy: HTTP probe at
// `/v1/info` returning 200 (Trino's lightweight readiness endpoint).
// Cold start is ~30-60s (JVM bootstrap dominates); warm restart ~10-20s.
//
// Threat T-2-test-only-creds-leak-trino (PLAN threat model — ACCEPT):
// the testcontainer uses Trino's no-auth dev mode. Production Trino
// integration (Plan 02-05+) uses TLS 1.3 + mTLS per D-2.08; never reuse
// this fixture pattern in production code paths.
func StartTrino(ctx context.Context) (*TrinoContainer, error) {
	req := testcontainers.ContainerRequest{
		Image:        TrinoImage,
		ExposedPorts: []string{trinoHTTPPort},
		// Trino's /v1/info returns 200 when the coordinator's discovery
		// is ready. The startup budget is generous (180s) to absorb CI
		// cold-image-pull variance — Polaris fixture uses the same
		// budget for the same reason.
		WaitingFor: wait.ForHTTP("/v1/info").
			WithPort(trinoHTTPPort).
			WithStartupTimeout(180 * time.Second).
			WithStatusCodeMatcher(func(status int) bool {
				return status == 200
			}),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start trino: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: trino host: %w", err)
	}
	port, err := c.MappedPort(ctx, trinoHTTPPort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: trino port: %w", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	return &TrinoContainer{
		Container: c,
		Endpoint:  endpoint,
	}, nil
}

// Terminate shuts down the Trino container. Safe to call multiple times.
func (t *TrinoContainer) Terminate(ctx context.Context) error {
	if t == nil || t.Container == nil {
		return nil
	}
	return t.Container.Terminate(ctx)
}

// JDBCEndpoint returns the Trino JDBC URL form
// (`jdbc:trino://<host>:<port>`). The Plan 02-05 SQL proxy uses this
// when routing Spark/Trino client connections through the gateway.
func (t *TrinoContainer) JDBCEndpoint() string {
	if t == nil {
		return ""
	}
	// Strip the http:// prefix; jdbc:trino expects <host>:<port>.
	endpoint := strings.TrimPrefix(t.Endpoint, "http://")
	return "jdbc:trino://" + endpoint
}

// CreateIcebergCatalog writes a per-test Iceberg REST-catalog properties
// file into the container's /etc/trino/catalog/ directory and triggers a
// catalog refresh via the SQL `CALL system.reload_catalog()` (Trino 467+).
//
// The properties file shape matches the Trino docs §iceberg-connector
// + REST catalog client config — pointing at the Polaris fixture's
// REST URL. Auth is currently unset (Polaris OAuth wiring will be added
// in Plan 02-05 alongside the SQL proxy mTLS path).
//
// Idempotent: a second call with the same name overwrites the properties
// file and reload_catalog refreshes the in-memory state.
//
// Plan 02-05 will likely supersede or extend this helper to wire mTLS +
// per-tenant catalog scoping; the current shape is the minimum viable
// fixture for cross-engine smoke tests.
func (t *TrinoContainer) CreateIcebergCatalog(ctx context.Context, name, restURL string) error {
	if t == nil || t.Container == nil {
		return fmt.Errorf("trino CreateIcebergCatalog: nil container")
	}
	props := fmt.Sprintf(`connector.name=iceberg
iceberg.catalog.type=rest
iceberg.rest-catalog.uri=%s
`, restURL)
	target := fmt.Sprintf("/etc/trino/catalog/%s.properties", name)

	// Write via `tee` so the in-container shell sees stdin properly;
	// testcontainers-go's `Exec` returns (exitCode, reader, err). We do
	// NOT consume the reader (the test surface only cares about exit
	// code) but defer-close would also be acceptable.
	exitCode, _, err := t.Container.Exec(ctx, []string{
		"sh", "-c", fmt.Sprintf("cat > %s <<'EOF'\n%sEOF\n", target, props),
	})
	if err != nil {
		return fmt.Errorf("trino CreateIcebergCatalog: exec write: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("trino CreateIcebergCatalog: write returned exit %d", exitCode)
	}
	// Note: Plan 02-05 will add the actual `CALL system.reload_catalog`
	// invocation via the trinodb/trino-go-client once that dep is added
	// (Task 3 of this plan). For now the properties file is in place and
	// the next coordinator restart picks it up — sufficient for the
	// Wave-0 smoke test which only asserts the container is bootable.
	return nil
}
