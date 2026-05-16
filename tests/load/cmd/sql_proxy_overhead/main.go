// Command sql_proxy_overhead is the Phase 2 SQL-proxy P95/P99 latency
// runner. Per REQ-NFR-latency-sql-proxy + ROADMAP Phase 2 success
// criterion §4: the SQL proxy MUST add ≤50ms P95 / ≤150ms P99 over a
// baseline-direct Trino query.
//
// Two trials per RESEARCH §Performance (mirror Phase 1
// gateway_overhead.go pattern):
//   1. Baseline phase  — SELECT ... via direct Trino driver (trino-go-client)
//                        bypassing the proxy entirely.
//   2. Proxy phase     — same query routed through /v1/sql/trino/{prefix}/...
//                        per D-2.08.
//
// Assert absolute P95 ≤ -assert-p95-under (default 50ms) AND absolute
// P99 ≤ -assert-p99-under (default 150ms). Per W9 (Phase 1 carryover),
// ALWAYS writes -baseline-out (default tests/load/sql-proxy-baseline.json)
// regardless of PASS / FAIL — empirical evidence trail for the
// REQ-NFR-latency-sql-proxy contract; PR review catches regressions
// via `git diff`.
//
// Plan 02-05 deferral handling: if the proxy endpoint returns 404 or
// connection-refused (Plan 02-05 ships the proxy itself; this Wave-0
// runner lands the measurement skeleton), the driver writes
// `status: PENDING_PROXY_LANDED` to the baseline JSON and exits 0.
// Once Plan 02-05 lands, the nightly CI script
// (scripts/ci/phase2-sql-proxy-p95.sh) starts populating real numbers.
//
// Usage:
//
//	go run ./tests/load/cmd/sql_proxy_overhead \
//	    -trino-dsn "http://test@127.0.0.1:8080?catalog=tpch&schema=tiny" \
//	    -proxy-url "https://gateway.example/v1/sql/trino/prod/query" \
//	    -bearer    "$ICEBERG_REST_BEARER" \
//	    -sample-count 1000 \
//	    -warmup       100 \
//	    -assert-p95-under=50ms \
//	    -assert-p99-under=150ms \
//	    -baseline-out tests/load/sql-proxy-baseline.json
//
// Exit codes:
//   0 — both P95 + P99 gates passed AND zero phase-fatal errors; OR the
//       proxy is in the not-yet-shipped state (baseline JSON written
//       with status=PENDING_PROXY_LANDED).
//   1 — gate miss or runtime error (baseline JSON still written).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/neksur-com/neksur/tests/load"
)

// baselineEnvelope is the canonical JSON shape committed to git as
// tests/load/sql-proxy-baseline.json. The PENDING_FIRST_RUN status is
// the initial committed value; live runs overwrite the file with
// measured numbers + status PASS / FAIL / PENDING_PROXY_LANDED. SREs
// subscribe via PR diff watch + dashboards (mirror Phase 1
// gateway-overhead-baseline.json shape).
type baselineEnvelope struct {
	Status            string  `json:"status"`
	MeasuredAt        string  `json:"measured_at"`
	BaselineP95Ms     int64   `json:"baseline_p95_ms"`
	InstrumentedP95Ms int64   `json:"instrumented_p95_ms"`
	P99Ms             int64   `json:"p99_ms"`
	OverheadRatio     float64 `json:"overhead_ratio"`
	SamplesPerTrial   int     `json:"samples_per_trial"`

	AssertP95UnderMs int64  `json:"assert_p95_under_ms"`
	AssertP99UnderMs int64  `json:"assert_p99_under_ms"`
	Errors           int    `json:"errors,omitempty"`
	TrinoDSN         string `json:"trino_dsn,omitempty"`
	ProxyURL         string `json:"proxy_url,omitempty"`
	Details          string `json:"details,omitempty"`

	// Diagnostic fields populated on every live run.
	BaselineP50Ms int64 `json:"baseline_p50_ms,omitempty"`
	BaselineP99Ms int64 `json:"baseline_p99_ms,omitempty"`
	ProxyP50Ms    int64 `json:"proxy_p50_ms,omitempty"`
}

