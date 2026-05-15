// Phase 1 commit-overhead measurement runner — proves the L1 Catalog
// Gateway adds ≤5% latency over a baseline-bypass commit directly to
// upstream Polaris (ADR-003 §3.3 + CONTEXT line 174).
//
// Two trials per RESEARCH §Performance:
//
//  1. Baseline phase  — execute opts.CommitCount commits against
//     polarisDirectURL (the Polaris testcontainer endpoint), bypassing
//     Neksur entirely. Each commit is timed; durations are sorted to
//     compute P50/P95/P99.
//
//  2. Gateway phase   — execute the SAME commits routed through
//     gatewayURL (httptest.NewServer wrapping iceberggw.CommitHandler).
//     Each commit body is byte-identical to the baseline phase so the
//     only delta is the gateway pipeline (creds fetch + policy fetch +
//     CEL eval + audit emit + ingest dispatch).
//
//  3. Compute OverheadRatio = (GatewayP95 - BaselineP95) / BaselineP95.
//     Caller asserts ≤ 0.05 per ADR-003 §3.3.
//
// The harness is HTTP-shape — it does NOT depend on a live AGE
// container or in-process gateway construction. Callers (cmd
// gateway_overhead) are responsible for:
//
//   - building a fresh Polaris testcontainer for the baseline endpoint;
//   - constructing iceberggw.Deps and httptest.NewServer-wrapping
//     iceberggw.CommitHandler for the gateway endpoint;
//   - cooking up a synthetic commit body that's accepted by Polaris
//     AND by the gateway's allow-all policy (the gateway pipeline
//     cannot validate beyond what its policy store + CEL evaluator
//     allow — the test substrate provisions an allow-all policy).

package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"time"
)

// OverheadOpts configures the overhead measurement shape. Defaults
// reflect RESEARCH §Standard Stack note: Polaris commit rate is
// approximately 10 cps per tenant; 1000 commits = 100s of wall-clock
// per phase, sufficient for stable P95/P99 estimation.
//
//   - CommitCount        — total commits per phase (after warmup).
//                          Default 1000.
//   - ConcurrentClients  — parallel commit goroutines per phase.
//                          Default 1 (serial = lower variance).
//   - CommitsPerSecond   — soft rate limit. Default 10 cps.
//   - WarmupCommits      — commits to drop before measurement starts
//                          (warm CEL cache + iceberg-go conn pool).
//                          Default 100.
type OverheadOpts struct {
	CommitCount       int
	ConcurrentClients int
	CommitsPerSecond  int
	WarmupCommits     int
}

func (o OverheadOpts) withDefaults() OverheadOpts {
	if o.CommitCount <= 0 {
		o.CommitCount = 1000
	}
	if o.ConcurrentClients <= 0 {
		o.ConcurrentClients = 1
	}
	if o.CommitsPerSecond <= 0 {
		o.CommitsPerSecond = 10
	}
	if o.WarmupCommits < 0 {
		o.WarmupCommits = 0
	}
	if o.WarmupCommits == 0 {
		o.WarmupCommits = 100
	}
	return o
}

// OverheadResult is the per-trial summary returned from
// MeasureGatewayOverhead. The caller's exit-0/1 decision keys on
// OverheadRatio ≤ assert-overhead-under (default 0.05 per ADR-003 §3.3).
//
// BaselineP95 < GatewayP95 is the expected shape (the gateway adds
// strictly positive latency: creds fetch + policy fetch + CEL eval +
// audit emit). The contract is that the DELTA stays ≤5% of baseline.
type OverheadResult struct {
	BaselineP50   time.Duration `json:"baseline_p50"`
	BaselineP95   time.Duration `json:"baseline_p95"`
	BaselineP99   time.Duration `json:"baseline_p99"`
	GatewayP50    time.Duration `json:"gateway_p50"`
	GatewayP95    time.Duration `json:"gateway_p95"`
	GatewayP99    time.Duration `json:"gateway_p99"`
	OverheadRatio float64       `json:"overhead_ratio"`
	CommitCount   int           `json:"commit_count"`
	Errors        int           `json:"errors"`

	// Millisecond projections for JSON-friendly downstream consumers
	// (jq + dashboards prefer integers over Go's "12.345s" duration
	// format). All four percentiles + the absolute baseline/gateway
	// duration in ms are mirrored here.
	BaselineP50Ms int64 `json:"baseline_p50_ms"`
	BaselineP95Ms int64 `json:"baseline_p95_ms"`
	BaselineP99Ms int64 `json:"baseline_p99_ms"`
	GatewayP50Ms  int64 `json:"gateway_p50_ms"`
	GatewayP95Ms  int64 `json:"gateway_p95_ms"`
	GatewayP99Ms  int64 `json:"gateway_p99_ms"`
}

