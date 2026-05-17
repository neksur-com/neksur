//go:build integration

// Plan 03-01 Wave-0 BLOCKING — Phase 3 migrations applied per tenant.
//
// TestPhase3MigrationsAppliedPerTenant asserts that:
//   * V0050 + V0051 + V0052 graph migrations land (28 vlabels + 42
//     elabels after the per-tenant Apply* loop runs).
//   * V0080 + V0081 relational migrations land (per-tenant
//     schema_changed trigger on snapshots + write_conflict_policy column
//     on policies).
//   * RLS is forced on the 3 new vlabels (SnapshotPin, PartitionSpec,
//     DivergenceEvent) and 3 new elabels.
//
// This is the Wave-0 gate Nyquist requires: every downstream Phase 3 plan
// (03-02..03-15) reads from this substrate, so any drift in migration
// application surfaces as a fail HERE rather than in a downstream feature test.

package integration

import (
	"context"
	"testing"

	"github.com/neksur-com/neksur/internal/migrate"
)

const phase3MigrationsTenant = "55555555-5555-5555-5555-555555555555"

func TestPhase3MigrationsAppliedPerTenant(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	schema := fx.ProvisionTenant(t, phase3MigrationsTenant)

	conn, err := fx.pool.Acquire(fx.ctx)
	if err != nil {
		t.Fatalf("pool.Acquire: %v", err)
	}
	defer conn.Release()

	// --- (1) Confirm V0050 vlabel + elabel counts ----------------------
	// After Phase 0+1+2+3: 19+4+2+3 = 28 vlabels, 24+5+10+3 = 42 elabels.
	var vcount, ecount int
	if err := conn.QueryRow(fx.ctx, `
		SELECT count(*) FROM ag_catalog.ag_label
		WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
		  AND kind = 'v'
		  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`).Scan(&vcount); err != nil {
		t.Fatalf("count vlabels: %v", err)
	}
	if err := conn.QueryRow(fx.ctx, `
		SELECT count(*) FROM ag_catalog.ag_label
		WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
		  AND kind = 'e'
		  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`).Scan(&ecount); err != nil {
		t.Fatalf("count elabels: %v", err)
	}
	if vcount != 28 {
		t.Errorf("vlabel count = %d; want 28 (19 V0010 + 4 V0030 + 2 V0040 + 3 V0050)", vcount)
	}
	if ecount != 42 {
		t.Errorf("elabel count = %d; want 42 (24 V0010 + 5 V0030 + 10 V0040 + 3 V0050)", ecount)
	}

	// --- (2) Confirm Phase3MaxVersion + Phase3GraphMaxVersion constants
	// are consistent with the migration constants.
	if migrate.Phase3MaxVersion != "0081" {
		t.Errorf("migrate.Phase3MaxVersion = %q; want 0081", migrate.Phase3MaxVersion)
	}
	if migrate.Phase3GraphMaxVersion != "0052" {
		t.Errorf("migrate.Phase3GraphMaxVersion = %q; want 0052", migrate.Phase3GraphMaxVersion)
	}

	// --- (3) Confirm V0052 FORCE RLS on the 3 new vlabels + 3 new elabels
	phase3Labels := []string{
		"SnapshotPin", "PartitionSpec", "DivergenceEvent",
		"PINS", "USES_SPEC", "DIVERGED_AT",
	}
	for _, label := range phase3Labels {
		var forced bool
		if err := conn.QueryRow(fx.ctx, `
			SELECT (c.relrowsecurity AND c.relforcerowsecurity)
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'neksur' AND c.relname = $1
		`, label).Scan(&forced); err != nil {
			t.Errorf("FORCE RLS probe for %s: %v", label, err)
			continue
		}
		if !forced {
			t.Errorf("FORCE RLS not set on neksur.%q (V0052 didn't land?)", label)
		}
	}

	// --- (4) Confirm V0080 schema_changed trigger exists on per-tenant snapshots
	var triggerExists bool
	if err := conn.QueryRow(fx.ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_trigger t
		  JOIN pg_class c ON c.oid = t.tgrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = 'snapshots'
		    AND t.tgname = 'schema_changed_trigger'
		    AND NOT t.tgisinternal
		)
	`, schema).Scan(&triggerExists); err != nil {
		t.Fatalf("V0080 trigger probe: %v", err)
	}
	if !triggerExists {
		t.Errorf("V0080 schema_changed_trigger missing on %s.snapshots", schema)
	}

	// --- (5) Confirm V0081 write_conflict_policy column exists on policies
	var colCount int
	if err := conn.QueryRow(fx.ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name   = 'policies'
		  AND column_name  = 'write_conflict_policy'
		  AND data_type    = 'text'
		  AND is_nullable  = 'NO'
	`, schema).Scan(&colCount); err != nil {
		t.Fatalf("V0081 column probe: %v", err)
	}
	if colCount != 1 {
		t.Errorf("V0081 write_conflict_policy column missing in %s.policies (count=%d)", schema, colCount)
	}

	// --- (6) Confirm V0080 notify_schema_changed function exists
	var fnExists bool
	if err := conn.QueryRow(fx.ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_proc p
		  JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE p.proname = 'notify_schema_changed'
		    AND n.nspname = $1
		)
	`, schema).Scan(&fnExists); err != nil {
		t.Fatalf("V0080 function probe: %v", err)
	}
	if !fnExists {
		t.Errorf("V0080 notify_schema_changed() function missing in schema %s", schema)
	}

	// --- (7) Confirm write_conflict_policy CHECK constraint exists ----
	var chkExists bool
	if err := conn.QueryRow(fx.ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		  WHERE n.nspname = $1
		    AND t.relname = 'policies'
		    AND c.contype = 'c'
		    AND pg_get_constraintdef(c.oid) LIKE '%write_conflict_policy IN%'
		)
	`, schema).Scan(&chkExists); err != nil {
		t.Fatalf("V0081 CHECK constraint probe: %v", err)
	}
	if !chkExists {
		t.Errorf("V0081 write_conflict_policy CHECK constraint missing on %s.policies", schema)
	}

	// --- (8) Smoke-test the schema_changed LISTEN/NOTIFY substrate ---
	// Open a fresh connection, LISTEN, then INSERT a snapshot with
	// operation='schema_change' and assert a notification fires within ~2s.
	smokeSchemaChangedNotify(t, fx, schema, phase3MigrationsTenant)
}

