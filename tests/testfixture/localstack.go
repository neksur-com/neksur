package testfixture

// localstack.go — LocalStack 3 testcontainer wrapper.
//
// Plan 01-07 (L3 regex detection) reads Iceberg manifests from S3 and
// reacts to S3 ObjectCreated events delivered via SNS->SQS. LocalStack
// provides those AWS-API-compatible services in-container so the
// detection pipeline can be exercised end-to-end without hitting real
// AWS.
//
// The community LocalStack image (`localstack/localstack:3`) covers
// S3 + SNS + SQS without requiring an auth token; LOCALSTACK_AUTH_TOKEN
// unlocks Pro features. Phase 1 tests set the token from env in CI;
// dev runs without the token still pass (Phase 1 uses only community
// services per CONTEXT scope).

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// LocalStackImage is the canonical LocalStack 3.x release stream.
const LocalStackImage = "localstack/localstack:3"

// localStackEdgePort is LocalStack's single edge gateway (every service
// is mounted under one port via host-style routing).
const localStackEdgePort = "4566/tcp"

// LocalStackContainer wraps a running LocalStack instance with the
// single Endpoint URL that the AWS SDK uses as `--endpoint-url`.
type LocalStackContainer struct {
	Container testcontainers.Container
	Endpoint  string // e.g., "http://127.0.0.1:53251"
}

// StartLocalStack spins up the LocalStack 3 community image with the
// services that Phase 1 detection (Plan 01-07) needs: s3 + sns + sqs.
// Pulls LOCALSTACK_AUTH_TOKEN from env if set (CI sources from GitHub
// Secrets per VALIDATION.md line 91); empty token is accepted for
// non-CI dev runs (community services do not require it).
//
// Wait strategy: HTTP probe at /_localstack/health with a 120s startup
// budget — LocalStack typically reports ready in 5-15s but cold-image
// pulls can add 60-90s.
func StartLocalStack(ctx context.Context) (*LocalStackContainer, error) {
	env := map[string]string{
		"SERVICES":       "s3,sns,sqs",
		"DEBUG":          "0",
		"EAGER_SERVICE_LOADING": "1",
	}
	if token := os.Getenv("LOCALSTACK_AUTH_TOKEN"); token != "" {
		// Pro-tier auth token — only set when actually provided by env.
		env["LOCALSTACK_AUTH_TOKEN"] = token
	}
	req := testcontainers.ContainerRequest{
		Image:        LocalStackImage,
		ExposedPorts: []string{localStackEdgePort},
		Env:          env,
		WaitingFor: wait.ForHTTP("/_localstack/health").
			WithPort(localStackEdgePort).
			WithStartupTimeout(120 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start localstack: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: localstack host: %w", err)
	}
	port, err := c.MappedPort(ctx, localStackEdgePort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: localstack port: %w", err)
	}
	return &LocalStackContainer{
		Container: c,
		Endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
	}, nil
}

// Terminate shuts down the LocalStack container. Safe to call multiple times.
func (l *LocalStackContainer) Terminate(ctx context.Context) error {
	if l == nil || l.Container == nil {
		return nil
	}
	return l.Container.Terminate(ctx)
}