func main() {
	var (
		trinoDSN = flag.String("trino-dsn", "",
			"direct Trino DSN (e.g. http://user@127.0.0.1:8080?catalog=tpch&schema=tiny)")
		proxyURL = flag.String("proxy-url", "",
			"Neksur SQL proxy endpoint URL (e.g. https://gateway/v1/sql/trino/prod/query)")
		bearer = flag.String("bearer", "",
			"OAuth Bearer token (applied to BOTH phases for fairness)")
		query = flag.String("query", "SELECT 1",
			"SQL query executed against both endpoints — keep trivial so we measure proxy overhead, not Trino planning")
		sampleCount = flag.Int("sample-count", 1000, "total queries per phase")
		warmup      = flag.Int("warmup", 100, "discarded queries before measurement")
		concurrent  = flag.Int("concurrent-clients", 1, "parallel goroutines per phase")
		qps         = flag.Int("queries-per-second", 10, "soft per-client rate limit")

		// REQ-NFR-latency-sql-proxy gate defaults: P95 default "50ms",
		// P99 default "150ms". The string literals are echoed in the
		// flag-help so operators see the canonical budget alongside the
		// Duration value.
		assertP95Under = flag.Duration("assert-p95-under", 50*time.Millisecond,
			`REQ-NFR-latency-sql-proxy: proxy P95 ≤ this absolute budget (default "50ms")`)
		assertP99Under = flag.Duration("assert-p99-under", 150*time.Millisecond,
			`REQ-NFR-latency-sql-proxy: proxy P99 ≤ this absolute budget (default "150ms")`)

		baselineOut = flag.String("baseline-out", "tests/load/sql-proxy-baseline.json",
			"JSON baseline artifact (committed to git)")
		skipBaseline = flag.Bool("skip-baseline", false, "skip baseline phase (dev-only)")
	)
	flag.Parse()

	if !*skipBaseline && *trinoDSN == "" {
		failNoBaseline(baselineOut, *assertP95Under, *assertP99Under,
			"-trino-dsn required (or -skip-baseline for dev runs)")
	}
	if *proxyURL == "" {
		failNoBaseline(baselineOut, *assertP95Under, *assertP99Under,
			"-proxy-url required")
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Build baselineFn (direct Trino) — skip wires a tiny no-op closure.
	var baselineFn load.QueryFn
	var cleanup func() error
	if *skipBaseline {
		baselineFn = func(ctx context.Context) (time.Duration, error) {
			return time.Millisecond, nil
		}
	} else {
		fn, cl, err := load.TrinoQueryFn(*trinoDSN, *query)
		if err != nil {
			// Baseline DSN bad — Trino reachability is a prerequisite, not
			// an optional path. Fail with PENDING_PROXY_LANDED-like shape
			// but exit-1 since the baseline IS required.
			failNoBaseline(baselineOut, *assertP95Under, *assertP99Under,
				fmt.Sprintf("baseline TrinoQueryFn build: %v", err))
		}
		baselineFn = fn
		cleanup = cl
	}
	if cleanup != nil {
		defer cleanup() //nolint:errcheck // best-effort pool close
	}

	proxyFn := load.HTTPQueryFn(httpClient, *proxyURL, *query, *bearer)

	opts := load.SqlProxyOpts{
		SampleCount:       *sampleCount,
		ConcurrentClients: *concurrent,
		QueriesPerSecond:  *qps,
		WarmupQueries:     *warmup,
		Query:             *query,
	}

	overallTimeout := time.Duration(2*opts.SampleCount/opts.QueriesPerSecond+
		opts.WarmupQueries/opts.QueriesPerSecond+60) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), overallTimeout)
	defer cancel()

	res, err := load.MeasureSqlProxyOverhead(ctx, baselineFn, proxyFn, opts)

	// PENDING_PROXY_LANDED detection: if the warmup or the proxy phase
	// surfaces ErrProxyNotShipped, write the deferred-status baseline
	// JSON and exit 0 (the runner skeleton landed; the proxy is the
	// downstream Plan 02-05 concern).
	if err != nil && errors.Is(err, load.ErrProxyNotShipped) {
		writePendingProxyLanded(*baselineOut, *assertP95Under, *assertP99Under,
			*trinoDSN, *proxyURL, opts.SampleCount, err)
		fmt.Fprintln(os.Stdout, "sql-proxy-overhead PENDING_PROXY_LANDED — Plan 02-05 not yet shipped; baseline JSON written.")
		os.Exit(0)
	}

	status := "PASS"
	details := ""
	if err != nil {
		status = "FAIL"
		details = fmt.Sprintf("MeasureSqlProxyOverhead: %v", err)
	} else {
		if res.ProxyP95 > *assertP95Under {
			status = "FAIL"
			details = fmt.Sprintf("proxy P95 = %v exceeds -assert-p95-under=%v", res.ProxyP95, *assertP95Under)
		}
		if res.ProxyP99 > *assertP99Under {
			status = "FAIL"
			if details != "" {
				details += "; "
			}
			details += fmt.Sprintf("proxy P99 = %v exceeds -assert-p99-under=%v", res.ProxyP99, *assertP99Under)
		}
	}

	bl := baselineEnvelope{
		Status:            status,
		MeasuredAt:        time.Now().UTC().Format(time.RFC3339Nano),
		BaselineP95Ms:     res.BaselineP95Ms,
		InstrumentedP95Ms: res.ProxyP95Ms,
		P99Ms:             res.ProxyP99Ms,
		OverheadRatio:     res.OverheadRatio,
		SamplesPerTrial:   opts.SampleCount,
		AssertP95UnderMs:  assertP95Under.Milliseconds(),
		AssertP99UnderMs:  assertP99Under.Milliseconds(),
		Errors:            res.Errors,
		TrinoDSN:          *trinoDSN,
		ProxyURL:          *proxyURL,
		Details:           details,
		BaselineP50Ms:     res.BaselineP50Ms,
		BaselineP99Ms:     res.BaselineP99Ms,
		ProxyP50Ms:        res.ProxyP50Ms,
	}
	if werr := writeBaseline(*baselineOut, bl); werr != nil {
		fmt.Fprintf(os.Stderr, "WARN: write baseline: %v\n", werr)
	}

	if status == "FAIL" {
		fmt.Fprintf(os.Stderr, "sql-proxy-overhead FAIL details=%s baseline_p95_ms=%d proxy_p95_ms=%d proxy_p99_ms=%d errors=%d\n",
			details, res.BaselineP95Ms, res.ProxyP95Ms, res.ProxyP99Ms, res.Errors)
		os.Exit(1)
	}
	fmt.Printf("sql-proxy-overhead PASS proxy_p95=%v proxy_p99=%v baseline_p95=%v samples=%d errors=%d\n",
		res.ProxyP95, res.ProxyP99, res.BaselineP95, opts.SampleCount, res.Errors)
}

