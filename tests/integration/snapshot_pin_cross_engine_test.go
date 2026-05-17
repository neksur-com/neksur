//go:build integration

// TestSnapshotPinCrossEngine — Wave-0 stub for REQ-snapshot-pinning.
//
// This test will be fully implemented in Plan 03-06 (snapshot pinning service).
// The stub boots Phase3Fixture so the infrastructure is verified, then
// calls t.Skipf to skip the assertion body until Plan 03-06 implements
// the SnapshotPin vlabel MERGE + READ edge property propagation.
//
// Acceptance requirement (to be satisfied by Plan 03-06):
//   - A named pin (pin_name, at_snapshot_id) is recorded as a SnapshotPin
//     node + PINS edge in the AGE graph.
//   - All four engines (Spark, Trino, Dremio, Snowflake-via-Polaris) reading
//     the pinned table receive data consistent with the pinned snapshot_id
//     rather than the latest snapshot.
//   - Pin is enforced for all engines within ≤30s of pin creation (SLA from
//     ROADMAP §3 SC §3).
//   - Pin expiry (expiry_utc < now()) causes the pin node to be swept by the
//     daily orphan sweep; pinned constraint removed from READ edges.
//
// REQ-snapshot-pinning — lit by Plan 03-06.

package integration

import (
	"testing"
)

func TestSnapshotPinCrossEngine(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	// Fixture-level sanity: Phase3Fixture is ready if we reach this point.
	if fx.Dremio == nil {
		t.Log("TestSnapshotPinCrossEngine: Dremio container is nil — unlikely; fixture boot should have failed")
	}
	if fx.Glue == nil {
		t.Log("TestSnapshotPinCrossEngine: Glue container is nil — unexpected; fixture boot should have failed")
	}

	// --- Stub --- light up in Plan 03-06 ---
	t.Skipf("MISSING — implemented in Plan 03-06 (snapshot pinning service): "+
		"pin.go UpsertSnapshotPin + PINS elabel MERGE + READ edge {pinned, pinned_by, at_snapshot} "+
		"propagation + 4-way cross-engine enforcement within ≤30s SLA (REQ-snapshot-pinning)")
}
