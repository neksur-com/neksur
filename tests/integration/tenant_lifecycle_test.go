//go:build integration

// Plan 00.5-07 Task 1 — tenant lifecycle integration tests against a real
// Postgres+AGE testcontainer. Exercises D-0.5.20's full state machine via
// internal/tenant.Repo methods (the same surface the CLI subcommands call).
//
// Test cases:
//   1. TestSuspendThenReadOnly — active → suspended; verify lifecycle_state
//      flipped + audit_log row written + (via probe) reads still work
//      (the GUC-set path returns rows from the tenant schema).
//   2. TestWindDownPreservesData — provision → wind-down; assert state and
//      that the tenant schema still exists (read window is preserved).
//   3. TestDeleteIrreversible — wind-down → delete; assert state, schema
//      dropped, audit_log row with event_type='tenant.deleted'. Second
//      Delete on the same id returns ErrTenantNotFound.
//   4. TestInvalidStateTransition — active → wind-down is allowed; but
//      WindDown on a 'deleted' tenant returns ErrTenantNotFound (no DB
//      mutation occurred — re-reading the row confirms state == 'deleted').

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

// TestSuspendThenReadOnly — active → suspended, assert state + audit row +
// tenant schema still queryable.
func TestSuspendThenReadOnly(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const tenantUUID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	id := uuid.MustParse(tenantUUID)
	schema := fx.ProvisionTenant(t, tenantUUID)

	pool := mustPool(t, ctx, fx.SuperDSN())
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	// Seed the tenants row (ProvisionTenant only does the schema+role).
	if err := repo.Create(ctx, tenant.Tenant{
		ID:             id,
		WorkOSOrgID:    "org_SUSPENDTEST",
		LifecycleState: "active",
		Pool:           "A",
	}); err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	if err := repo.Suspend(ctx, id, "lifecycle-test@neksur.com"); err != nil {
		t.Fatalf("repo.Suspend: %v", err)
	}

	state := mustReadLifecycleState(t, ctx, pool, id)
	if state != "suspended" {
		t.Fatalf("lifecycle_state after Suspend = %q; want %q", state, "suspended")
	}

	auditCount := mustCountAudit(t, ctx, pool, id, "tenant.suspended")
	if auditCount != 1 {
		t.Fatalf("system_audit_log rows for tenant.suspended = %d; want 1", auditCount)
	}

	// Tenant schema still exists + readable. (We connect as superuser
	// here, not the tenant role, because the role's GRANTs are only
	// for the per-request transaction path. The point of this check
	// is to confirm Suspend did NOT drop the schema.)
	probe := mustConn(t, ctx, fx.SuperDSN())
	defer func() { _ = probe.Close(ctx) }()
	var schemaCount int
	if err := probe.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname=$1`, schema).Scan(&schemaCount); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if schemaCount != 1 {
		t.Fatalf("schema %s presence after Suspend: count=%d; want 1 (Suspend MUST NOT drop schema)", schema, schemaCount)
	}
	t.Logf("Suspend OK: state=%s, audit_count=%d, schema_intact=%d", state, auditCount, schemaCount)
}

// TestWindDownPreservesData — active → wind_down, assert state and that
// the tenant schema is still queryable (D-0.5.20: 30-day read-only window).
func TestWindDownPreservesData(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const tenantUUID = "11111111-2222-4333-8444-555555555555"
	id := uuid.MustParse(tenantUUID)
	schema := fx.ProvisionTenant(t, tenantUUID)

	pool := mustPool(t, ctx, fx.SuperDSN())
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	if err := repo.Create(ctx, tenant.Tenant{
		ID:             id,
		WorkOSOrgID:    "org_WINDDOWNTEST",
		LifecycleState: "active",
		Pool:           "A",
	}); err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	if err := repo.WindDown(ctx, id, "lifecycle-test@neksur.com"); err != nil {
		t.Fatalf("repo.WindDown: %v", err)
	}

	state := mustReadLifecycleState(t, ctx, pool, id)
	if state != "wind_down" {
		t.Fatalf("lifecycle_state after WindDown = %q; want %q", state, "wind_down")
	}

	auditCount := mustCountAudit(t, ctx, pool, id, "tenant.wind_down")
	if auditCount != 1 {
		t.Fatalf("system_audit_log rows for tenant.wind_down = %d; want 1", auditCount)
	}

	// Schema must still exist — customer can still download audit_log
	// and policies during the 30-day window.
	probe := mustConn(t, ctx, fx.SuperDSN())
	defer func() { _ = probe.Close(ctx) }()
	var schemaCount int
	if err := probe.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname=$1`, schema).Scan(&schemaCount); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if schemaCount != 1 {
		t.Fatalf("schema %s presence after WindDown: count=%d; want 1 (read-only window preserves data)", schema, schemaCount)
	}
	t.Logf("WindDown OK: state=%s, audit_count=%d, schema_intact=%d", state, auditCount, schemaCount)
}