// smokeSchemaChangedNotify confirms the V0080 trigger emits a notification
// on the schema_changed channel when a schema-change snapshot is inserted.
// Data-append snapshots (operation='append') MUST NOT fire a notification
// per RESEARCH Pitfall 1 (DDL-only filter).
func smokeSchemaChangedNotify(t *testing.T, fx *Phase3Fixture, schema, tenantUUID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(fx.ctx, 5_000_000_000) // 5s budget
	defer cancel()

	listenConn, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("smokeSchemaChangedNotify: listenConn acquire: %v", err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(ctx, "LISTEN schema_changed"); err != nil {
		t.Fatalf("smokeSchemaChangedNotify: LISTEN: %v", err)
	}

	insertConn, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("smokeSchemaChangedNotify: insertConn acquire: %v", err)
	}
	defer insertConn.Release()

	// Set search_path to the tenant schema so the per-tenant snapshots
	// table is targeted.
	if _, err := insertConn.Exec(ctx,
		`SELECT set_config('search_path', $1, false)`, schema); err != nil {
		t.Fatalf("smokeSchemaChangedNotify: set_config search_path: %v", err)
	}
	// Insert a schema_change snapshot — MUST fire the trigger.
	if _, err := insertConn.Exec(ctx, `
		INSERT INTO snapshots (tenant_id, table_id, snapshot_id, operation)
		VALUES ($1::uuid, gen_random_uuid(), gen_random_uuid()::text, 'schema_change')
	`, tenantUUID); err != nil {
		t.Fatalf("smokeSchemaChangedNotify: INSERT schema_change snapshot: %v", err)
	}

	notification, err := listenConn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("smokeSchemaChangedNotify: WaitForNotification: %v", err)
	}
	if notification.Channel != "schema_changed" {
		t.Errorf("notification channel = %q; want schema_changed", notification.Channel)
	}
	if len(notification.Payload) == 0 {
		t.Error("notification payload is empty (V0080 trigger emitted nothing?)")
	}

	// Now confirm an 'append' snapshot does NOT fire the trigger within 1s.
	// We reset the listener and INSERT an append; expect no notification.
	appendCtx, appendCancel := context.WithTimeout(ctx, 1_000_000_000) // 1s
	defer appendCancel()
	if _, err := insertConn.Exec(ctx, `
		INSERT INTO snapshots (tenant_id, table_id, snapshot_id, operation)
		VALUES ($1::uuid, gen_random_uuid(), gen_random_uuid()::text, 'append')
	`, tenantUUID); err != nil {
		t.Fatalf("smokeSchemaChangedNotify: INSERT append snapshot: %v", err)
	}
	_, err = listenConn.Conn().WaitForNotification(appendCtx)
	if err == nil {
		t.Error("smokeSchemaChangedNotify: unexpected notification for 'append' operation (DDL-only filter broken?)")
	}
	// WaitForNotification returns an error on context timeout — that's the
	// expected outcome for the append case.
}
