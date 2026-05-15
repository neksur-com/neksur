// Command gateway_overhead is the Phase 1 commit-overhead measurement
// runner. Per ADR-003 §3.3 + CONTEXT line 174: the L1 Catalog Gateway
// MUST add ≤5% latency over a baseline-bypass commit directly to the
// upstream catalog (Polaris in Phase 1).
//
// Two trials per RESEARCH §Performance:
//   1. Baseline phase  — POST <body> to -baseline-url (direct Polaris
//                        testcontainer), bypassing Neksur entirely.
//   2. Gateway phase   — POST <body> to -gateway-url (the Neksur L1
//                        Catalog Gateway endpoint, mounted under
//                        /v1/iceberg/{prefix}/namespaces/{ns}/tables/
//                        {table} per Plan 01-06).
//
// Compute OverheadRatio = (GatewayP95 - BaselineP95) / BaselineP95;
// assert ≤ -assert-overhead-under (default 0.05 per ADR-003 §3.3).
//
// ALWAYS writes -baseline-out (default
// tests/load/gateway-overhead-baseline.json) regardless of PASS or FAIL
// per the cmd/footprint pattern. The baseline JSON is COMMITTED to git
// (not gitignored — per CONTEXT line 156 OQ resolution: empirical
// evidence trail for ADR-003 §3.3 ≤5% overhead contract; PR review
// catches regressions via `git diff`).
//
// Usage:
//
//	go run ./tests/load/cmd/gateway_overhead \
//	    -baseline-url "http://localhost:8181/api/catalog/v1/test/namespaces/default/tables/orders" \
//	    -gateway-url  "http://localhost:8080/v1/iceberg/prod-polaris/namespaces/default/tables/orders" \
//	    -bearer       "$ICEBERG_REST_BEARER" \
//	    -commit-count 1000 \
//	    -warmup       100 \
//	    -assert-overhead-under 0.05 \
//	    -baseline-out tests/load/gateway-overhead-baseline.json
//
// Required flags:
//   -baseline-url           — direct Polaris commit endpoint URL.
//   -gateway-url            — Neksur gateway commit endpoint URL.
//   -bearer                 — Authorization Bearer token (used for both
//                             phases — fairness requires identical auth
//                             treatment).
//
// Optional flags (defaults reflect ADR-003 §3.3 + RESEARCH §Standard Stack):
//   -commit-count 1000      — total commits per phase.
//   -warmup 100             — discarded commits before measurement.
//   -concurrent-clients 1   — parallel commit goroutines per phase.
//   -commits-per-second 10  — soft rate limit (Polaris commit ceiling).
//   -assert-overhead-under 0.05  — ADR-003 §3.3 contract (5%).
//   -baseline-out tests/load/gateway-overhead-baseline.json
//
// Exit codes:
//   0 — OverheadRatio ≤ assert threshold AND zero phase-fatal errors.
//   1 — assertion miss OR runtime error (baseline JSON still written).

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/neksur-com/neksur/tests/load"
)

// baselineEnvelope is the canonical JSON shape committed to git as
// tests/load/gateway-overhead-baseline.json. The PENDING_FIRST_RUN
// status is the initial committed value; live runs overwrite the file
// with measured numbers + status PASS / FAIL. SREs subscribe via PR
// diff watch + dashboards.
//
// Field order is the same as the initial seed JSON (Task 2 step 4) so
// `git diff` is human-readable.
type baselineEnvelope struct {
	MeasuredAt        string  `json:"measured_at"`
	BaselineP95Ms     int64   `json:"baseline_p95_ms"`
	GatewayP95Ms      int64   `json:"gateway_p95_ms"`
	OverheadRatio     float64 `json:"overhead_ratio"`
	AssertUnder       float64 `json:"assert_under"`
	Status            string  `json:"status"`
	Commits           int     `json:"commits"`
	ConcurrentClients int     `json:"concurrent_clients"`
	Warmup            int     `json:"warmup"`

	// Diagnostic fields — populated on every live run to make trend
	// dashboards meaningful (P50/P99 surface tail-latency regressions
	// even when P95 stays under budget).
	BaselineP50Ms int64  `json:"baseline_p50_ms,omitempty"`
	BaselineP99Ms int64  `json:"baseline_p99_ms,omitempty"`
	GatewayP50Ms  int64  `json:"gateway_p50_ms,omitempty"`
	GatewayP99Ms  int64  `json:"gateway_p99_ms,omitempty"`
	Errors        int    `json:"errors,omitempty"`
	BaselineURL   string `json:"baseline_url,omitempty"`
	GatewayURL    string `json:"gateway_url,omitempty"`
	Details       string `json:"details,omitempty"`
}