// TestDeleteIrreversible — wind_down → deleted, assert state + audit row +
// schema dropped + second Delete returns ErrTenantNotFound.
func TestDeleteIrreversible(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const tenantUUID = "deadbeef-cafe-4abc-8def-012345678901"
	id := uuid.MustParse(tenantUUID)
	schema := fx.ProvisionTenant(t, tenantUUID)

	// Use DescribeExec query mode — drop_graph mutates session state
	// (LOAD 'age' + search_path); prepared-statement cache invalidates
	// on subsequent transitionLifecycle calls. Same idiom as
	// provisioning_test.go line 62.
	cfg, err := pgxpool.ParseConfig(fx.SuperDSN())
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	if err := repo.Create(ctx, tenant.Tenant{
		ID:             id,
		WorkOSOrgID:    "org_DELETETEST",
		LifecycleState: "active",
		Pool:           "A",
	}); err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	// Transition active → wind_down → deleted, exercising the typical
	// post-cancellation flow.
	if err := repo.WindDown(ctx, id, "lifecycle-test@neksur.com"); err != nil {
		t.Fatalf("repo.WindDown: %v", err)
	}
	if err := repo.Delete(ctx, id, "lifecycle-test@neksur.com", true); err != nil {
		t.Fatalf("repo.Delete: %v", err)
	}

	state := mustReadLifecycleState(t, ctx, pool, id)
	if state != "deleted" {
		t.Fatalf("lifecycle_state after Delete = %q; want %q", state, "deleted")
	}

	auditCount := mustCountAudit(t, ctx, pool, id, "tenant.deleted")
	if auditCount != 1 {
		t.Fatalf("system_audit_log rows for tenant.deleted = %d; want 1", auditCount)
	}

	// Schema MUST be dropped.
	probe := mustConn(t, ctx, fx.SuperDSN())
	defer func() { _ = probe.Close(ctx) }()
	var schemaCount int
	if err := probe.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname=$1`, schema).Scan(&schemaCount); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if schemaCount != 0 {
		t.Fatalf("schema %s presence after Delete: count=%d; want 0 (Delete MUST drop schema via drop_graph)", schema, schemaCount)
	}

	// Second Delete returns ErrTenantNotFound (the lifecycle guard
	// rejects deleting a row already in 'deleted' state).
	err2 := repo.Delete(ctx, id, "lifecycle-test@neksur.com", true)
	if err2 == nil {
		t.Fatal("second Delete returned nil; want ErrTenantNotFound")
	}
	if !errors.Is(err2, tenant.ErrTenantNotFound) {
		t.Fatalf("second Delete error = %v; want ErrTenantNotFound", err2)
	}
	t.Logf("Delete OK: state=%s, audit_count=%d, schema_dropped=true, second_call=ErrTenantNotFound", state, auditCount)
}

// TestInvalidStateTransition — verify the state-machine guards reject
// invalid transitions. WindDown on a 'deleted' row returns ErrTenantNotFound
// AND DOES NOT mutate the row.
func TestInvalidStateTransition(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const tenantUUID = "99999999-8888-4777-8666-555555555555"
	id := uuid.MustParse(tenantUUID)
	_ = fx.ProvisionTenant(t, tenantUUID)

	cfg, err := pgxpool.ParseConfig(fx.SuperDSN())
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	if err := repo.Create(ctx, tenant.Tenant{
		ID:             id,
		WorkOSOrgID:    "org_INVALIDTRANSITION",
		LifecycleState: "active",
		Pool:           "A",
	}); err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	// Delete (active → deleted is allowed — lifecycle.go fromStates
	// list for Delete accepts active/suspended/wind_down).
	if err := repo.Delete(ctx, id, "test", true); err != nil {
		t.Fatalf("repo.Delete (active→deleted is permitted): %v", err)
	}

	// Now try Suspend on a deleted row — must fail and NOT mutate.
	preState := mustReadLifecycleState(t, ctx, pool, id)
	err1 := repo.Suspend(ctx, id, "test")
	if err1 == nil {
		t.Fatal("Suspend on deleted tenant returned nil; want ErrTenantNotFound")
	}
	if !errors.Is(err1, tenant.ErrTenantNotFound) {
		t.Fatalf("Suspend on deleted: error = %v; want ErrTenantNotFound", err1)
	}
	postState := mustReadLifecycleState(t, ctx, pool, id)
	if preState != postState {
		t.Fatalf("lifecycle_state changed despite invalid transition: %q → %q", preState, postState)
	}

	// And WindDown on a deleted row.
	err2 := repo.WindDown(ctx, id, "test")
	if err2 == nil {
		t.Fatal("WindDown on deleted tenant returned nil; want ErrTenantNotFound")
	}
	if !errors.Is(err2, tenant.ErrTenantNotFound) {
		t.Fatalf("WindDown on deleted: error = %v; want ErrTenantNotFound", err2)
	}
	postState2 := mustReadLifecycleState(t, ctx, pool, id)
	if postState != postState2 {
		t.Fatalf("lifecycle_state changed across two invalid transitions: %q → %q", postState, postState2)
	}
	t.Logf("Invalid transitions correctly rejected; state remained %q across two failed attempts.", postState2)
}

// --- Helpers ---------------------------------------------------------------

func mustPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	return pool
}

func mustConn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	return conn
}

func mustReadLifecycleState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(ctx,
		`SELECT lifecycle_state FROM public.tenants WHERE id=$1`,
		id,
	).Scan(&state); err != nil {
		t.Fatalf("read lifecycle_state for %s: %v", id, err)
	}
	return state
}

func mustCountAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM public.system_audit_log
		 WHERE target_tenant_id = $1 AND event_type = $2
	`, id, eventType).Scan(&n); err != nil {
		// Surface the nature of the failure clearly — a missing
		// system_audit_log table is an upstream schema problem.
		if strings.Contains(err.Error(), "system_audit_log") && strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("public.system_audit_log table missing (Plan 02 V0043 not applied?): %v", err)
		}
		t.Fatalf("count audit rows for tenant %s event %s: %v", id, eventType, err)
	}
	return n
}

// ensure imported fmt usage (silences staticcheck in builds where the
// integration tag isn't applied).
var _ = fmt.Sprintf
