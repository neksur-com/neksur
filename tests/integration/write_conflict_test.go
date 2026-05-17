//go:build integration

// TestWriteConflictLWWAbortRetry — Wave-0 stub for REQ-write-conflict-coordination.
//
// This test will be fully implemented in Plan 03-10 (write-conflict coordination
// policies). The stub boots Phase3Fixture and skips until Plan 03-10 ships the
// write-coordinator conflict-resolution policies.
//
// Acceptance requirement (to be satisfied by Plan 03-10):
//   - Policy with write_conflict_policy='lww' (last-writer-wins):
//     two concurrent Trino INSERT jobs targeting the same table — the later
//     commit wins; the earlier commit is NOT rejected (HTTP 200 for both;
//     the Iceberg snapshot history shows both, but latest wins).
//   - Policy with write_conflict_policy='abort':
//     two concurrent Trino INSERT jobs — the first succeeds; the second
//     receives HTTP 409 Conflict from the write-coordinator.
//   - Policy with write_conflict_policy='retry-with-backoff' (DEFAULT):
//     concurrent writes succeed after backoff (no 409; eventual consistency).
//   - write_conflict_policy CHECK constraint (V0081) rejects unknown values.
//   - The write_conflict_policy column defaults to 'retry-with-backoff' for
//     tenants that don't set an explicit policy.
//
// REQ-write-conflict-coordination — lit by Plan 03-10.

package integration

import (
	"testing"
)

func TestWriteConflictLWWAbortRetry(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	// Variables that Plan 03-10 will populate with real values.
	var (
		_ = fx // suppress unused warning — fx is used in the real implementation
	)

	// --- Stub --- light up in Plan 03-10 ---
	t.Skipf("MISSING — implemented in Plan 03-10 (write-conflict policies): "+
		"lww/abort/retry-with-backoff policy enforcement at write-coordinator + "+
		"V0081 write_conflict_policy column wiring + license-feature-flag gating "+
		"for lww+abort (L2 features) (REQ-write-conflict-coordination)")
}
