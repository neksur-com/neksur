//go:build integration

// TestPartitionSpecEvolutionCrossEngine — Wave-0 stub for REQ-partition-spec-versioning.
//
// This test will be fully implemented in Plan 03-08 (partition spec versioning).
// The stub boots Phase3Fixture and skips until Plan 03-08 ships the PartitionSpec
// vlabel MERGE + USES_SPEC edge management.
//
// Acceptance requirement (to be satisfied by Plan 03-08):
//   - An Iceberg table evolves from partition spec v1 (year(ts)) to spec v2
//     (year(ts), month(ts)). The evolution is recorded as two PartitionSpec
//     nodes in the AGE graph with a USES_SPEC edge pointing from the table
//     to the current (active) spec.
//   - Engines reading the table are auto-routed to the correct spec: engines
//     that have not re-read metadata yet are served spec v1 files; engines that
//     re-read after evolution get spec v2 file listing.
//   - The schema_changed notification (V0080 trigger via operation=
//     'add_partition_spec' or 'replace_partition_spec') fires on spec evolution
//     so the broadcaster invalidates engine caches within ≤30s SLA.
//   - Historical data written against spec v1 is still readable via spec v1
//     partition layout (no data loss, no rewrite required).
//   - `graph.MustSanitizeCypherLiteral` is applied to all PartitionSpec
//     property values (table_id, spec_id) per AGE 1.6 CR-01 invariant.
//
// REQ-partition-spec-versioning — lit by Plan 03-08.

package integration

import (
	"testing"
)

func TestPartitionSpecEvolutionCrossEngine(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	// Variables that Plan 03-08 will populate with real values.
	var (
		_ = fx // suppress unused warning — fx is used in the real implementation
	)

	// --- Stub --- light up in Plan 03-08 ---
	t.Skipf("MISSING — implemented in Plan 03-08 (partition spec versioning): "+
		"PartitionSpec vlabel MERGE + USES_SPEC edge management + spec v1→v2 "+
		"cross-engine read routing + schema_changed emit on spec evolution "+
		"(REQ-partition-spec-versioning)")
}
