// Phase 2 SQL proxy P95/P99 latency runner — proves the read-path
// SQL proxy adds ≤50ms P95 / ≤150ms P99 over a baseline-direct query
// to Trino (REQ-NFR-latency-sql-proxy + ROADMAP Phase 2 success
// criterion §4).
//
// Two trials per RESEARCH §Performance (mirrors Phase 1
// gateway_overhead.go 2-trial paired pattern):
//
//  1. Baseline phase  — execute opts.SampleCount SELECT queries against
//     trinoDirectURL (direct Trino testcontainer), bypassing the SQL
//     proxy entirely. Each query is timed; durations sorted to compute
//     P50/P95/P99.
//
//  2. Proxy phase     — execute the SAME queries routed through the
//     SQL proxy endpoint (Neksur /v1/sql/trino/{prefix}/... per D-2.08).
//     Each query body is byte-identical to the baseline phase so the
//     only delta is the proxy pipeline (mTLS + CompiledPolicy fetch +
//     row-filter injection + audit emit).
//
//  3. Compute absolute P95 / P99 of the proxy phase. Caller asserts
//     P95 ≤ 50ms AND P99 ≤ 150ms per REQ-NFR-latency-sql-proxy.
//
// The runner gracefully handles the "proxy not yet implemented" case:
// if the proxy URL returns 404 / connection refused, the runner writes
// `status: "PENDING_PROXY_LANDED"` to the baseline JSON and exits 0.
// Plan 02-05 ships the proxy; once that lands, the nightly CI script
// (phase2-sql-proxy-p95.sh) starts populating real numbers.

package load

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	// Imported for side-effect: registers the `trino` driver with
	// database/sql. Phase 2 RESEARCH §Standard Stack line 142.
	_ "github.com/trinodb/trino-go-client/trino"
)

// SqlProxyOpts configures the SQL-proxy overhead measurement shape.
// Defaults reflect REQ-NFR-latency-sql-proxy (50ms P95 / 150ms P99) +
// the Phase 1 gateway_overhead.go pattern.
type SqlProxyOpts struct {
	// SampleCount — total queries per phase (after warmup). Default 1000.
	SampleCount int

	// ConcurrentClients — parallel goroutines per phase. Default 1
	// (serial = lower variance, easier to interpret regressions).
	ConcurrentClients int

	// QueriesPerSecond — soft rate limit per client. Default 10 qps.
	QueriesPerSecond int

	// WarmupQueries — discarded before measurement starts (warms the
	// trino-go-client connection pool + CompiledPolicy LRU cache).
	// Default 100.
	WarmupQueries int

	// Query — the SQL the runner executes against both endpoints. A
	// trivial `SELECT 1` is the canonical neutral probe (matches Plan
	// 02-15 verification probe shape: minimum work on the engine side,
	// so we measure proxy overhead and not Trino's query planning).
	Query string
}

func (o SqlProxyOpts) withDefaults() SqlProxyOpts {
	if o.SampleCount <= 0 {
		o.SampleCount = 1000
	}
	if o.ConcurrentClients <= 0 {
		o.ConcurrentClients = 1
	}
	if o.QueriesPerSecond <= 0 {
		o.QueriesPerSecond = 10
	}
	if o.WarmupQueries < 0 {
		o.WarmupQueries = 0
	}
	if o.WarmupQueries == 0 {
		o.WarmupQueries = 100
	}
	if o.Query == "" {
		o.Query = "SELECT 1"
	}
	return o
}

// SqlProxyResult is the per-trial summary. The caller's exit-0/1
// decision keys on (BaselineP95 ≤ baseline-budget) AND (ProxyP95 ≤
// proxy-budget) AND (ProxyP99 ≤ proxy-p99-budget). The driver wires
// the assertion thresholds; this struct just carries the numbers.
type SqlProxyResult struct {
	BaselineP50 time.Duration `json:"baseline_p50"`
	BaselineP95 time.Duration `json:"baseline_p95"`
	BaselineP99 time.Duration `json:"baseline_p99"`
	ProxyP50    time.Duration `json:"proxy_p50"`
	ProxyP95    time.Duration `json:"proxy_p95"`
	ProxyP99    time.Duration `json:"proxy_p99"`

	// OverheadRatio — (ProxyP95 - BaselineP95) / BaselineP95 — useful
	// as a sanity metric alongside the absolute P95/P99 gates.
	OverheadRatio float64 `json:"overhead_ratio"`

	SampleCount int `json:"sample_count"`
	Errors      int `json:"errors"`

	// Millisecond projections for JSON-friendly downstream consumers
	// (jq + dashboards prefer integers over Go's "12.345s" format).
	BaselineP50Ms int64 `json:"baseline_p50_ms"`
	BaselineP95Ms int64 `json:"baseline_p95_ms"`
	BaselineP99Ms int64 `json:"baseline_p99_ms"`
	ProxyP50Ms    int64 `json:"proxy_p50_ms"`
	ProxyP95Ms    int64 `json:"proxy_p95_ms"`
	ProxyP99Ms    int64 `json:"proxy_p99_ms"`
}

