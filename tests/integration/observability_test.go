//go:build integration

// Plan 00-05 Wave 4 — Cypher telemetry / D-001.14 / REQ-NFR-graph-observability.
//
// Four integration tests that verify the ExecuteCypher middleware
// (internal/graph/cypher_wrapper.go) end-to-end:
//
//   - TestDurationEmitted        — every call emits cypher_duration_ms.
//   - TestErrorsCounted          — error path increments cypher_errors_total.
//   - TestTraversalMetricsEmitted — slow / sampled call emits
//                                   cypher_nodes_visited + cypher_edges_traversed.
//   - TestDBSemconvAttributes    — span has the four OTel DB-semconv
//                                   attributes (validates Assumption A6).
//
// Build tag: `//go:build integration` — these tests run only under
// `go test -tags integration ./tests/integration/...`. They reuse the
// package-scoped AGE testcontainer fixture from main_test.go (which is
// untagged and therefore loaded by both default and integration-tag
// builds).
//
// Metrics server lifecycle: each test that needs to scrape /metrics
// uses startMetricsServerForTest, which binds the Prometheus
// promhttp.Handler on a free local port via httptest.NewServer (NOT
// graph.StartMetricsServer — that binds the default registry on a
// fixed port and isn't safe for concurrent test runs). The default
// registry is shared with the production code path, so the assertions
// see the same counters / histograms that production would emit.

package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/neksur-com/neksur/internal/graph"
)

// startMetricsServerForTest binds promhttp.Handler on a free local port
// via httptest.NewServer and returns the base URL. The server is torn
// down via t.Cleanup.
func startMetricsServerForTest(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// scrapeMetrics performs a GET on baseURL/metrics and returns the body
// as a string. Test-fatal on any HTTP / IO error.
func scrapeMetrics(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("scrape /metrics: status %d, body=%s", resp.StatusCode, string(body))
	}
	return string(body)
}

// graphClientForTest constructs a GraphClient against the package
// fixture's superuser DSN — sufficient to exercise the AGE call shape
// without needing the RLS-bound role-downgrade dance.
func graphClientForTest(t *testing.T) *graph.GraphClient {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — observability_test.go needs the AGE container")
	}
	if fix == nil {
		t.Fatal("fix is nil; TestMain did not run")
	}
	gc, err := graph.NewGraphClient(fix.ctx, fix.container.SuperuserDSN)
	if err != nil {
		t.Fatalf("NewGraphClient: %v", err)
	}
	t.Cleanup(gc.Close)
	return gc
}

// drainRows fully consumes a pgx.Rows so the pool connection is
// returned. Tests don't care about the result rows themselves.
func drainRows(t *testing.T, rows pgx.Rows) {
	t.Helper()
	if rows == nil {
		return
	}
	for rows.Next() {
		// nothing — just iterate to completion
	}
	if err := rows.Err(); err != nil {
		t.Logf("rows.Err (non-fatal): %v", err)
	}
	rows.Close()
}

// TestDurationEmitted runs one Cypher query via graph.ExecuteCypher and
// asserts that the resulting /metrics scrape includes a non-zero
// cypher_duration_ms_bucket sample for the test graph label.
//
// Maps to 00-VALIDATION.md W4 row: REQ-NFR-graph-observability /
// D-001.14 part 1 (every call emits duration).
func TestDurationEmitted(t *testing.T) {
	baseURL := startMetricsServerForTest(t)
	gc := graphClientForTest(t)

	rows, err := graph.ExecuteCypher(fix.ctx, gc, "neksur", "RETURN 1")
	if err != nil {
		t.Fatalf("ExecuteCypher: %v", err)
	}
	drainRows(t, rows)

	body := scrapeMetrics(t, baseURL)
	if !strings.Contains(body, `cypher_duration_ms_bucket{graph="neksur"`) {
		t.Fatalf("expected cypher_duration_ms_bucket{graph=\"neksur\",...} in /metrics, body=%s", body)
	}
	// Sanity: the histogram _count series should be ≥1 too.
	if !strings.Contains(body, `cypher_duration_ms_count{graph="neksur"`) {
		t.Errorf("expected cypher_duration_ms_count{graph=\"neksur\"} in /metrics")
	}
}

// TestErrorsCounted runs a syntactically-invalid Cypher (open paren
// imbalance) and asserts the next /metrics scrape exposes
// cypher_errors_total{graph="neksur",error_type="..."} ≥ 1. The exact
// error_type label is implementation-specific (e.g., "pg:42601" for a
// SQLSTATE syntax error), so we assert presence of the
// `cypher_errors_total{graph="neksur"` prefix rather than a specific
// type value.
//
// Maps to D-001.14 part 2 (every error path increments the counter).
func TestErrorsCounted(t *testing.T) {
	baseURL := startMetricsServerForTest(t)
	gc := graphClientForTest(t)

	// Open-paren imbalance is a Cypher parser error → AGE surfaces it
	// as a SQLSTATE 42601 (syntax_error) at execute time.
	_, err := graph.ExecuteCypher(fix.ctx, gc, "neksur", "MATCH ((( RETURN 1")
	if err == nil {
		t.Fatal("expected ExecuteCypher to return an error on malformed Cypher; got nil")
	}

	body := scrapeMetrics(t, baseURL)
	if !strings.Contains(body, `cypher_errors_total{`) {
		t.Fatalf("expected cypher_errors_total{...} in /metrics, body did not contain it")
	}
	if !strings.Contains(body, `graph="neksur"`) {
		t.Fatalf("expected error counter to carry graph=\"neksur\" label, body=%s", body)
	}
}

