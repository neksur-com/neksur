//go:build integration

// TestSchemaCacheBroadcastSLA30s — Wave-0 stub for REQ-schema-cache-invalidation.
//
// This test will be fully implemented in Plan 03-07 (schema-cache invalidation
// broadcaster). The stub boots Phase3Fixture and skips the assertion body until
// Plan 03-07 implements the LISTEN consumer + per-engine cache invalidation.
//
// Acceptance requirement (to be satisfied by Plan 03-07):
//   - Inserting a Iceberg snapshot with operation='schema_change' on the per-
//     tenant snapshots table triggers the V0080 notify_schema_changed() trigger,
//     which emits a pg_notify on the 'schema_changed' channel.
//   - The broadcaster service (internal/coordination/cache/broadcaster.go)
//     LISTENS on 'schema_changed' and, upon receiving the notification, fires a
//     cache invalidation to all registered engines for the affected (tenant_id,
//     table_id) pair.
//   - Each engine's per-table schema cache is invalidated and the engine
//     re-fetches the schema on its next query.
//   - The full cycle (snapshot INSERT → pg_notify → broadcaster → engine
//     invalidation → engine re-fetch) completes within ≤30s SLA per ROADMAP
//     §3 SC §2.
//
// REQ-schema-cache-invalidation — lit by Plan 03-07.

package integration

import (
	"testing"
)

func TestSchemaCacheBroadcastSLA30s(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	// Variables that Plan 03-07 will populate with real values.
	var (
		_ = fx // suppress unused warning — fx is used in the real implementation
	)

	// --- Stub --- light up in Plan 03-07 ---
	t.Skipf("MISSING — implemented in Plan 03-07 (schema-cache invalidation broadcaster): "+
		"LISTEN on 'schema_changed' + per-engine cache invalidation goroutines + "+
		"commit-to-visible ≤30s SLA measurement (REQ-schema-cache-invalidation)")
}
