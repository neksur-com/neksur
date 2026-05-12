// Package security holds the Phase 0 security tier — RLS isolation
// (cross-tenant blocking, FORCE-bypass blocking, session-var bleed
// detection), CHECK-constraint enforcement of mandatory tenant_id, and
// Cypher injection parameter-passthrough. Each test runs against a
// real apache/age:release_PG16_1.6.0 testcontainer with the Phase 0
// migrations applied, using the neksur_app non-superuser role so the
// V0030 RLS policies actually fire.
//
// Maps to 00-VALIDATION.md rows for T-0-RLS, T-0-INJ, T-0-SESS.
//
// Originally Python's tests/security/* under the Wave 1 plan, now Go
// per the 2026-05-13 D-PHASE0-stack correction.
package security

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/tests/testfixture"
)

// fix is the package-scoped testcontainer + pool, populated by TestMain.
var fix *fixture

type fixture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	container *testfixture.AGEContainer
	// superPool is connected as `postgres` superuser. The tenantTx /
	// tenantTxCommit helpers acquire from here and SET LOCAL ROLE
	// neksur_app inside the txn so V0030 RLS policies actually fire
	// (postgres bypasses RLS unconditionally). LOAD 'age' is
	// superuser-only so connecting directly as neksur_app fails at
	// the AfterConnect hook — matches the Python tier's pattern.
	superPool *pgxpool.Pool
}

func TestMain(m *testing.M) {
	if os.Getenv("SKIP_DOCKER") == "1" {
		fmt.Fprintln(os.Stderr, "SKIP_DOCKER=1 — skipping tests/security")
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	c, err := testfixture.Start(ctx)
	if err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "TestMain: testfixture.Start: %v\n", err)
		os.Exit(1)
	}
	superPool, err := testfixture.NewAGEPool(ctx, c.SuperuserDSN)
	if err != nil {
		_ = c.Terminate(ctx)
		cancel()
		fmt.Fprintf(os.Stderr, "TestMain: superPool: %v\n", err)
		os.Exit(1)
	}

	fix = &fixture{
		ctx:       ctx,
		cancel:    cancel,
		container: c,
		superPool: superPool,
	}

	code := m.Run()

	fix.superPool.Close()
	tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = c.Terminate(tctx)
	tcancel()
	cancel()

	os.Exit(code)
}

// tenantTx opens a fresh transaction on the superuser pool, role-
// downgrades to neksur_app, sets the tenant GUC via the production
// code path. Defer release() to ROLLBACK.
//
// The SET LOCAL ROLE pattern matches the Python tier (conftest.py
// tenant_conn_factory): superuser connects + LOAD 'age', then each
// test txn role-downgrades so RLS applies. LOAD 'age' is superuser-
// only so this is the only working pattern absent a custom-built
// image that pre-grants LOAD privileges.
func tenantTx(t *testing.T, tenantID string) (pgx.Tx, func()) {
	t.Helper()
	conn, err := fix.superPool.Acquire(fix.ctx)
	if err != nil {
		t.Fatalf("tenantTx acquire: %v", err)
	}
	tx, err := conn.Begin(fix.ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("tenantTx begin: %v", err)
	}
	if _, err := tx.Exec(fix.ctx, "SET LOCAL ROLE neksur_app"); err != nil {
		_ = tx.Rollback(fix.ctx)
		conn.Release()
		t.Fatalf("tenantTx SET ROLE: %v", err)
	}
	if _, err := tx.Exec(fix.ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		_ = tx.Rollback(fix.ctx)
		conn.Release()
		t.Fatalf("tenantTx set_config: %v", err)
	}
	return tx, func() {
		_ = tx.Rollback(fix.ctx)
		conn.Release()
	}
}

// tenantTxCommit is like tenantTx but the returned closer COMMITs.
func tenantTxCommit(t *testing.T, tenantID string) (pgx.Tx, func() error) {
	t.Helper()
	conn, err := fix.superPool.Acquire(fix.ctx)
	if err != nil {
		t.Fatalf("tenantTxCommit acquire: %v", err)
	}
	tx, err := conn.Begin(fix.ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("tenantTxCommit begin: %v", err)
	}
	if _, err := tx.Exec(fix.ctx, "SET LOCAL ROLE neksur_app"); err != nil {
		_ = tx.Rollback(fix.ctx)
		conn.Release()
		t.Fatalf("tenantTxCommit SET ROLE: %v", err)
	}
	if _, err := tx.Exec(fix.ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		_ = tx.Rollback(fix.ctx)
		conn.Release()
		t.Fatalf("tenantTxCommit set_config: %v", err)
	}
	return tx, func() error {
		defer conn.Release()
		return tx.Commit(fix.ctx)
	}
}
