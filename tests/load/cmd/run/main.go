// Command run is the latency runner for the Phase 0 acceptance gate
// (Wave 5 / Plan 00-06 Task 2). It drives the workload generator from
// tests/load/cypher_workload.go for a fixed window at the REQ-NFR-
// catalog-scale concurrency (50 workers) and asserts a P95 budget per
// query kind.
//
// Per 00-VALIDATION.md §Per-Task Verification Map (W5 rows for
// REQ-NFR-graph-latency):
//
//   - SingleNode P95 budget: <50ms
//   - OneHop     P95 budget: <150ms
//   - ThreeHop   P95 budget: <1200ms
//
// Usage:
//
//	go run ./tests/load/cmd/run -query=single-node -assert-p95=50  -with-full-fixture
//	go run ./tests/load/cmd/run -query=one-hop     -assert-p95=150 -with-full-fixture
//	go run ./tests/load/cmd/run -query=three-hop   -assert-p95=1200 -with-full-fixture
//
// Required environment:
//
//	DATABASE_URL — libpq DSN to a Postgres+AGE cluster.
//
// Without -with-full-fixture the runner seeds a 100K node / 500K edge
// dev fixture via load.Seed first (~2-3 min). With -with-full-fixture
// the runner ASSUMES the 10M/50M envelope is already loaded (operator
// invokes ./tests/load/cmd/seed beforehand).
//
// Exit codes:
//
//	0 — measured P95 ≤ -assert-p95; one-line summary on stdout.
//	1 — measured P95 > -assert-p95 or runtime error; full samples
//	    written to tests/load/_latency-<query>-failure.csv for triage.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/tests/load"
)

func main() {
	var (
		queryKind = flag.String("query", "",
			"query shape; one of: single-node, one-hop, three-hop (required)")
		assertP95 = flag.Int("assert-p95", 0,
			"upper bound on observed P95 in milliseconds (required)")
		withFullFixture = flag.Bool("with-full-fixture", false,
			"if set, assume the 10M/50M seed is already loaded; otherwise seed a 100K/500K dev fixture first")
		duration = flag.Duration("duration", 60*time.Second,
			"workload window (default 60s)")
		qps = flag.Int("qps", 500,
			"target queries per second across all 50 workers (per-worker QPS = qps/50)")
	)
	flag.Parse()

	if *queryKind != "single-node" && *queryKind != "one-hop" && *queryKind != "three-hop" {
		failf("-query must be one of single-node, one-hop, three-hop; got %q", *queryKind)
	}
	if *assertP95 <= 0 {
		failf("-assert-p95 must be a positive integer (milliseconds)")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		failf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration+15*time.Minute)
	defer cancel()

	// Optional dev-fixture seed pass — only when -with-full-fixture is
	// absent. When the full envelope is preloaded the operator skips
	// this branch entirely (it would be a no-op against an existing
	// 10M-row table — Plan 06 deliberately does NOT layer MERGE, so
	// re-seeding under full-fixture would spew duplicate-key errors).
	if !*withFullFixture {
		seedConn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			failf("pgx.Connect (seed): %v", err)
		}
		if _, err := seedConn.Exec(ctx, "LOAD 'age'"); err != nil {
			failf("LOAD 'age' (seed): %v", err)
		}
		if _, err := seedConn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
			failf("SET search_path (seed): %v", err)
		}
		_, err = load.Seed(ctx, seedConn, load.SeedOpts{
			TargetNodes: 100_000,
			TargetEdges: 500_000,
			Tenants:     1,
		})
		_ = seedConn.Close(context.Background())
		if err != nil {
			failf("dev-fixture seed: %v", err)
		}
	}

	// Build the production GraphClient — same code path the latency
	// runner exercises in /gsd-verify-work; this routes through
	// internal/graph.ExecuteCypher so D-001.14 Prometheus collectors
	// emit alongside the application-boundary measurement.
	client, err := graph.NewGraphClient(ctx, dsn)
	if err != nil {
		failf("NewGraphClient: %v", err)
	}
	defer client.Close()

	// Workload mix: 100% the chosen kind so P95 is measured per shape.
	mix := map[string]float64{*queryKind: 1.0}
	samples, err := load.GenerateWorkload(ctx, client, *qps, *duration, mix)
	if err != nil {
		failf("GenerateWorkload: %v", err)
	}
	if len(samples) == 0 {
		failf("workload produced zero samples; check DATABASE_URL connectivity / graph schema")
	}

	// Sort durations to compute P50/P95/P99.
	durations := make([]time.Duration, 0, len(samples))
	errCount := 0
	for _, s := range samples {
		durations = append(durations, s.Duration)
		if s.Err != nil {
			errCount++
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[int(0.50*float64(len(durations)))]
	p95 := durations[int(0.95*float64(len(durations)))]
	p99 := durations[int(0.99*float64(len(durations)))]

	// Always print the summary (matches the structured-output convention
	// across the W5 CLIs — so even on FAIL the human reader sees the
	// distribution shape directly).
	fmt.Printf("%s p50=%v p95=%v p99=%v n=%d errors=%d\n",
		*queryKind, p50, p95, p99, len(samples), errCount)

	if p95 > time.Duration(*assertP95)*time.Millisecond {
		// Triage path: dump every sample to CSV.
		csvPath := fmt.Sprintf("tests/load/_latency-%s-failure.csv", *queryKind)
		if err := writeFailureCSV(csvPath, samples); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: could not write %s: %v\n", csvPath, err)
		}
		failf("P95 %v exceeds budget %dms; samples written to %s",
			p95, *assertP95, csvPath)
	}
}

func writeFailureCSV(path string, samples []load.WorkloadSample) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"index", "kind", "duration_ms", "error"}); err != nil {
		return err
	}
	for i, s := range samples {
		errStr := ""
		if s.Err != nil {
			errStr = s.Err.Error()
		}
		if err := w.Write([]string{
			strconv.Itoa(i),
			s.Kind,
			strconv.FormatInt(s.Duration.Milliseconds(), 10),
			errStr,
		}); err != nil {
			return err
		}
	}
	return nil
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