// CommitFn executes a single commit against either the baseline-bypass
// upstream or the gateway endpoint. Implementations MUST be safe to
// call concurrently and MUST not retain references to the request
// body buffer past return (the harness reuses the buffer per request
// to avoid per-iteration allocation pressure that would skew the
// measurement).
//
// Returns the wall-clock duration of the commit. A non-nil error is
// counted as an Errors increment but does NOT halt the measurement
// (per-error Halt would bias P95 toward fast-fail paths — the test
// substrate is responsible for ensuring the gateway's policy is set
// to "allow all" so genuine errors are exceptional).
type CommitFn func(ctx context.Context) (time.Duration, error)

// MeasureGatewayOverhead drives the two-trial measurement: baseline
// commits (via baselineFn — the test wires this to a direct Polaris
// HTTP call) followed by gateway commits (via gatewayFn — the test
// wires this to a httptest.NewServer wrapping iceberggw.CommitHandler).
//
// The function is single-shot per (baselineFn, gatewayFn) pair. The
// warmup phase uses the gatewayFn ONLY (the gateway has both a CEL
// LRU cache + iceberg-go connection pool to warm; the baseline path
// has only the iceberg-go pool which warms in <100ms naturally).
func MeasureGatewayOverhead(
	ctx context.Context,
	baselineFn CommitFn,
	gatewayFn CommitFn,
	opts OverheadOpts,
) (OverheadResult, error) {
	opts = opts.withDefaults()
	result := OverheadResult{CommitCount: opts.CommitCount}
	var totalErrors int32

	// Warmup phase — run gatewayFn opts.WarmupCommits times to warm
	// the CEL compile-cache + iceberg-go connection pool. Discard
	// timings.
	for i := 0; i < opts.WarmupCommits; i++ {
		if _, err := gatewayFn(ctx); err != nil {
			// Warmup errors signal a fundamental wiring issue (bad
			// adapter, broken policy store) — surface immediately so
			// the operator doesn't get a false PASS from a misconfigured
			// gateway.
			return result, fmt.Errorf("warmup commit %d: %w", i, err)
		}
	}

	// Baseline phase.
	baselineDurations, baselineErrs, err := runPhase(ctx, baselineFn, opts)
	if err != nil {
		return result, fmt.Errorf("baseline phase: %w", err)
	}
	atomic.AddInt32(&totalErrors, int32(baselineErrs))

	// Gateway phase.
	gatewayDurations, gatewayErrs, err := runPhase(ctx, gatewayFn, opts)
	if err != nil {
		return result, fmt.Errorf("gateway phase: %w", err)
	}
	atomic.AddInt32(&totalErrors, int32(gatewayErrs))

	if len(baselineDurations) == 0 || len(gatewayDurations) == 0 {
		return result, fmt.Errorf("zero successful commits in one phase (baseline=%d gateway=%d)",
			len(baselineDurations), len(gatewayDurations))
	}

	// Sort + compute percentiles per phase. P50 = median, P95, P99
	// per the standard percentile-by-rank-position formula.
	sort.Slice(baselineDurations, func(i, j int) bool { return baselineDurations[i] < baselineDurations[j] })
	sort.Slice(gatewayDurations, func(i, j int) bool { return gatewayDurations[i] < gatewayDurations[j] })

	result.BaselineP50 = pickPercentile(baselineDurations, 0.50)
	result.BaselineP95 = pickPercentile(baselineDurations, 0.95)
	result.BaselineP99 = pickPercentile(baselineDurations, 0.99)
	result.GatewayP50 = pickPercentile(gatewayDurations, 0.50)
	result.GatewayP95 = pickPercentile(gatewayDurations, 0.95)
	result.GatewayP99 = pickPercentile(gatewayDurations, 0.99)

	result.BaselineP50Ms = result.BaselineP50.Milliseconds()
	result.BaselineP95Ms = result.BaselineP95.Milliseconds()
	result.BaselineP99Ms = result.BaselineP99.Milliseconds()
	result.GatewayP50Ms = result.GatewayP50.Milliseconds()
	result.GatewayP95Ms = result.GatewayP95.Milliseconds()
	result.GatewayP99Ms = result.GatewayP99.Milliseconds()

	if result.BaselineP95 > 0 {
		result.OverheadRatio = float64(result.GatewayP95-result.BaselineP95) /
			float64(result.BaselineP95)
	}
	result.Errors = int(totalErrors)
	return result, nil
}

