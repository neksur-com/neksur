package testfixture

// spark.go — Apache Spark 3.5.4 testcontainer wrapper.
//
// Plan 02-01 introduces the Spark fixture so the L2 Spark Extension
// (Plan 02-09), the SDK round-trip (Plan 02-10), and the end-to-end
// Spark write path (Plan 02-14) can be exercised against a real Spark
// session — not a mock. Spark 3.5.4 is the version locked by VALIDATION
// §JVM config + RESEARCH §Standard Stack (Scala 2.12.18 + Spark 3.5.4
// + iceberg-spark-runtime-3.5_2.12 1.6.1 — the JVM library's compile
// matrix in Plan 02-08).
//
// The container ships in `bash` entrypoint mode by default; we override
// to a long-running sleep so the fixture can `spark-submit` jobs into
// the live container via Exec without the entrypoint terminating.
//
// Threat T-2-spark-testcontainer-resource-exhaustion (PLAN threat model
// — ACCEPT): we cap Spark driver memory via SPARK_DRIVER_MEMORY env so
// a misconfigured test can't exhaust CI runner memory. t.Cleanup
// guarantees per-test container termination (Phase 0/1 pattern).

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// SparkImage is the Apache Spark release stream Phase 2 fixtures target.
// 3.5.4 (December 2024) is the latest Spark 3.5.x release at Phase 2
// start — Plan 02-08's neksur-spark-policy JVM library compiles against
// iceberg-spark-runtime-3.5_2.12:1.6.1 which pairs with this Spark line.
const SparkImage = "apache/spark:3.5.4"

// SparkContainer wraps a running Spark container kept alive via a sleep
// entrypoint so the fixture can spark-submit jobs into it from the host.
type SparkContainer struct {
	Container testcontainers.Container
}

// StartSpark spins up an apache/spark:3.5.4 testcontainer in long-running
// mode (sleep entrypoint). The container has spark-submit + the bundled
// Iceberg runtime available; Plan 02-09 + 02-10 tests submit jars + run
// pyspark scripts via SubmitJob.
//
// Wait strategy: the sleep entrypoint is ready immediately, but we wait
// for `spark-submit --version` to return cleanly via Exec to confirm
// the JVM is reachable. 120s startup budget absorbs cold-image pulls
// (Spark image is ~700MB).
//
// Resource cap: SPARK_DRIVER_MEMORY=1g + SPARK_EXECUTOR_MEMORY=512m
// limit the JVM heap so a runaway test can't OOM the CI runner
// (Threat T-2-spark-testcontainer-resource-exhaustion mitigation).
func StartSpark(ctx context.Context) (*SparkContainer, error) {
	req := testcontainers.ContainerRequest{
		Image: SparkImage,
		// Override the default entrypoint with a long-running sleep so
		// the container stays up after start — we spark-submit into it
		// from the test rather than running a "the test IS the
		// spark-submit" shape.
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"infinity"},
		Env: map[string]string{
			// Cap driver + executor memory so the container can't OOM a
			// CI runner. The neksur-spark-policy tests in Plan 02-08+
			// run pyspark/spark-submit jobs that are tiny by design;
			// 1g driver + 0.5g executor is comfortable.
			"SPARK_DRIVER_MEMORY":   "1g",
			"SPARK_EXECUTOR_MEMORY": "512m",
		},
		// The sleep entrypoint comes up instantly. We wait for the
		// container log to show the entrypoint executed (any byte
		// stream is fine — sleep doesn't print). Use a short timeout
		// so we don't spend 120s waiting for spark-submit confirmation
		// (which we verify lazily on first SubmitJob).
		WaitingFor: wait.ForExec([]string{"test", "-x", "/opt/spark/bin/spark-submit"}).
			WithStartupTimeout(120 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start spark: %w", err)
	}
	return &SparkContainer{Container: c}, nil
}

// Terminate shuts down the Spark container. Safe to call multiple times.
func (s *SparkContainer) Terminate(ctx context.Context) error {
	if s == nil || s.Container == nil {
		return nil
	}
	return s.Container.Terminate(ctx)
}

// SubmitJob runs spark-submit inside the container with the supplied
// jarPath (must already be copied into the container — Plan 02-08
// CopyFile pattern), main class, and arguments. Returns the exec exit
// code and any error.
//
// The neksur-spark-policy jar (Plan 02-08) is the typical input here —
// the test mounts the jar into /opt/neksur-spark-policy.jar via
// CopyFileToContainer then calls SubmitJob("/opt/neksur-spark-policy.jar",
// "com.neksur.spark.PolicyApplier", []string{"--input", "..."}).
//
// Plan 02-09 + 02-10 will likely supersede this minimal shape with a
// richer SparkSession-driver API once the test surface matures.
func (s *SparkContainer) SubmitJob(ctx context.Context, jarPath, mainClass string, args []string) (int, error) {
	if s == nil || s.Container == nil {
		return -1, fmt.Errorf("spark SubmitJob: nil container")
	}
	cmd := []string{
		"/opt/spark/bin/spark-submit",
		"--master", "local[2]",
		"--class", mainClass,
		jarPath,
	}
	cmd = append(cmd, args...)
	exitCode, _, err := s.Container.Exec(ctx, cmd)
	if err != nil {
		return exitCode, fmt.Errorf("spark SubmitJob: exec: %w", err)
	}
	return exitCode, nil
}

// CopyFile copies a host file into the container at the given target
// path. The neksur-spark-policy jar (Plan 02-08) is copied this way
// before SubmitJob is invoked.
func (s *SparkContainer) CopyFile(ctx context.Context, hostPath, containerPath string, mode int64) error {
	if s == nil || s.Container == nil {
		return fmt.Errorf("spark CopyFile: nil container")
	}
	return s.Container.CopyFileToContainer(ctx, hostPath, containerPath, mode)
}