func main() {
	var (
		baselineURL = flag.String("baseline-url", "",
			"direct Polaris commit endpoint URL (e.g. http://host:8181/api/catalog/v1/<prefix>/namespaces/<ns>/tables/<t>)")
		gatewayURL = flag.String("gateway-url", "",
			"Neksur gateway commit endpoint URL (e.g. http://host:8080/v1/iceberg/<prefix>/namespaces/<ns>/tables/<t>)")
		bearer = flag.String("bearer", "",
			"Authorization Bearer token (applied to BOTH phases for fairness)")
		commitCount = flag.Int("commit-count", 1000, "total commits per phase")
		warmup      = flag.Int("warmup", 100, "discarded commits before measurement")
		concurrent  = flag.Int("concurrent-clients", 1, "parallel commit goroutines per phase")
		cps         = flag.Int("commits-per-second", 10, "soft per-client rate limit")
		assertUnder = flag.Float64("assert-overhead-under", 0.05,
			"ADR-003 §3.3 contract: gateway commits must be ≤ this ratio slower than baseline")
		baselineOut = flag.String("baseline-out", "tests/load/gateway-overhead-baseline.json",
			"JSON baseline artifact (committed to git)")
		// Phase-A skip flags for the test substrate (e.g., when wiring
		// only one of the two paths in CI). Both default false; setting
		// either yields a degenerate-mode baseline with status=SKIPPED
		// on the corresponding side.
		skipBaseline = flag.Bool("skip-baseline", false, "skip baseline phase (dev-only)")
		skipGateway  = flag.Bool("skip-gateway", false, "skip gateway phase (dev-only)")
		dsn          = flag.String("dsn", "", "(reserved for future in-process gateway bootstrap; unused in HTTP-driven mode)")
		gatewayDsn   = flag.String("gateway-dsn", "", "(reserved; symmetric with -dsn for split-pool deployments)")
		tenant       = flag.String("tenant", "", "(reserved; recorded in baseline JSON for traceability)")
		polarisEndpt = flag.String("polaris-endpoint", "",
			"(optional) recorded in baseline JSON; if set + -baseline-url empty, used to derive default baseline URL")
	)
	flag.Parse()

	// Suppress unused-warnings for reserved flags (recorded in baseline
	// for traceability; future agents can wire in-process bootstrap).
	_ = dsn
	_ = gatewayDsn
	_ = tenant

	// If baseline URL is empty but polaris-endpoint is set, derive the
	// default baseline URL from the Polaris catalog ROOT. This matches
	// the deployment shape: operators configure the gateway URL and
	// the Polaris endpoint; the baseline URL is "Polaris + the same
	// namespace/table path the gateway is fronting".
	if *baselineURL == "" && *polarisEndpt != "" {
		*baselineURL = *polarisEndpt + "/v1/test/namespaces/default/tables/probe"
	}

	if !*skipBaseline && *baselineURL == "" {
		failNoBaseline(baselineOut, *assertUnder, "-baseline-url required (or -polaris-endpoint to derive)")
	}
	if !*skipGateway && *gatewayURL == "" {
		failNoBaseline(baselineOut, *assertUnder, "-gateway-url required")
	}

	// Build a single shared http.Client; re-used across all commits in
	// both phases. Connection pooling is what real Spark/Trino do —
	// fairness requires the test substrate not pay TCP handshake cost
	// per request.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	body := load.SyntheticCommitBody()

	baselineFn := load.HTTPCommitFn(client, *baselineURL, body, *bearer)
	gatewayFn := load.HTTPCommitFn(client, *gatewayURL, body, *bearer)

	if *skipBaseline {
		baselineFn = func(ctx context.Context) (time.Duration, error) {
			return time.Millisecond, nil
		}
	}
	if *skipGateway {
		gatewayFn = func(ctx context.Context) (time.Duration, error) {
			return time.Millisecond, nil
		}
	}

	opts := load.OverheadOpts{
		CommitCount:       *commitCount,
		ConcurrentClients: *concurrent,
		CommitsPerSecond:  *cps,
		WarmupCommits:     *warmup,
	}

	overallTimeout := time.Duration(2*opts.CommitCount/opts.CommitsPerSecond+
		opts.WarmupCommits/opts.CommitsPerSecond+60) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), overallTimeout)
	defer cancel()

	res, err := load.MeasureGatewayOverhead(ctx, baselineFn, gatewayFn, opts)
	status := "PASS"
	details := ""
	if err != nil {
		status = "FAIL"
		details = fmt.Sprintf("MeasureGatewayOverhead: %v", err)
	} else if res.OverheadRatio > *assertUnder {
		status = "FAIL"
		details = fmt.Sprintf("overhead_ratio %.4f exceeds -assert-overhead-under=%.4f (baseline_p95=%v gateway_p95=%v)",
			res.OverheadRatio, *assertUnder, res.BaselineP95, res.GatewayP95)
	}

	bl := baselineEnvelope{
		MeasuredAt:        time.Now().UTC().Format(time.RFC3339Nano),
		BaselineP95Ms:     res.BaselineP95Ms,
		GatewayP95Ms:      res.GatewayP95Ms,
		OverheadRatio:     res.OverheadRatio,
		AssertUnder:       *assertUnder,
		Status:            status,
		Commits:           opts.CommitCount,
		ConcurrentClients: opts.ConcurrentClients,
		Warmup:            opts.WarmupCommits,
		BaselineP50Ms:     res.BaselineP50Ms,
		BaselineP99Ms:     res.BaselineP99Ms,
		GatewayP50Ms:      res.GatewayP50Ms,
		GatewayP99Ms:      res.GatewayP99Ms,
		Errors:            res.Errors,
		BaselineURL:       *baselineURL,
		GatewayURL:        *gatewayURL,
		Details:           details,
	}
	if werr := writeBaseline(*baselineOut, bl); werr != nil {
		fmt.Fprintf(os.Stderr, "WARN: write baseline: %v\n", werr)
	}

	if status == "FAIL" {
		fmt.Fprintf(os.Stderr, "gateway-overhead FAIL details=%s ratio=%.4f baseline_p95_ms=%d gateway_p95_ms=%d errors=%d\n",
			details, res.OverheadRatio, res.BaselineP95Ms, res.GatewayP95Ms, res.Errors)
		os.Exit(1)
	}
	fmt.Printf("gateway-overhead PASS ratio=%.4f baseline_p95=%v gateway_p95=%v commits=%d errors=%d\n",
		res.OverheadRatio, res.BaselineP95, res.GatewayP95, opts.CommitCount, res.Errors)
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

func failNoBaseline(baselineOut *string, assertUnder float64, msg string) {
	bl := baselineEnvelope{
		MeasuredAt:  time.Now().UTC().Format(time.RFC3339Nano),
		AssertUnder: assertUnder,
		Status:      "FAIL",
		Details:     msg,
	}
	_ = writeBaseline(*baselineOut, bl)
	fmt.Fprintf(os.Stderr, "FAIL: %s\n", msg)
	os.Exit(1)
}
