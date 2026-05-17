//go:build integration

// TestCompactionSnapshotRetentionGuard — Wave-0 stub for REQ-compaction-coordinator.
//
// This test will be fully implemented in Plan 03-12 (compaction coordinator).
// The stub boots Phase3Fixture and skips until Plan 03-12 ships the snapshot-
// retention guard (extending Iceberg's ExpireSnapshots API with SnapshotPin checks).
//
// Acceptance requirement (to be satisfied by Plan 03-12):
//   - An active SnapshotPin (pin_name, at_snapshot_id, expiry_utc > now())
//     blocks compaction (ExpireSnapshots) on the pinned snapshot_id.
//   - CompactionBlockedTotal{reason="active_pin", tenant_id, table_id_short}
//     metric is incremented when the guard fires (Pitfall 11: table_id_short
//     is the first 8 chars via observability.TableIDShort()).
//   - An expired SnapshotPin (expiry_utc <= now()) does NOT block compaction;
//     the daily orphan sweep removes the expired pin node.
//   - When no active pins exist, compaction proceeds normally (no guard latency
//     penalty).
//   - Cost tracking: compaction coordinator emits a compaction_duration_seconds
//     metric per compaction run (Phase 03-12 plans this as a further observable).
//
// REQ-compaction-coordinator — lit by Plan 03-12.

package integration

import (
	"testing"
)

func TestCompactionSnapshotRetentionGuard(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	// Variables that Plan 03-12 will populate with real values.
	var (
		_ = fx // suppress unused warning — fx is used in the real implementation
	)

	// --- Stub --- light up in Plan 03-12 ---
	t.Skipf("MISSING — implemented in Plan 03-12 (compaction coordinator): "+
		"snapshot-retention guard querying SnapshotPin graph nodes before "+
		"forwarding ExpireSnapshots; CompactionBlockedTotal metric; orphan pin "+
		"sweep for expired pins (REQ-compaction-coordinator)")
}
