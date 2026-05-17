// Package iceberg — write-coordinator pre-commit hook.
//
// WriteCoordinatorPreCommit slots into the Phase 1 CommitHandler 10-step
// pipeline between Step 8 (LoadTable) and Step 9 (policy fetch) per D-3.03
// and 03-RESEARCH §Pattern 3 lines 326-360 / §Code Example 3 lines 773-851.
//
// Engine detection is best-effort via User-Agent (see detectEngineKind).
// The hook fires ONLY for engineKind in {"trino", "dremio"} — all other
// engines (Spark, Unity, Glue, Snowflake-via-Polaris) pass through unchanged.
//
// All three Phase-3 stores are nilable so the same binary works across L1
// (BSL Core), L1+L2 (commercial), and L1+L2+L3 (enterprise) builds:
//   - PinStore         — L1, always non-nil in production (not consulted here;
//     used by the POST /pin gateway endpoint in handler.go).
//   - WriteConflictStore — L2, nil in L1-only binary.
//   - PartitionSpecStore — L3, nil in L1 and L2 binaries.
//
// Per Pitfall 11: this file never logs the commit body. Error returns wrap
// iceberg sentinels only (ErrPartitionSpecMismatch, or generic wrapped errs).
package iceberg

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg/polaris"
)

// PartitionSpecStore is the narrow interface Plan 03-09 needs from the L3
// enterprise PartitionSpec store. The concrete type
// (coordination/partitionspec.Store) satisfies this interface; nil is the
// L1/L2 "skip" sentinel.
type PartitionSpecStore interface {
	// LoadActive returns the currently-active PartitionSpec for the table.
	// Returns an error only when the store is reachable but the query fails.
	// When the table has no partition-spec record yet, return the zero-value
	// PartitionSpec (SpecID=0) — specMatches will accept any incoming spec_id.
	LoadActive(ctx context.Context, ref iceberg.TableRef) (*iceberg.PartitionSpec, error)
}

// WriteConflictStore is the narrow interface Plan 03-09 needs from the L2
// commercial WriteConflict store. The concrete type
// (coordination/writeconflict.Store) satisfies this interface; nil is the
// L1-only "skip" sentinel.
type WriteConflictStore interface {
	// LoadForTable returns the per-table write_conflict_policy string.
	// The returned policy is one of {"lww", "abort", "retry-with-backoff"}.
	// An error is returned only when the store is reachable but the query
	// fails; "no policy configured" returns ("lww", nil) as the default.
	LoadForTable(ctx context.Context, ref iceberg.TableRef) (string, error)
}

// ctxConflictKey is the unexported context key type for the per-request
// conflict policy value. Using a private struct type prevents key collisions
// with other packages.
type ctxConflictKey struct{}

// withConflictPolicy returns a new context carrying the per-table conflict
// policy string. Downstream adapter.CommitTable (Plan 03-10 retry.go) reads
// this value via WriteConflictPolicyFromContext.
func withConflictPolicy(ctx context.Context, policy string) context.Context {
	return context.WithValue(ctx, ctxConflictKey{}, policy)
}

// WriteConflictPolicyFromContext reads the per-table conflict policy attached
// by WriteCoordinatorPreCommit. Returns ("", false) when no policy is attached
// (L1-only binary or engine passthrough path).
func WriteConflictPolicyFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxConflictKey{}).(string)
	return v, ok
}

// detectEngineKind extracts the engine kind from the request User-Agent header.
// Returns "trino", "dremio", "spark", or "" (empty for unrecognized engines).
//
// Engine detection is best-effort and intentionally spoofable (see threat model
// T-3-write-coordinator-bypass). Spoofing User-Agent to "spark" skips the
// write-coordinator but does NOT bypass Step 9 policy fetch or the L1 advanced
// policy evaluation path.
//
// Matching is case-insensitive substring match per 03-RESEARCH §Pattern 3 lines
// 791-805. "iceberg-spark" is matched first before the plain "spark" fallback
// (both return "spark" — ordering matters only for readability).
func detectEngineKind(r *http.Request) string {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	switch {
	case strings.Contains(ua, "trino"):
		return "trino"
	case strings.Contains(ua, "dremio"):
		return "dremio"
	case strings.Contains(ua, "iceberg-spark") || strings.Contains(ua, "spark"):
		return "spark"
	default:
		// Unity / Glue / Snowflake-via-Polaris flow through their own adapters.
		return ""
	}
}

