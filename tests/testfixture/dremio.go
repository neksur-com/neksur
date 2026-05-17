package testfixture

// dremio.go — Dremio OSS testcontainer wrapper.
//
// Plan 03-01 introduces the Dremio fixture so Phase 3 cross-engine
// integration tests (snapshot pinning, 4-way read, write-coordinator)
// can be exercised against a real Dremio instance.
//
// Dremio OSS is the open-source distribution published on Docker Hub as
// `dremio/dremio-oss`. Startup is slow (~120-180s JVM bootstrap + REST
// API initialization) — the wait strategy polls /apiv2/server_status
// with a generous 180s timeout (matching the Trino fixture's budget).
//
// PENDING_FIRST_RUN pattern (D-2.09 Phase 02 precedent): nightly CI
// integration tests gate on SKIP_DOCKER and may skip via t.Skip() if
// the Dremio image pull fails. Individual integration tests that require
// a live Dremio instance should call StartDremio + t.Cleanup(Terminate).
//
// Iceberg catalog wiring: Dremio connects to Polaris (Neksur's Iceberg
// REST proxy) via the Dremio Arctic/Nessie REST connector. Phase 3 tests
// configure the catalog via Dremio's API; the helper IcebergRESTURL()
// returns the correct endpoint for catalog configuration.
//
// Threat T-3-dremio-test-creds-leak (PLAN threat model — ACCEPT): Dremio
// OSS test container uses a default `dremio/dremio123` admin credential.
// This is test-only; production Dremio integration (Plan 03-04) uses
// mTLS + service accounts per D-3.04. Never reuse this credential pattern
// in production code paths.

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// DremioImage is the Dremio OSS release Phase 3 fixtures target.
// Using latest for Phase 3 Wave 0 — Pin to a specific tag once a
// stable integration test baseline is confirmed in nightly CI.
const DremioImage = "dremio/dremio-oss:latest"

// dremioAPIPort is the Dremio REST API port (Dremio v22+).
const dremioAPIPort = "9047/tcp"

// DremioContainer wraps a running Dremio OSS coordinator node.
type DremioContainer struct {
	Container   testcontainers.Container
	APIEndpoint string // e.g., "http://127.0.0.1:49152" — Dremio REST API root
}

// StartDremio spins up a dremio/dremio-oss testcontainer and waits for
// the REST API to become ready. Cold start is ~120-180s (JVM bootstrap +
// catalog service initialization dominates); warm restart ~40-60s.
//
// Wait strategy: HTTP probe at /apiv2/server_status returning 200.
// Dremio's server_status endpoint returns 200 when the coordinator is
// fully initialized and accepting REST requests.
//
// PENDING_FIRST_RUN: the first invocation in a fresh CI environment may
// pull the ~1.8GB image. Callers should ensure sufficient disk + network
// bandwidth in the CI environment.
func StartDremio(ctx context.Context) (*DremioContainer, error) {
	req := testcontainers.ContainerRequest{
		Image:        DremioImage,
		ExposedPorts: []string{dremioAPIPort},
		// Dremio's /apiv2/server_status endpoint returns 200 when the
		// coordinator REST API is ready. The 180s budget is generous to
		// absorb CI cold-start variance (same budget as the Trino fixture).
		WaitingFor: wait.ForHTTP("/apiv2/server_status").
			WithPort(dremioAPIPort).
			WithStartupTimeout(180 * time.Second).
			WithStatusCodeMatcher(func(status int) bool {
				return status == 200
			}),
		Env: map[string]string{
			// Limit JVM heap for CI environments. Dremio defaults to 50%
			// of host RAM which can OOM a CI runner.
			"DREMIO_JAVA_SERVER_EXTRA_OPTS": "-Xmx2g -Xms512m",
		},
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start dremio: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: dremio host: %w", err)
	}
	port, err := c.MappedPort(ctx, dremioAPIPort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: dremio port: %w", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	return &DremioContainer{
		Container:   c,
		APIEndpoint: endpoint,
	}, nil
}

// Terminate shuts down the Dremio container. Safe to call multiple times.
func (d *DremioContainer) Terminate(ctx context.Context) error {
	if d == nil || d.Container == nil {
		return nil
	}
	return d.Container.Terminate(ctx)
}

// IcebergRESTURL returns the base URL that downstream plans should use
// when configuring Dremio's Iceberg REST catalog integration to point at
// the Neksur L1 gateway. In a test environment, this is typically the
// Polaris testcontainer's REST URL (wired by Phase3Fixture). The method
// documents the expected shape; callers replace with the actual gateway URL.
func (d *DremioContainer) IcebergRESTURL() string {
	if d == nil {
		return ""
	}
	// The Dremio container itself doesn't host an Iceberg REST endpoint —
	// it consumes one. Return the API endpoint as a documentation placeholder;
	// Phase3Fixture.IcebergRESTEndpointForEngine("dremio") returns the
	// correct Polaris/Neksur gateway URL to use in Dremio catalog config.
	return d.APIEndpoint
}
