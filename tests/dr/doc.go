// Package dr is the disaster-recovery test surface for the Neksur
// metadata-graph foundation (Phase 0 Wave 3, Plan 00-04).
//
// All functional code in this package is gated behind the `dr`
// build tag — see `wal_throughput.go`, `wal_throughput_test.go`,
// and `dr_targets_test.go`. Run the suite with:
//
//	go test -tags dr -timeout 90m ./tests/dr/...
//
// This file is intentionally untagged so that `go build ./tests/dr/...`
// and `go vet ./tests/dr/...` (without the dr tag) discover the
// package and return exit 0 (vet errors out with "no packages to
// vet" if every file in the directory is excluded by build tags).
//
// Cross-reference: D-OQ.04 (LOCKED) — RTO 1h / RPO 15min unified
// across all metadata stores; supersedes ADR-001 D-001.13.
package dr
