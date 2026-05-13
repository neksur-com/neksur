// neksur-server — main backend binary entry point.
//
// Phase 0 stub. M1 wires up the REST API skeleton + Iceberg REST proxy
// foundation; M2 adds the MCP server + policy CRUD; M3 adds the pgwire
// SQL proxy + L1 Catalog Gateway full validation; M4 adds the Spark
// write-path integration. See docs/phase-0-stack.md §5 for the milestone
// breakdown, and §6 for the planned internal/ package layout this binary
// will compose.
//
// Plan 00-05 (Wave 4) addition: when NEKSUR_OBSERVABILITY=1 is set the
// binary wires up the OTLP gRPC trace exporter and the Prometheus
// /metrics HTTP server defined in internal/graph/telemetry.go. The
// feature flag is OFF by default so dev workflows that don't bring up
// an OTel collector / Prometheus pair still build & run cleanly.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/neksur-com/neksur/internal/graph"
)

func main() {
	fmt.Println("Neksur Server (placeholder — Phase 0 stub; M1 will wire up REST API, MCP server, SQL proxy).")

	if os.Getenv("NEKSUR_OBSERVABILITY") == "1" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := runWithObservability(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "observability bootstrap failed: %v\n", err)
			os.Exit(1)
		}
	}
}

// runWithObservability wires the OTel SDK + Prometheus metrics server
// per the Plan 00-05 D-001.14 contract.
//
//   - OTLP gRPC trace exporter (defaults to localhost:4317, the OTel
//     collector port from infra/otel/docker-compose.observability.yml).
//   - sdktrace.NewTracerProvider with WithBatcher — production-grade
//     batching, not the dev WithSyncer.
//   - otel.SetTracerProvider so internal/graph.ExecuteCypher's
//     otel.Tracer("neksur.graph") resolves to this provider.
//   - Prometheus metrics server on :9100, matching the
//     infra/prometheus/prometheus.yml neksur-graph scrape target.
//
// The function blocks on ctx until SIGINT/SIGTERM, then drains both
// the metrics HTTP server (5s grace) and the trace exporter (5s grace).
func runWithObservability(ctx context.Context) error {
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return fmt.Errorf("otlptracegrpc.New: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(tp)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}()

	// Start the Prometheus /metrics server in a goroutine. The error
	// channel surfaces a non-graceful exit so we can fail-fast at boot
	// time if (e.g.) port 9100 is already taken.
	addr := os.Getenv("NEKSUR_METRICS_ADDR")
	if addr == "" {
		addr = ":9100"
	}
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- graph.StartMetricsServer(ctx, addr) }()

	select {
	case <-ctx.Done():
		// Drain the metrics server's error (StartMetricsServer returns
		// ctx.Err() on cancellation, which we expect here).
		<-metricsErr
		return nil
	case err := <-metricsErr:
		return fmt.Errorf("metrics server: %w", err)
	}
}
