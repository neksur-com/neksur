//go:build integration

// Plan 02-01 Wave-0 BLOCKING — Phase 2 migrations applied per tenant.
//
// TestPhase2MigrationsAppliedPerTenant asserts that:
//   * V0040 + V0041 + V0042 graph migrations land (25 vlabels + 39
//     elabels after the per-tenant Apply* loop runs).
//   * V0070 + V0071 + V0072 + V0073 relational migrations land
//     (public.engines table + public.tenants.tenant_default_attributes
//     column + per-tenant policy_compile_log + per-tenant trigger).
//   * RLS is forced on the 2 new vlabels (CompiledPolicy + Attribute).
//
// This is the Wave-0 gate Nyquist requires: every downstream plan
// (02-03, 02-04, 02-05, 02-07) reads from this substrate, so any drift
// in migration application surfaces as a fail HERE rather than in a
// downstream feature test.

package integration

import (
	"context"
	"testing"
)

const phase2MigrationsTenant = "44444444-4444-4444-4444-444444444444"

func TestPhase2MigrationsAppliedPerTenant(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	schema := fx.ProvisionTenant(t, phase2MigrationsTenant)

	conn, err := fx.pool.Acquire(fx.ctx)
	if err != nil {
		t.Fatalf("pool.Acquire: %v", err)
	}
	defer conn.Release()

	// --- (1) Confirm V0040 vlabel + elabel counts ----------------------
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
	if vcount != 25 {
		t.Errorf("vlabel count = %d; want 25 (19 V0010 + 4 V0030 + 2 V0040)", vcount)
	}
	if ecount != 39 {
		t.Errorf("elabel count = %d; want 39 (24 V0010 + 5 V0030 + 10 V0040)", ecount)
	}

	// --- (2) Confirm V0042 FORCE RLS on the 2 new vlabels --------------
	for _, label := range []string{"CompiledPolicy", "Attribute"} {
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
			t.Errorf("FORCE RLS not set on neksur.%q (V0042 didn't land?)", label)
		}
	}

	// --- (3) Confirm V0070 public.engines exists + canary insert works -
	if err := canaryEngineRowProbe(fx.ctx, fx, phase2MigrationsTenant); err != nil {
		t.Errorf("V0070 public.engines canary failed: %v", err)
	}

	// --- (4) Confirm V0071 tenant_default_attributes column exists -----
	var colCount int
	if err := conn.QueryRow(fx.ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name='tenants'
		  AND column_name='tenant_default_attributes'
		  AND data_type='jsonb' AND is_nullable='NO'
	`).Scan(&colCount); err != nil {
		t.Fatalf("V0071 column probe: %v", err)
	}
	if colCount != 1 {
		t.Errorf("V0071 tenant_default_attributes column missing (count=%d)", colCount)
	}

	// --- (5) Confirm V0072 per-tenant policy_compile_log exists --------
	var compileLogExists bool
	if err := conn.QueryRow(fx.ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_tables
		  WHERE schemaname = $1 AND tablename = 'policy_compile_log'
		)
	`, schema).Scan(&compileLogExists); err != nil {
		t.Fatalf("V0072 policy_compile_log probe: %v", err)
	}
	if !compileLogExists {
		t.Errorf("V0072 policy_compile_log missing in schema %s", schema)
	}

	// --- (6) Confirm V0073 trigger exists on policies -----------------
	var triggerExists bool
	if err := conn.QueryRow(fx.ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_trigger t
		  JOIN pg_class c ON c.oid = t.tgrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = 'policies'
		    AND t.tgname = 'policy_changed_trigger'
		    AND NOT t.tgisinternal
		)
	`, schema).Scan(&triggerExists); err != nil {
		t.Fatalf("V0073 trigger probe: %v", err)
	}
	if !triggerExists {
		t.Errorf("V0073 policy_changed_trigger missing on %s.policies", schema)
	}

	// --- (7) Smoke-test the LISTEN/NOTIFY substrate -------------------
	// Open a fresh connection, LISTEN, then INSERT a policy via a second
	// connection and assert a notification fires within ~1s.
	// (Smoke test only — full delivery semantics tested in Plan 02-04.)
	smokeListenNotify(t, fx, schema, phase2MigrationsTenant)
}

// smokeListenNotify confirms the V0073 trigger emits a notification on
// the policy_changed channel after a policies INSERT. Uses two
// connections from the admin pool: one LISTENs, the other INSERTs.
func smokeListenNotify(t *testing.T, fx *Phase2Fixture, schema, tenantUUID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(fx.ctx, 5_000_000_000) // 5s budget
	defer cancel()

	listenConn, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("smokeListenNotify: listenConn acquire: %v", err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(ctx, "LISTEN policy_changed"); err != nil {
		t.Fatalf("smokeListenNotify: LISTEN: %v", err)
	}

	insertConn, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("smokeListenNotify: insertConn acquire: %v", err)
	}
	defer insertConn.Release()
	// Set the tenant GUC so the V0073 trigger's strict
	// current_setting('app.current_tenant') call doesn't error.
	if _, err := insertConn.Exec(ctx,
		`SELECT set_config('app.current_tenant', $1, false)`, tenantUUID); err != nil {
		t.Fatalf("smokeListenNotify: set_config: %v", err)
	}
	// Set search_path to the tenant schema so the per-tenant policies
	// table is targeted (V0052 created it in the tenant schema).
	if _, err := insertConn.Exec(ctx,
		`SELECT set_config('search_path', $1, false)`, schema); err != nil {
		t.Fatalf("smokeListenNotify: set_config search_path: %v", err)
	}
	if _, err := insertConn.Exec(ctx, `
		INSERT INTO policies (name, kind, body)
		VALUES ('smoke-policy-' || gen_random_uuid()::text, 'row_filter', '{}'::jsonb)
	`); err != nil {
		t.Fatalf("smokeListenNotify: INSERT policy: %v", err)
	}

	notification, err := listenConn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("smokeListenNotify: WaitForNotification: %v", err)
	}
	if notification.Channel != "policy_changed" {
		t.Errorf("notification channel = %q; want policy_changed", notification.Channel)
	}
	// Payload should be JSON containing tenant_id + policy_id.
	if len(notification.Payload) == 0 {
		t.Error("notification payload is empty (V0073 trigger emitted nothing?)")
	}
}
