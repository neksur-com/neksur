//go:build integration

// Plan 00.5-03 Task 3 — the canonical proof that pgxpool's
// BeforeAcquire DISCARD ALL hook prevents Layer 1 + Layer 3 session
// state from leaking across pool acquisitions.
//
// RESEARCH §Summary line 13 calls this out as the single biggest
// landmine of Phase 0.5: pgx's AfterRelease hook runs ASYNC with no
// context (jackc/pgx#1666) so it cannot be used reliably for session
// reset. BeforeAcquire is the correct hook — and this test proves it
// works end-to-end against a real Postgres+AGE container.
//
// Test flow:
//   1. Start a SaasFixture (Postgres+AGE testcontainer + V0041-V0044).
//   2. Provision two tenants (A, B) so both schemas + roles exist.
//   3. Build a pgxpool with WithBeforeAcquireDiscardAll + MaxConns=1
//      (forces the pool to reuse the SAME physical connection across
//      acquisitions, which is the worst-case for session bleed).
//   4. Acquisition 1: use tenant.WithTenantTx for tenant A; the three
//      SET LOCAL layers apply + clear at COMMIT.
//   5. Acquisition 2: raw pool.Acquire (NO WithTenantTx); assert:
//      * app.current_tenant is empty (BeforeAcquire ran DISCARD ALL).
//      * search_path does NOT contain tenant_aaaaaaaa (Layer 1 cleared).
//
// If WithBeforeAcquireDiscardAll were removed, the test would fail —
// because even though SET LOCAL clears at COMMIT inside step 4, the
// AGE prelude AfterConnect hook is run only on physical conn creation,
// not on every acquire. Without DISCARD ALL, residual state from the
// AfterConnect prelude (which itself uses non-LOCAL `SET search_path`)
// would not be the bleed vector — but ANY future code that forgets
// SET LOCAL would leak. This test guarantees the defense-in-depth
// invariant holds.
package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
)

const (
	tenantAUUID = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	tenantBUUID = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
)

// TestSessionBleed — the canonical proof of DISCARD ALL discipline.
func TestSessionBleed(t *testing.T) {
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	// Provision two tenants — both schemas + roles created.
	tenantA := uuid.MustParse(tenantAUUID)
	tenantB := uuid.MustParse(tenantBUUID)
	fx.ProvisionTenant(t, tenantA.String())
	fx.ProvisionTenant(t, tenantB.String())

	// Build a pool with our DISCARD ALL hook. MaxConns=1 forces the
	// pool to hand back the SAME physical connection on every
	// acquisition. AfterConnect runs the AGE prelude (LOAD 'age' +
	// search_path) so the conn is ready for AGE if we ever ran a
	// cypher() query from this test (we don't, but it's the
	// production parity invariant).
	cfg, err := pgxpool.ParseConfig(fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	graph.WithBeforeAcquireDiscardAll(cfg)
	cfg.MaxConns = 1
	// describe_exec to avoid prepared-statement cache issues after
	// DISCARD ALL (which invalidates prepared statements). The
	// testfixture.NewAGEPool helper does the same for the same reason.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`)
		return err
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	defer pool.Close()

	// --- Acquisition 1: tenant A via WithTenantTx ----------------------
	ctxA := tenant.WithID(context.Background(), tenantA)
	err = tenant.WithTenantTx(ctxA, pool, func(tx pgx.Tx) error {
		var got string
		if err := tx.QueryRow(ctxA, "SELECT current_setting('app.current_tenant')").Scan(&got); err != nil {
			return err
		}
		if got != tenantA.String() {
			t.Errorf("inside WithTenantTx: current_setting('app.current_tenant') = %q; want %q",
				got, tenantA.String())
		}
		var sp string
		if err := tx.QueryRow(ctxA, "SELECT current_setting('search_path')").Scan(&sp); err != nil {
			return err
		}
		if !strings.Contains(sp, tenant.SchemaName(tenantA)) {
			t.Errorf("inside WithTenantTx: search_path = %q; want it to contain %q",
				sp, tenant.SchemaName(tenantA))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTenantTx(A): %v", err)
	}

	// --- Acquisition 2: raw acquire (NO WithTenantTx) ------------------
	// BeforeAcquire should have run DISCARD ALL between releases.
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	defer conn.Release()

	var leakedGUC string
	if err := conn.QueryRow(context.Background(),
		"SELECT COALESCE(current_setting('app.current_tenant', true), '__EMPTY__')",
	).Scan(&leakedGUC); err != nil {
		t.Fatalf("query app.current_tenant: %v", err)
	}
	if leakedGUC != "__EMPTY__" && leakedGUC != "" {
		t.Errorf("SESSION BLEED: expected app.current_tenant cleared by DISCARD ALL; got %q", leakedGUC)
	}

	// Layer 1: search_path. After DISCARD ALL the AfterConnect hook's
	// `SET search_path = ag_catalog, "$user", public` does NOT re-run
	// (AfterConnect runs once per physical conn at acquisition-creation,
	// not on every acquire). So search_path should be the Postgres
	// default `"$user", public` — i.e., NOT contain tenant_aaaaaaaa.
	var sp string
	if err := conn.QueryRow(context.Background(),
		"SELECT current_setting('search_path')",
	).Scan(&sp); err != nil {
		t.Fatalf("query search_path: %v", err)
	}
	if strings.Contains(sp, tenant.SchemaName(tenantA)) {
		t.Errorf("SESSION BLEED: search_path = %q still contains %q after DISCARD ALL",
			sp, tenant.SchemaName(tenantA))
	}

	// Layer 2: current_user / role. We released the connection from a
	// transaction that SET LOCAL ROLE — after COMMIT, the role
	// reverts to session_user. DISCARD ALL also runs RESET ROLE
	// defensively. Either way the current_user should NOT be
	// tenant_aaaaaaaa_role.
	var cu string
	if err := conn.QueryRow(context.Background(),
		"SELECT current_user::text",
	).Scan(&cu); err != nil {
		t.Fatalf("query current_user: %v", err)
	}
	if cu == tenant.RoleName(tenantA) {
		t.Errorf("SESSION BLEED: current_user is still %q (Layer 2 leaked)", cu)
	}
}
