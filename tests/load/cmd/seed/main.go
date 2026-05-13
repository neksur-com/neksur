// Command seed is the standalone runner for the Phase 0 envelope seed
// (10M nodes / 50M edges). Per 00-VALIDATION.md row 06-T1 / REQ-NFR-
// catalog-scale: "10M nodes / 50M edges fixture loadable in <30min".
//
// Usage:
//
//	go run ./tests/load/cmd/seed -assert-completes-under=30m
//
// Required environment:
//
//	DATABASE_URL — libpq DSN to a Postgres+AGE instance with V0010
//	               + V0020 + V0025 + V0030 migrations already applied.
//
// Optional environment:
//
//	NEKSUR_RUN_MIGRATIONS=1 — exec infra/migrations/run-migrations.sh
//	                          before seeding (useful for fresh fixtures
//	                          in CI; in production migrations are applied
//	                          by sqitch via the deploy runbook).
//
// Exit codes:
//
//	0 — seed completed under -assert-completes-under and node/edge counts
//	    are within ±5% of target; structured single-line summary on stdout.
//	1 — seed failed (preflight, COPY error, or assertion miss); structured
//	    JSON-shaped diagnostic on stderr suitable for `jq`-driven CI parse.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/tests/load"
)

func main() {
	var (
		assertUnder = flag.Duration("assert-completes-under", 30*time.Minute,
			"upper bound on seed wall-clock; non-zero exit if exceeded")
		targetNodes = flag.Int64("target-nodes", 10_000_000, "node count target (default = full envelope)")
		targetEdges = flag.Int64("target-edges", 50_000_000, "edge count target (default = full envelope)")
		tenants     = flag.Int("tenants", 1, "number of synthetic tenants to interleave")
	)
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		failf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *assertUnder+5*time.Minute)
	defer cancel()

	if os.Getenv("NEKSUR_RUN_MIGRATIONS") == "1" {
		// Best-effort migration apply — sqitch is the canonical driver
		// in production, but for CI fixtures we shell out to the helper
		// script. If the helper does not exist (e.g., new fork), we
		// surface the error via stderr and exit 1.
		cmd := exec.CommandContext(ctx, "bash", "infra/migrations/run-migrations.sh")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			failf("infra/migrations/run-migrations.sh: %v", err)
		}
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		failf("pgx.Connect: %v", err)
	}
	defer conn.Close(context.Background())

	// AGE LOAD is needed before any cypher() call; the seed itself
	// uses raw COPY against neksur.* tables which does NOT need AGE
	// to be loaded, but the pre-check queries ag_catalog.ag_label
	// which is registered by AGE — so we LOAD here for safety.
	if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
		failf("LOAD 'age': %v", err)
	}
	if _, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		failf("SET search_path: %v", err)
	}

	result, err := load.Seed(ctx, conn, load.SeedOpts{
		TargetNodes: *targetNodes,
		TargetEdges: *targetEdges,
		Tenants:     *tenants,
	})
	if err != nil {
		failf("Seed: %v", err)
	}

	// Assertion 1: wall-clock budget.
	if result.Duration > *assertUnder {
		failResultf(result, "seed took %s; --assert-completes-under=%s exceeded", result.Duration, *assertUnder)
	}

	// Assertion 2: ±5% target counts.
	tolerance := 0.05
	if !within(result.NodesCreated, *targetNodes, tolerance) {
		failResultf(result, "nodes_created=%d outside ±5%% of target=%d", result.NodesCreated, *targetNodes)
	}
	if !within(result.EdgesCreated, *targetEdges, tolerance) {
		failResultf(result, "edges_created=%d outside ±5%% of target=%d", result.EdgesCreated, *targetEdges)
	}

	// Single-line summary on stdout for easy CI scrape.
	fmt.Printf("seed PASS nodes=%d edges=%d duration=%s bytes=%d\n",
		result.NodesCreated, result.EdgesCreated, result.Duration, result.BytesWrittenEstimate)
}

func within(actual, target int64, tolerance float64) bool {
	if target == 0 {
		return actual == 0
	}
	delta := float64(actual-target) / float64(target)
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}

func failf(format string, args ...any) {
	out := map[string]any{
		"status":  "FAIL",
		"message": fmt.Sprintf(format, args...),
	}
	enc, _ := json.Marshal(out)
	fmt.Fprintln(os.Stderr, string(enc))
	os.Exit(1)
}

func failResultf(result load.SeedResult, format string, args ...any) {
	out := map[string]any{
		"status":         "FAIL",
		"message":        fmt.Sprintf(format, args...),
		"nodes_created":  result.NodesCreated,
		"edges_created":  result.EdgesCreated,
		"duration_ms":    result.Duration.Milliseconds(),
		"bytes_estimate": result.BytesWrittenEstimate,
	}
	enc, _ := json.Marshal(out)
	fmt.Fprintln(os.Stderr, string(enc))
	os.Exit(1)
}