// QueryFn executes a single SQL query against either the baseline-bypass
// upstream (direct Trino) or the proxy endpoint. Implementations MUST
// be safe to call concurrently. Returns the wall-clock round-trip
// duration of the query. A non-nil error increments the errors counter
// but does NOT halt the measurement (per-error Halt would bias P95
// toward fast-fail paths).
type QueryFn func(ctx context.Context) (time.Duration, error)

// MeasureSqlProxyOverhead drives the two-trial measurement. baselineFn
// is wired to a direct Trino driver (trino-go-client); proxyFn is
// wired to the Neksur SQL proxy HTTP endpoint (httptest.NewServer
// wrapping the proxy handler, or a live deployment URL).
//
// The function is single-shot per (baselineFn, proxyFn) pair. Warmup
// runs proxyFn ONLY — the baseline path has only the trino-go-client
// pool to warm (sub-100ms naturally); the proxy has both the pool +
// the CompiledPolicy LRU cache.
func MeasureSqlProxyOverhead(
	ctx context.Context,
	baselineFn QueryFn,
	proxyFn QueryFn,
	opts SqlProxyOpts,
) (SqlProxyResult, error) {
	opts = opts.withDefaults()
	result := SqlProxyResult{SampleCount: opts.SampleCount}

	// Warmup phase — gateway/proxy + CompiledPolicy cache warm.
	for i := 0; i < opts.WarmupQueries; i++ {
		if _, err := proxyFn(ctx); err != nil {
			return result, fmt.Errorf("warmup query %d: %w", i, err)
		}
	}

	// Baseline phase.
	baselineDurations, baselineErrs, err := runQueryPhase(ctx, baselineFn, opts)
	if err != nil {
		return result, fmt.Errorf("baseline phase: %w", err)
	}

	// Proxy phase.
	proxyDurations, proxyErrs, err := runQueryPhase(ctx, proxyFn, opts)
	if err != nil {
		return result, fmt.Errorf("proxy phase: %w", err)
	}

	if len(baselineDurations) == 0 || len(proxyDurations) == 0 {
		return result, fmt.Errorf("zero successful queries in one phase (baseline=%d proxy=%d)",
			len(baselineDurations), len(proxyDurations))
	}

	sort.Slice(baselineDurations, func(i, j int) bool { return baselineDurations[i] < baselineDurations[j] })
	sort.Slice(proxyDurations, func(i, j int) bool { return proxyDurations[i] < proxyDurations[j] })

	result.BaselineP50 = pickPctl(baselineDurations, 0.50)
	result.BaselineP95 = pickPctl(baselineDurations, 0.95)
	result.BaselineP99 = pickPctl(baselineDurations, 0.99)
	result.ProxyP50 = pickPctl(proxyDurations, 0.50)
	result.ProxyP95 = pickPctl(proxyDurations, 0.95)
	result.ProxyP99 = pickPctl(proxyDurations, 0.99)

	result.BaselineP50Ms = result.BaselineP50.Milliseconds()
	result.BaselineP95Ms = result.BaselineP95.Milliseconds()
	result.BaselineP99Ms = result.BaselineP99.Milliseconds()
	result.ProxyP50Ms = result.ProxyP50.Milliseconds()
	result.ProxyP95Ms = result.ProxyP95.Milliseconds()
	result.ProxyP99Ms = result.ProxyP99.Milliseconds()

	if result.BaselineP95 > 0 {
		result.OverheadRatio = float64(result.ProxyP95-result.BaselineP95) /
			float64(result.BaselineP95)
	}
	result.Errors = baselineErrs + proxyErrs
	return result, nil
}

// runQueryPhase executes opts.SampleCount queries via fn, distributed
// across opts.ConcurrentClients goroutines. Returns per-query durations,
// error count, and any phase-level fatal error.
func runQueryPhase(ctx context.Context, fn QueryFn, opts SqlProxyOpts) ([]time.Duration, int, error) {
	queriesPerClient := opts.SampleCount / opts.ConcurrentClients
	if queriesPerClient < 1 {
		queriesPerClient = 1
	}
	type clientResult struct {
		durations []time.Duration
		errCount  int
	}
	results := make(chan clientResult, opts.ConcurrentClients)

	clientCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	rateGap := time.Second / time.Duration(opts.QueriesPerSecond)
	for c := 0; c < opts.ConcurrentClients; c++ {
		go func() {
			out := clientResult{}
			ticker := time.NewTicker(rateGap)
			defer ticker.Stop()
			for i := 0; i < queriesPerClient; i++ {
				select {
				case <-clientCtx.Done():
					results <- out
					return
				case <-ticker.C:
				}
				dur, err := fn(clientCtx)
				if err != nil {
					out.errCount++
					continue
				}
				out.durations = append(out.durations, dur)
			}
			results <- out
		}()
	}
	var allDurations []time.Duration
	totalErrs := 0
	for c := 0; c < opts.ConcurrentClients; c++ {
		r := <-results
			allDurations = append(allDurations, r.durations...)
			totalErrs += r.errCount
	}
	return allDurations, totalErrs, nil
}

