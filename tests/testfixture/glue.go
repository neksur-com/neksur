package testfixture

// glue.go — AWS Glue Iceberg REST catalog via LocalStack testcontainer.
//
// Plan 03-01 introduces the Glue fixture so Phase 3 integration tests can
// exercise the Glue Iceberg REST adapter (Plan 03-05) against a local
// LocalStack emulation of the Glue service. LocalStack provides a full
// AWS Glue API emulator including Iceberg REST catalog support (LocalStack
// Pro or the open-source version with SERVICES=glue,s3 is sufficient for
// Phase 3 Iceberg metadata operations).
//
// The container exposes LocalStack's edge port (4566) which routes all
// AWS service APIs. Glue Iceberg REST is accessible at
// http://{host}:{port}/iceberg per LocalStack Glue Iceberg REST path.
//
// Threat T-3-glue-test-creds-leak (PLAN threat model — ACCEPT): LocalStack
// uses dummy AWS credentials (test/test). These are test-only; production
// Glue integration (Plan 03-05) uses real SigV4 + Lake Formation credentials
// per D-3.04. Never commit real AWS credentials to source code.

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// glueLocalStackImage is the LocalStack release Phase 3 Glue fixtures target.
// localstack/localstack:latest includes Glue + S3 support in the OSS
// version sufficient for Iceberg REST catalog metadata operations.
// Note: LocalStackImage and localStackEdgePort are defined in localstack.go;
// this file uses an alias to avoid a redeclaration conflict.
const glueLocalStackImage = "localstack/localstack:latest"

// glueEdgePort mirrors localStackEdgePort (defined in localstack.go).
// We re-declare as a local const to avoid a package-level constant conflict
// while keeping the Glue fixture self-contained.
const glueEdgePort = "4566/tcp"

// GlueContainer wraps a running LocalStack container configured with
// the Glue + S3 services for Iceberg REST catalog testing.
type GlueContainer struct {
	Container    testcontainers.Container
	EdgeEndpoint string // e.g., "http://127.0.0.1:4566" — LocalStack edge URL
}

// StartGlue spins up a LocalStack testcontainer with SERVICES=glue,s3.
// Wait strategy: HTTP probe at /_localstack/health returning 200 (LocalStack
// 3.x health endpoint). Cold start is ~20-40s.
//
// AWS credentials for LocalStack are fixed to AWS_ACCESS_KEY_ID=test /
// AWS_SECRET_ACCESS_KEY=test / AWS_REGION=us-east-1 — callers MUST use
// these dummy credentials when creating S3/Glue clients in tests.
//
// Note: the Phase 1 LocalStack fixture (localstack.go) starts with
// SERVICES=s3,sns,sqs for the detection pipeline. Phase 3's GlueContainer
// is a separate LocalStack instance with SERVICES=glue,s3 for the Glue
// Iceberg REST catalog adapter tests — both can run concurrently.
func StartGlue(ctx context.Context) (*GlueContainer, error) {
	req := testcontainers.ContainerRequest{
		Image:        glueLocalStackImage,
		ExposedPorts: []string{glueEdgePort},
		Env: map[string]string{
			// Enable Glue + S3 services. LocalStack OSS supports Glue
			// REST catalog for Iceberg table management + S3 for data file
			// storage (the Iceberg REST adapter writes manifest files to S3).
			"SERVICES":              "glue,s3",
			"AWS_DEFAULT_REGION":    "us-east-1",
			"LOCALSTACK_AUTH_TOKEN": "", // OSS mode — no auth token required
		},
		// LocalStack 3.x health endpoint: /_localstack/health returns 200
		// when the edge router and all SERVICES are up.
		WaitingFor: wait.ForHTTP("/_localstack/health").
			WithPort(glueEdgePort).
			WithStartupTimeout(90 * time.Second).
			WithStatusCodeMatcher(func(status int) bool {
				return status == 200
			}),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start glue/localstack: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: glue host: %w", err)
	}
	port, err := c.MappedPort(ctx, localStackEdgePort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: glue port: %w", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	return &GlueContainer{
		Container:    c,
		EdgeEndpoint: endpoint,
	}, nil
}

// Terminate shuts down the LocalStack container. Safe to call multiple times.
func (g *GlueContainer) Terminate(ctx context.Context) error {
	if g == nil || g.Container == nil {
		return nil
	}
	return g.Container.Terminate(ctx)
}

// IcebergRESTEndpoint returns the Glue Iceberg REST API base URL for the
// LocalStack emulated environment. LocalStack routes Glue Iceberg REST
// requests at `{edge_url}/iceberg`. Integration tests use this URL when
// configuring the Glue Iceberg REST adapter (Plan 03-05).
func (g *GlueContainer) IcebergRESTEndpoint() string {
	if g == nil {
		return ""
	}
	return g.EdgeEndpoint + "/iceberg"
}

// S3Endpoint returns the LocalStack S3 endpoint URL. Integration tests
// use this when configuring AWS S3 clients to target the LocalStack S3
// emulation (e.g., for verifying that Iceberg manifests written by the
// Glue adapter land in the expected S3 bucket).
func (g *GlueContainer) S3Endpoint() string {
	if g == nil {
		return ""
	}
	return g.EdgeEndpoint
}