// runPhase executes opts.CommitCount commits via fn, distributed across
// opts.ConcurrentClients goroutines. Returns the per-commit durations
// (one slice across all clients), the error count, and any phase-level
// fatal error.
//
// The CommitsPerSecond rate limit is enforced per-client (each client
// waits 1/cps between commits) — at 1 client + 10 cps = 100ms per
// commit minimum, which is comfortable for the test-container Polaris.
func runPhase(ctx context.Context, fn CommitFn, opts OverheadOpts) ([]time.Duration, int, error) {
	commitsPerClient := opts.CommitCount / opts.ConcurrentClients
	if commitsPerClient < 1 {
		commitsPerClient = 1
	}
	type clientResult struct {
		durations []time.Duration
		errCount  int
	}
	results := make(chan clientResult, opts.ConcurrentClients)

	clientCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	rateGap := time.Second / time.Duration(opts.CommitsPerSecond)
	for c := 0; c < opts.ConcurrentClients; c++ {
		go func() {
			out := clientResult{}
			ticker := time.NewTicker(rateGap)
			defer ticker.Stop()
			for i := 0; i < commitsPerClient; i++ {
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

// pickPercentile returns the duration at percentile p (0 < p < 1) from
// a pre-sorted slice. Uses nearest-rank: rank = ceil(p × N) − 1 (0-indexed).
// For N=1000, p=0.95 → rank 949 (the 950th-largest). Standard P95
// definition; matches Phase 0 Plan 00-06's tests/load/cmd/run.
func pickPercentile(sorted []time.Duration, p float64) time.Duration {
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
// Helper: HTTP-driven CommitFn factory.
//
// The test substrate uses these to build per-endpoint CommitFn closures
// without re-implementing the HTTP plumbing in the cmd driver. Each
// returned closure:
//
//   - issues POST <url> with the supplied body + headers (Content-Type
//     application/json + Authorization Bearer <token>);
//   - times the round-trip from before-Send to after-ReadAll(body);
//   - returns the duration + any error;
//   - reuses the http.Client across calls (connection-pool friendly,
//     matches what real Spark/Trino clients do).
//
// The harness has no opinion on whether the URL is the Polaris
// testcontainer or the httptest gateway server — same shape, same
// timing methodology.
// ----------------------------------------------------------------------

// HTTPCommitFn returns a CommitFn that POSTs the given commit body to
// url with the supplied bearer token. The body is sent verbatim per
// call; callers that need to vary the body per call should wrap.
func HTTPCommitFn(client *http.Client, url string, body []byte, bearer string) CommitFn {
	return func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return 0, fmt.Errorf("HTTPCommitFn: build req: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, fmt.Errorf("HTTPCommitFn: do req: %w", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		dur := time.Since(start)
		if resp.StatusCode >= 400 {
			return dur, fmt.Errorf("HTTPCommitFn: status %d", resp.StatusCode)
		}
		return dur, nil
	}
}

// SyntheticCommitBody returns a JSON body shaped like an Iceberg REST
// commit (empty Requirements + empty Updates). Phase 1 polaris adapter
// only accepts empty Requirements/Updates (Plan 01-02 deferral —
// typed-dispatch lands in 01-06+); the gateway pipeline doesn't care
// about the body content (allow-all policy passes anything). This
// minimal shape lets both phases issue identical commits without
// per-phase divergence.
func SyntheticCommitBody() []byte {
	body, _ := json.Marshal(map[string]any{
		"requirements": []any{},
		"updates":      []any{},
	})
	return body
}