// specMatches reports whether the commit's PartitionSpec matches the
// table's canonically-active spec. A match requires identical SpecID.
//
// When activeSpec is nil or has SpecID=0 (no spec record in the enterprise
// store yet) the check is skipped and true is returned — this is the correct
// behaviour for tables that pre-date the L3 partition-spec versioning feature.
func specMatches(commit iceberg.PartitionSpec, active *iceberg.PartitionSpec) bool {
	if active == nil || active.SpecID == 0 {
		return true
	}
	return commit.SpecID == active.SpecID
}

// WriteCoordinatorPreCommit is the pre-commit hook that fires between Step 8
// (LoadTable) and Step 9 (policy fetch) in handler.go's CommitHandler pipeline.
//
// Returns (ctx, nil) to proceed to Step 9. Returns (ctx, err) with a wrapped
// sentinel to short-circuit:
//   - iceberg.ErrPartitionSpecMismatch → handler emits 403 +
//     metrics.ReasonPolicyPartitionSpecMismatch
//   - generic err from store → handler emits 503 +
//     metrics.ReasonPolicyEngineUnavailable
//
// ctx is always returned (may be the same as input or enriched with the
// conflict policy) so the caller can replace its local ctx with the returned
// value.
//
// Per Pitfall 11: never logs commit body. Only the ref + sentinel are logged
// by the caller.
func WriteCoordinatorPreCommit(
	ctx context.Context,
	deps Deps,
	engineKind string,
	ref iceberg.TableRef,
	current *iceberg.TableMetadata,
	req iceberg.CommitRequest,
) (context.Context, error) {
	// Hook fires only for Trino + Dremio per D-3.03.
	if engineKind != "trino" && engineKind != "dremio" {
		return ctx, nil
	}

	// L1-only binary path: when both Phase-3 stores are nil the hook is a
	// no-op. The gating condition in handler.go (`if deps.WriteConflictStore
	// != nil || deps.PartitionSpecStore != nil`) prevents this function from
	// being called at all in L1 binaries, but we guard here defensively.
	if deps.PartitionSpecStore == nil && deps.WriteConflictStore == nil {
		return ctx, nil
	}

	// 1. Active partition spec check — L3 enterprise builds only.
	if deps.PartitionSpecStore != nil {
		activeSpec, err := deps.PartitionSpecStore.LoadActive(ctx, ref)
		if err != nil {
			return ctx, fmt.Errorf("write-coordinator: load partition spec: %w", err)
		}
		// Determine which spec ID the commit is claiming. If current metadata
		// is available, compare against the metadata's spec; otherwise accept.
		commitSpec := iceberg.PartitionSpec{}
		if current != nil {
			commitSpec = current.PartitionSpec
		}
		if !specMatches(commitSpec, activeSpec) {
			return ctx, fmt.Errorf(
				"write-coordinator: partition spec mismatch (commit spec_id=%d, active spec_id=%d): %w",
				commitSpec.SpecID, activeSpec.SpecID, iceberg.ErrPartitionSpecMismatch,
			)
		}
	}

	// 2. Per-table write_conflict_policy — L2 commercial builds only.
	if deps.WriteConflictStore != nil {
		policy, err := deps.WriteConflictStore.LoadForTable(ctx, ref)
		if err != nil {
			return ctx, fmt.Errorf("write-coordinator: load conflict policy: %w", err)
		}
		ctx = withConflictPolicy(ctx, policy)
	}

	return ctx, nil
}

// isCommitConflictForTest is a thin test-accessible wrapper around
// polaris.IsCommitConflict that lets the gateway package's unit tests call the
// newly-exported symbol (B-2) without importing the polaris package directly
// in the test file. Production callers (Plan 03-10 retry.go) import
// polaris.IsCommitConflict directly.
//
// This helper is defined here (not in _test.go) because it needs to be
// accessible from writecoordinator_test.go (same package — package iceberg).
// The linter will flag it as "dead code" in production builds since no
// non-test caller exists; that is expected and intentional.
func isCommitConflictForTest(err error) bool {
	return polaris.IsCommitConflict(err)
}
