// Package load hosts the Phase 0 envelope load-seed fixture — STUB.
//
// The Phase 0 acceptance envelope is 10M nodes / 50M edges (per
// ADR-001 and ROADMAP.md Phase 0 §Phase Details). Seed will use the
// pgx COPY-then-MERGE pattern from 00-RESEARCH.md §Code Examples
// (since AGEFreighter — the original Python helper — is not available
// in Go; the COPY+MERGE pattern works directly through pgx).
//
// Real implementation lands in Plan 06 (Wave 5 — load envelope). This
// stub exists so chaos / DR drill scripts that consume the seed output
// (kill-primary-mid-load in Plan 04, etc.) can import their upstream
// dependency without circular failure.
//
// Originally Python's tests/load/fixtures/phase0_envelope_seed.py under
// the Wave 0 plan; now Go per the 2026-05-13 D-PHASE0-stack correction.
package load

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotImplemented is the sentinel returned by Seed. Plan 06 replaces
// the body with the real COPY-then-MERGE seed implementation.
var ErrNotImplemented = errors.New("filled by Plan 06 — Wave 5 envelope load")

// SeedOpts controls the Seed run. Defaults give the full Phase 0
// envelope; tests may request a smaller fraction for fast smoke runs.
type SeedOpts struct {
	TargetNodes int64 // default 10_000_000
	TargetEdges int64 // default 50_000_000
}

// SeedResult is the seed run's output. Plan 06 will populate all three
// fields; callers (chaos drills, throughput acceptance gates) read
// NodesCreated + EdgesCreated for verification and Duration for the
// >=100k nodes/s sustained-throughput assertion.
type SeedResult struct {
	NodesCreated int64
	EdgesCreated int64
	Duration     time.Duration
}

// Seed materialises synthetic Phase 0 envelope data on conn. Plan 06
// will fill this in; for now it returns the stub sentinel so callers
// can build against the API surface before W5 lands.
func Seed(ctx context.Context, conn *pgx.Conn, opts SeedOpts) (SeedResult, error) {
	_ = ctx
	_ = conn
	_ = opts
	return SeedResult{}, ErrNotImplemented
}
