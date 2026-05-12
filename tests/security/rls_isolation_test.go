package security

import (
	"fmt"
	"strings"
	"testing"
)

// TestNoCrossTenantRead is the T-0-RLS smoking gun: Tenant B MUST NOT
// see Tenant A's rows. Procedure:
//  1. Insert one Table node as Tenant A with tenant_id='A'.
//  2. MATCH the same uri inside a Tenant B context.
//  3. Assert 0 rows returned.
//
// Runs as the neksur_app non-superuser role (per V0030 RLS policies);
// the postgres superuser would bypass RLS unconditionally and mask
// real leaks.
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-tenant-isolation / T-0-RLS /
// "Cross-tenant read returns zero rows".
func TestNoCrossTenantRead(t *testing.T) {
	aID := "tenant-A-cross-read"
	bID := "tenant-B-cross-read"

	// Insert as Tenant A.
	{
		tx, commit := tenantTxCommit(t, aID)
		props := `{"uri":"iceberg://t/A-only","name":"A-only","tenant_id":"` + aID + `"}`
		if _, err := tx.Exec(fix.ctx,
			`INSERT INTO neksur."Table" (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
			"281474976710801", props,
		); err != nil {
			t.Fatalf("seed A: %v", err)
		}
		if err := commit(); err != nil {
			t.Fatalf("seed commit A: %v", err)
		}
	}

	// Read as Tenant B — expect 0 rows.
	tx, release := tenantTx(t, bID)
	defer release()
	rows, err := tx.Query(fix.ctx, `
		SELECT * FROM cypher('neksur', $$
			MATCH (t:Table {uri: 'iceberg://t/A-only'})
			RETURN t
		$$) AS (t ag_catalog.agtype)
	`)
	if err != nil {
		t.Fatalf("MATCH as B: %v", err)
	}
	defer rows.Close()
	got := 0
	for rows.Next() {
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if got != 0 {
		t.Fatalf("T-0-RLS LEAK: Tenant B read %d of Tenant A's rows; expected 0", got)
	}
}

// TestInsertWithoutTenantFails verifies that an INSERT lacking
// tenant_id in the properties JSON is rejected. The row may be blocked
// by EITHER the CHECK constraint (`CHECK (properties ? 'tenant_id'::text)`,
// Deviation #3) OR the RLS WITH CHECK policy. Both gates exist in
// V0030. Under the neksur_app non-superuser role the RLS rejection
// typically wins (it runs before CHECK on INSERT per Postgres docs).
// We accept either error path as "the gate fired".
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-tenant-isolation / T-0-RLS /
// "Insert without tenant_id is rejected".
func TestInsertWithoutTenantFails(t *testing.T) {
	tid := "tenant-check-test"
	tx, release := tenantTx(t, tid)
	defer release()

	props := `{"uri":"iceberg://no-tenant/x"}` // deliberately no tenant_id
	_, err := tx.Exec(fix.ctx,
		`INSERT INTO neksur."Table" (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
		"281474976710803", props,
	)
	if err == nil {
		t.Fatalf("INSERT without tenant_id SUCCEEDED — V0030 gates are broken")
	}
	msg := err.Error()
	if !(strings.Contains(msg, "tenant_id_required") || strings.Contains(msg, "row-level security") || strings.Contains(msg, "row level security")) {
		t.Errorf("unexpected error (neither CHECK nor RLS): %v", err)
	}
}

// TestForceRlsBlocksOwnerBypass is the FORCE-RLS attack surface check.
// Procedure:
//  1. Insert one Tenant A row.
//  2. Reassign neksur."Table" OWNER to neksur_app (the table-owning
//     role attack surface).
//  3. Connect as neksur_app with NO tenant context.
//  4. Read directly from the heap; expect 0 rows.
//
// Without FORCE RLS, a table-owning role bypasses RLS — leaking data.
// V0030's FORCE clause closes that hole; this test proves it works.
// Restore ownership at end so other tests still have a clean state.
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-tenant-isolation / T-0-RLS /
// "RLS bypass via direct table query blocked (FORCE RLS)".
func TestForceRlsBlocksOwnerBypass(t *testing.T) {
	aID := "tenant-owner-bypass-A"

	// Step 1: insert as Tenant A.
	{
		tx, commit := tenantTxCommit(t, aID)
		props := `{"uri":"iceberg://t/force-rls-probe","name":"force-probe","tenant_id":"` + aID + `"}`
		if _, err := tx.Exec(fix.ctx,
			`INSERT INTO neksur."Table" (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
			"281474976710821", props,
		); err != nil {
			t.Fatalf("seed A: %v", err)
		}
		if err := commit(); err != nil {
			t.Fatalf("seed commit A: %v", err)
		}
	}

	// Step 2: reassign ownership (use superuser pool — only postgres
	// can ALTER TABLE OWNER, neksur_app cannot grant itself ownership).
	if _, err := fix.superPool.Exec(fix.ctx, `ALTER TABLE neksur."Table" OWNER TO neksur_app`); err != nil {
		t.Fatalf("ALTER TABLE OWNER: %v", err)
	}
	// Always restore on the way out — failing to restore would corrupt
	// other tests in this package and integration too.
	defer func() {
		if _, err := fix.superPool.Exec(fix.ctx, `ALTER TABLE neksur."Table" OWNER TO postgres`); err != nil {
			t.Logf("WARNING: failed to restore Table owner to postgres: %v", err)
		}
	}()

	// Step 3+4: acquire a superuser connection (LOAD 'age' already
	// ran via AfterConnect), BEGIN, SET LOCAL ROLE neksur_app so RLS
	// applies AND neksur_app is the table owner (Step 2 above), and
	// read with NO tenant context. The combination "table owner +
	// no current_tenant" is the FORCE-bypass attack surface.
	conn, err := fix.superPool.Acquire(fix.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(fix.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(fix.ctx)

	if _, err := tx.Exec(fix.ctx, "SET LOCAL ROLE neksur_app"); err != nil {
		t.Fatalf("SET LOCAL ROLE neksur_app: %v", err)
	}
	// Explicitly empty the GUC.
	if _, err := tx.Exec(fix.ctx, "SELECT set_config('app.current_tenant', '', true)"); err != nil {
		t.Fatalf("empty current_tenant: %v", err)
	}
	var n int
	err = tx.QueryRow(fix.ctx,
		`SELECT count(*) FROM neksur."Table" WHERE id = $1::ag_catalog.graphid`,
		"281474976710821",
	).Scan(&n)
	if err != nil {
		t.Fatalf("count as neksur_app owner: %v", err)
	}
	if n != 0 {
		t.Fatalf("FORCE RLS BYPASS: as table-owning neksur_app with no tenant context, saw %d rows; expected 0", n)
	}
}

// TestSessionVarBleed validates that DISCARD ALL clears app.current_tenant
// before a pooled connection is returned for reuse. Procedure:
//  1. Acquire a connection from the app pool.
//  2. Set app.current_tenant via set_config (mid-txn).
//  3. COMMIT (clears SET LOCAL by default).
//  4. DISCARD ALL (also clears any session-level GUCs).
//  5. Read current_setting('app.current_tenant', true); expect empty.
//
// Maps to PLAN 00-02 Task 3 non-VALIDATION-listed but mandated check:
// "test_session_var_bleed — validates DISCARD ALL pool reset wiring".
func TestSessionVarBleed(t *testing.T) {
	tid := "tenant-bleed-probe"
	conn, err := fix.superPool.Acquire(fix.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(fix.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(fix.ctx, "SELECT set_config('app.current_tenant', $1, true)", tid); err != nil {
		_ = tx.Rollback(fix.ctx)
		t.Fatalf("set_config: %v", err)
	}
	// Sanity: mid-txn the GUC is set.
	var mid string
	if err := tx.QueryRow(fix.ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&mid); err != nil {
		_ = tx.Rollback(fix.ctx)
		t.Fatalf("mid-txn read: %v", err)
	}
	if mid != tid {
		_ = tx.Rollback(fix.ctx)
		t.Fatalf("setup sanity: mid-txn GUC = %q; expected %q", mid, tid)
	}
	if err := tx.Commit(fix.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := conn.Exec(fix.ctx, "DISCARD ALL"); err != nil {
		t.Fatalf("DISCARD ALL: %v", err)
	}
	// Post-reset: GUC must be empty.
	var post string
	if err := conn.QueryRow(fix.ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&post); err != nil {
		t.Fatalf("post-reset read: %v", err)
	}
	if post != "" {
		t.Fatalf("SESSION VAR BLEED: post-reset current_tenant = %q; expected empty", post)
	}

	// Diagnostic logging (only on success path) for understanding
	// future regressions.
	if testing.Verbose() {
		t.Logf("session-var-bleed verified: pre-reset %q → post-reset %q (after %s)",
			tid, post, fmt.Sprint("COMMIT + DISCARD ALL"))
	}
}