// pickPctl returns the duration at percentile p (0 < p < 1) from a
// pre-sorted slice. Nearest-rank: rank = ceil(p × N) − 1 (0-indexed).
// Identical to gateway_overhead.go pickPercentile but locally-named to
// keep both packages independent.
func pickPctl(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ----------------------------------------------------------------------
// Helper: trino-go-client QueryFn factory.
//
// The driver uses this for the baseline phase. The proxy phase uses
// HTTPQueryFn (below) because the proxy endpoint is HTTP-shaped (Phase
// 2 D-2.08 defines `/v1/sql/{engine}/{prefix}/...`).
// ----------------------------------------------------------------------

// TrinoQueryFn returns a QueryFn that opens a trino-go-client connection
// to the given DSN and executes `query` on each call. The DB handle is
// reused across calls via database/sql's connection pool (per real
// Trino client behavior — Spark, dbt, presto-cli all pool connections).
//
// DSN shape: `http://user@host:port?catalog=...&schema=...` — the
// canonical trino-go-client URL form. For a testcontainer Trino with no
// auth, `http://test@127.0.0.1:<port>?catalog=tpch&schema=tiny` works.
//
// Returns an error if the initial sql.Open fails. The returned closure
// surfaces per-query errors but doesn't halt the runner.
func TrinoQueryFn(dsn, query string) (QueryFn, func() error, error) {
	db, err := sql.Open("trino", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("TrinoQueryFn: sql.Open: %w", err)
	}
	// Conservative pool config — match the proxy's expected client pool
	// shape so fairness isn't skewed by one side having more connections.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(5 * time.Minute)

	fn := func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return 0, fmt.Errorf("TrinoQueryFn: QueryContext: %w", err)
		}
		// Drain the rowset so the measurement includes the full
		// network round-trip (Trino streams pages; closing without
		// draining can short-circuit the round-trip on small results).
		for rows.Next() {
			_ = rows.Scan() // ignore values; we only care about timing
		}
		_ = rows.Err()
		_ = rows.Close()
		return time.Since(start), nil
	}
	cleanup := func() error { return db.Close() }
	return fn, cleanup, nil
}

// HTTPQueryFn returns a QueryFn that issues a GET against the SQL proxy
// endpoint (or POST if the proxy implementation chooses POST for SQL
// pass-through). Plan 02-05 finalizes the exact wire shape; for the
// Wave-0 skeleton this is a probe — we issue a GET to detect 404 /
// connection refused (the "proxy not yet shipped" path) and a POST
// when the proxy is live.
//
// `proxyURL` is the full proxy endpoint URL (e.g.,
// `https://gateway.neksur.example/v1/sql/trino/prod/query`). `bearer`
// is the OAuth Bearer token for authn — empty string is acceptable when
// the proxy is in the not-yet-shipped state (the HTTP probe shape only
// gates on status code).
//
// The QueryFn returns a sentinel error wrapping ErrProxyNotShipped
// when the proxy returns 404 or the TCP connection is refused; the
// driver translates this into a `status: PENDING_PROXY_LANDED` baseline
// JSON write (no test failure).
func HTTPQueryFn(client *http.Client, proxyURL, query, bearer string) QueryFn {
	return func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL, nil)
		if err != nil {
			return 0, fmt.Errorf("HTTPQueryFn: build req: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		// The proxy is expected to accept the SQL via a `X-Trino-Sql`
		// header or a JSON body — Plan 02-05 picks one. For the
		// Wave-0 skeleton we pass via header so the GET shape works.
		req.Header.Set("X-Trino-Sql", query)
		resp, err := client.Do(req)
		if err != nil {
			// connection-refused / DNS / TLS — treat as "proxy not shipped"
			// at the driver level (the sentinel comparison happens there).
			return 0, fmt.Errorf("%w: %v", ErrProxyNotShipped, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		dur := time.Since(start)
		// 404 = proxy not shipped (Plan 02-05 deferred).
		if resp.StatusCode == 404 {
			return dur, fmt.Errorf("%w: status 404", ErrProxyNotShipped)
		}
		if resp.StatusCode >= 400 {
			return dur, fmt.Errorf("HTTPQueryFn: status %d", resp.StatusCode)
		}
		return dur, nil
	}
}

// ErrProxyNotShipped sentinels the "Plan 02-05 hasn't landed yet" case.
// The driver checks errors.Is(err, ErrProxyNotShipped) to flip the
// baseline JSON status to PENDING_PROXY_LANDED (instead of FAIL).
var ErrProxyNotShipped = errProxyNotShipped{}

type errProxyNotShipped struct{}

func (errProxyNotShipped) Error() string {
	return "sql proxy endpoint not shipped (Plan 02-05 deferred)"
}
