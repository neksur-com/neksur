// Cypher observability metric registry — D-001.14 part 1/4.
//
// Defines the four Prometheus collectors that internal/graph emits on every
// (or sampled) Cypher call, plus the metrics HTTP server that exposes them
// for Prometheus scraping.
//
// Metric contract (D-001.14, encoded in ops/prometheus/alerts/cypher-latency.yaml):
//
//   - cypher_duration_ms (Histogram) — emitted on EVERY ExecuteCypher call.
//     The histogram_quantile P99 over a 5min window drives the
//     CypherP99LatencyBreach PromQL alert (sustained P99 > 2000ms for 5m).
//   - cypher_errors_total (Counter) — incremented on each error path.
//     Labeled by error_type so on-call can triage by failure mode.
//   - cypher_nodes_visited (Histogram) — sampled (1% + all slow >500ms).
//     Parsed from the EXPLAIN ANALYZE second-execution path in
//     cypher_wrapper.go::ExecuteCypher.
//   - cypher_edges_traversed (Histogram) — same sampling contract as
//     nodes_visited.
//
// All four collectors are registered with the default Prometheus registry
// via promauto.NewHistogramVec / NewCounterVec at package-init time. The
// HTTP server (StartMetricsServer) serves the default registry's snapshot
// at /metrics on the supplied listen address.
//
// Sampling rationale (RESEARCH §Pattern 3, also Pitfall 6): emitting an
// EXPLAIN ANALYZE on every call doubles Postgres load. We instead sample
// 1% of all calls + every call whose first-pass duration crossed 500ms
// (those are the calls we WANT detailed shape info on). This is the
// "all slow + 1% tail" pattern.
//
// Bucket choices:
//   - duration: 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000 ms.
//     Spans submillisecond planner-cache hits to the >5s window where the
//     PromQL alert fires. 250 / 500 buckets bracket the 500ms sampling
//     threshold; the 2500 bucket sits just above the 2000ms alert threshold.
//   - nodes/edges: 1, 10, 100, 1000, 10000, 100000, 1000000.
//     Geometric — captures both small starlight queries (1-10 nodes) and
//     pathological full-graph scans (>1M). The 100k+ buckets are the early
//     warning for D-001.10 Phase 2 graph engine migration trigger.

package graph

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// CypherDurationMs is the histogram emitted on EVERY ExecuteCypher call.
// Labeled by graph name (typically "neksur"). Drives the P99 PromQL alert.
var CypherDurationMs = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "cypher_duration_ms",
		Help:    "Cypher query wall-clock duration in milliseconds (D-001.14 part 1).",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
	},
	[]string{"graph"},
)

// CypherErrorsTotal counts Cypher execution failures. error_type is the
// Go reflect.TypeOf(err).String() (or "error" when reflection yields the
// generic *errors.errorString case via fmt.Errorf wrapping) — gives
// on-call enough fingerprint to triage by failure mode without leaking
// query text into the label cardinality.
var CypherErrorsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cypher_errors_total",
		Help: "Cypher errors counted (D-001.14 part 2).",
	},
	[]string{"graph", "error_type"},
)

// CypherNodesVisited is observed on the SAMPLED EXPLAIN ANALYZE path
// (all queries >500ms first-pass duration + 1% random sample). Parsed
// from AGE's plan tree via ParseExplain.
var CypherNodesVisited = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "cypher_nodes_visited",
		Help:    "Vertices touched per Cypher query (sampled; D-001.14 part 3).",
		Buckets: []float64{1, 10, 100, 1000, 10000, 100000, 1000000},
	},
	[]string{"graph"},
)

// CypherEdgesTraversed mirrors CypherNodesVisited for edges.
var CypherEdgesTraversed = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "cypher_edges_traversed",
		Help:    "Edges traversed per Cypher query (sampled; D-001.14 part 4).",
		Buckets: []float64{1, 10, 100, 1000, 10000, 100000, 1000000},
	},
	[]string{"graph"},
)

// StartMetricsServer binds the Prometheus default-registry /metrics
// handler on addr and serves it until ctx is cancelled. The server is
// gracefully shut down (5-second drain budget) when the context is
// done; the function returns ctx.Err() in that case, or any non-trivial
// http.Server error otherwise.
//
// addr follows net.Listen conventions ("host:port" or ":port"). A
// canonical Phase 0 deployment uses ":9100" (matches the
// infra/prometheus/prometheus.yml scrape job for the neksur-graph
// target). Tests bind ":0" via httptest patterns when port collision
// is a concern.
//
// This is intended to be invoked in a goroutine from cmd/neksur-server,
// gated behind the NEKSUR_OBSERVABILITY=1 feature flag so dev workflows
// without an OTel collector can still build & run.
func StartMetricsServer(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