func writeBaseline(path string, bl baselineEnvelope) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(bl)
}

func writePendingProxyLanded(path string, p95Budget, p99Budget time.Duration, trinoDSN, proxyURL string, samples int, cause error) {
	bl := baselineEnvelope{
		Status:           "PENDING_PROXY_LANDED",
		MeasuredAt:       time.Now().UTC().Format(time.RFC3339Nano),
		SamplesPerTrial:  samples,
		AssertP95UnderMs: p95Budget.Milliseconds(),
		AssertP99UnderMs: p99Budget.Milliseconds(),
		TrinoDSN:         trinoDSN,
		ProxyURL:         proxyURL,
		Details:          fmt.Sprintf("proxy endpoint not reachable: %v", cause),
	}
	_ = writeBaseline(path, bl)
}

func failNoBaseline(baselineOut *string, p95Budget, p99Budget time.Duration, msg string) {
	bl := baselineEnvelope{
		Status:           "FAIL",
		MeasuredAt:       time.Now().UTC().Format(time.RFC3339Nano),
		AssertP95UnderMs: p95Budget.Milliseconds(),
		AssertP99UnderMs: p99Budget.Milliseconds(),
		Details:          msg,
	}
	_ = writeBaseline(*baselineOut, bl)
	fmt.Fprintf(os.Stderr, "FAIL: %s\n", msg)
	os.Exit(1)
}
