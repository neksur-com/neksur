// Package integration contains the Phase 0 integration tier — schema,
// indexes, EXPLAIN, hybrid storage, and tenant RLS smoke tests against
// a real apache/age:release_PG16_1.6.0 testcontainer. The fixture is
// created once per package via TestMain so each test pays only setup
// cost for its own assertions.
//
// Maps to 00-VALIDATION.md row 02-T3 / REQ-knowledge-graph-foundation.
//
// Originally Python's tests/integration/* under the Wave 1 plan, now Go
// per the 2026-05-13 D-PHASE0-stack correction.
//
// Phase 0.5 (SaaS) tests use StartSaasFixture (saas_fixtures.go) which
// adds public-tier Atlas migrations on top of the Phase 0 schema.
//
// Phase 1 tests use StartPhase1Fixture (phase1_fixtures.go) which
// composes SaasFixture with Polaris + Nessie + LocalStack testcontainers
// and per-tenant graph migrations via internal/migrate.ApplyTenantGraph.
// Both extensions are opt-in per-test; this TestMain only initialises
// the Phase 0 superuser pool for the Phase 0 RLS tests in this package.
package integration

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

// fix is the package-scoped testcontainer + pool, populated by TestMain
// and used by every test in this package.
var fix *fixture

type fixture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	container *testfixture.AGEContainer
	// superPool is connected as `postgres` superuser. Used directly for
	// catalog introspection / non-RLS reads. For RLS-bound transactions
	// the tenantTx / tenantTxCommit helpers acquire from this pool and
	// SET LOCAL ROLE neksur_app inside the transaction so the V0030
	// policies actually fire — matching the Python tier's pattern where
	// a single superuser connection switches role per-tenant txn.
	// (LOAD 'age' is a superuser-only operation; connecting directly as
	// neksur_app would fail at AfterConnect.)
	superPool *pgxpool.Pool
}

// TestMain runs once for the integration package. It starts the AGE
// container, applies migrations, builds the two pools, runs the tests,
// then tears down on the way out.
func TestMain(m *testing.M) {
	if os.Getenv("SKIP_DOCKER") == "1" {
		// Allow developers without Docker to run `go test ./internal/...`
		// without this package erroring on missing Docker socket.
		fmt.Fprintln(os.Stderr, "SKIP_DOCKER=1 — skipping tests/integration")
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

// tenantTx opens a fresh transaction on the superuser pool, switches
// role to neksur_app (so V0030 RLS policies fire — superusers bypass
// RLS unconditionally), sets the tenant GUC via the production code
// path (set_config), and returns the tx plus a release that rolls
// back. Tests should defer release().
//
// The SET LOCAL ROLE pattern matches the Python tier (conftest.py
// tenant_conn_factory): one superuser connection that already loaded
// AGE (LOAD 'age' is superuser-only), each test txn role-downgrades
// to neksur_app so RLS applies. The role reverts at txn end.
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

// tenantTxCommit is like tenantTx but commits on success (returns a
// commit-or-rollback closer). Used for tests that need to seed data
// inside a tenant context for a later read by a different tenant.
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
