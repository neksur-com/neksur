// Command footprint is the Phase 0 footprint measurement runner.
// Per 00-VALIDATION.md row 06-T2 / REQ-NFR-graph-ops-footprint:
// "Postgres footprint at envelope fits proportionally within 900GB
// Phase 1 cap" — Phase 0 (10% of envelope) target <200GB.
//
// Empirical A3 validation (00-RESEARCH.md §Assumptions) — A3 is
// confirmed by the value this CLI WRITES to tests/load/_footprint-baseline.json
// regardless of PASS/FAIL; ACCEPTANCE.md §Assumption Validation
// references this evidence file.
//
// Usage:
//
//	go run ./tests/load/cmd/footprint -assert-under-gb=200
//	go run ./tests/load/cmd/footprint -assert-under-gb=200 -breakdown
//
// Required environment:
//
//	DATABASE_URL — libpq DSN to the Postgres+AGE cluster the seed loaded.
//
// Outputs:
//
//	stdout — single-line PASS / FAIL summary with total bytes.
//	tests/load/_footprint-baseline.json — full per-relation breakdown
//	    (always written, on PASS or FAIL — this is the A3 evidence trail).
//	tests/load/_footprint-failure.csv — per-relation breakdown CSV
//	    (only written on FAIL, for triage).
//
// Exit codes:
//
//	0 — total Postgres footprint ≤ assert threshold.
//	1 — assertion miss or runtime error.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type relationSize struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

type baseline struct {
	MeasuredAt    string         `json:"measured_at"`
	TotalBytes    int64          `json:"total_bytes"`
	TotalGB       float64        `json:"total_gb"`
	AssertUnderGB int            `json:"assert_under_gb"`
	Status        string         `json:"status"`
	Relations     []relationSize `json:"relations"`
}

func main() {
	var (
		assertUnderGB = flag.Int("assert-under-gb", 200,
			"upper bound on total pg_database_size in GB; 200 = Phase 0 cap per A3 (00-RESEARCH §Assumptions)")
		breakdown = flag.Bool("breakdown", false,
			"also print per-relation breakdown to stdout")
	)
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		failf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		failf("pgx.Connect: %v", err)
	}
	defer conn.Close(context.Background())

	// 1) Total DB size — the headline assertion.
	var totalBytes int64
	if err := conn.QueryRow(ctx,
		`SELECT pg_database_size(current_database())::int8`).Scan(&totalBytes); err != nil {
		failf("pg_database_size: %v", err)
	}

	// 2) Per-relation breakdown — neksur.* tables and their indexes.
	rows, err := conn.Query(ctx, `
		SELECT relname::text, pg_total_relation_size(c.oid)::int8
		  FROM pg_class c
		  JOIN pg_namespace n ON c.relnamespace = n.oid
		 WHERE n.nspname = 'neksur'
		 ORDER BY pg_total_relation_size(c.oid) DESC
	`)
	if err != nil {
		failf("per-relation query: %v", err)
	}
	defer rows.Close()
	var relations []relationSize
	for rows.Next() {
		var r relationSize
		if err := rows.Scan(&r.Name, &r.SizeBytes); err != nil {
			failf("scan relation row: %v", err)
		}
		relations = append(relations, r)
	}
	if err := rows.Err(); err != nil {
		failf("rows.Err: %v", err)
	}

	threshold := int64(*assertUnderGB) * 1024 * 1024 * 1024
	status := "PASS"
	if totalBytes > threshold {
		status = "FAIL"
	}

	// 3) ALWAYS write the baseline JSON — this is A3's empirical
	// evidence trail and ACCEPTANCE.md references it directly.
	bl := baseline{
		MeasuredAt:    time.Now().UTC().Format(time.RFC3339),
		TotalBytes:    totalBytes,
		TotalGB:       float64(totalBytes) / (1024 * 1024 * 1024),
		AssertUnderGB: *assertUnderGB,
		Status:        status,
		Relations:     relations,
	}
	if err := writeBaselineJSON("tests/load/_footprint-baseline.json", bl); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: could not write baseline JSON: %v\n", err)
	}

	// 4) Print the headline summary.
	fmt.Printf("footprint %s total=%.2f GB threshold=%d GB relations=%d\n",
		status, bl.TotalGB, *assertUnderGB, len(relations))
	if *breakdown {
		for _, r := range relations {
			fmt.Printf("  %-50s %10.2f MB\n", r.Name, float64(r.SizeBytes)/(1024*1024))
		}
	}

	if status == "FAIL" {
		// Triage: per-relation CSV.
		if err := writeFailureCSV("tests/load/_footprint-failure.csv", relations); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: could not write failure CSV: %v\n", err)
		}
		failf("total footprint %.2f GB exceeds %d GB cap; per-relation report at tests/load/_footprint-failure.csv",
			bl.TotalGB, *assertUnderGB)
	}
}

func writeBaselineJSON(path string, bl baseline) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(bl)
}

func writeFailureCSV(path string, relations []relationSize) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"relation", "size_bytes", "size_mb"}); err != nil {
		return err
	}
	for _, r := range relations {
		if err := w.Write([]string{
			r.Name,
			strconv.FormatInt(r.SizeBytes, 10),
			fmt.Sprintf("%.2f", float64(r.SizeBytes)/(1024*1024)),
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