// TestTraversalMetricsEmitted exercises the sampling path of
// ExecuteCypher: a synthetic slow query (wraps pg_sleep) is GUARANTEED
// to cross the slow-query threshold (>500ms), so its
// cypher_nodes_visited / cypher_edges_traversed observation MUST land
// in the registry by the time we scrape.
//
// We use the pg_sleep wrapper rather than RETURN 1 because the 1%
// random sampler is non-deterministic; the slow-query path is
// deterministic.
//
// Maps to D-001.14 parts 3+4 (sampled traversal metrics).
func TestTraversalMetricsEmitted(t *testing.T) {
	baseURL := startMetricsServerForTest(t)
	gc := graphClientForTest(t)

	// pg_sleep(0.6) — guaranteed slow path. We wrap it in a Cypher
	// MATCH that DOES touch graph storage so EXPLAIN's relation tree
	// has at least one classifiable node. AGE handles arbitrary
	// pg_* calls inside RETURN expressions.
	slow := `MATCH (n) RETURN n, pg_sleep(0.6) LIMIT 1`
	rows, err := graph.ExecuteCypher(fix.ctx, gc, "neksur", slow)
	if err != nil {
		t.Fatalf("ExecuteCypher slow: %v", err)
	}
	drainRows(t, rows)

	body := scrapeMetrics(t, baseURL)
	// We assert presence of the bucket family, not a specific count —
	// the actual nodes/edges counts depend on graph contents which
	// the fixture leaves empty.
	if !strings.Contains(body, "cypher_nodes_visited_bucket") {
		t.Errorf("expected cypher_nodes_visited_bucket in /metrics, body did not contain it")
	}
	if !strings.Contains(body, "cypher_edges_traversed_bucket") {
		t.Errorf("expected cypher_edges_traversed_bucket in /metrics, body did not contain it")
	}
	// _count for both should also be ≥1 after a slow call.
	if !strings.Contains(body, `cypher_nodes_visited_count{graph="neksur"`) {
		t.Errorf("expected cypher_nodes_visited_count{graph=\"neksur\"} ≥1 after slow call")
	}
	if !strings.Contains(body, `cypher_edges_traversed_count{graph="neksur"`) {
		t.Errorf("expected cypher_edges_traversed_count{graph=\"neksur\"} ≥1 after slow call")
	}
}

// TestDBSemconvAttributes captures spans emitted by ExecuteCypher using
// tracetest.InMemoryExporter and asserts the four OTel DB-semconv
// attributes are present with the expected values. This is the
// empirical confirmation of Assumption A6: OTel DB semconv applies
// cleanly to AGE-wrapped Postgres at the client layer.
func TestDBSemconvAttributes(t *testing.T) {
	gc := graphClientForTest(t)

	// Install a TracerProvider with an in-memory exporter for the
	// duration of this test. The graph package uses
	// otel.Tracer("neksur.graph") which resolves to the global
	// provider — so we must SetTracerProvider here and reset on
	// cleanup. This is single-test scope; the parallel-test contract
	// is preserved because the global is only read inside
	// ExecuteCypher, not stored.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
	})

	ctx, cancel := context.WithTimeout(fix.ctx, 30*time.Second)
	defer cancel()
	rows, err := graph.ExecuteCypher(ctx, gc, "neksur", "RETURN 1")
	if err != nil {
		t.Fatalf("ExecuteCypher: %v", err)
	}
	drainRows(t, rows)

	// Force the SDK to flush spans synchronously (NewTracerProvider's
	// WithSyncer is synchronous, but the span only finalises on
	// span.End(), which ExecuteCypher's defer ensures by the time we
	// observe — so this is belt-and-braces).
	if err := tp.ForceFlush(ctx); err != nil {
		t.Logf("ForceFlush (non-fatal): %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span from ExecuteCypher, got 0")
	}

	// Find the cypher.execute span (defensive: if some future code
	// adds parent spans, we still pick the right one).
	var target *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "cypher.execute" {
			target = &spans[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("no cypher.execute span found; got %d spans named %v", len(spans), spanNames(spans))
	}

	attrs := map[string]string{}
	for _, kv := range target.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}

	if got := attrs["db.system.name"]; got != "postgresql" {
		t.Errorf("db.system.name = %q, want %q", got, "postgresql")
	}
	if got := attrs["db.namespace"]; got != "neksur|ag_catalog" {
		t.Errorf("db.namespace = %q, want %q", got, "neksur|ag_catalog")
	}
	if got := attrs["db.operation.name"]; got != "cypher" {
		t.Errorf("db.operation.name = %q, want %q", got, "cypher")
	}
	if got := attrs["db.query.text"]; got == "" {
		t.Errorf("db.query.text is empty; want non-empty parameterised query text")
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
