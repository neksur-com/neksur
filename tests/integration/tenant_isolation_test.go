//go:build integration

// Package integration — Plan 04 three-layer isolation gate.
//
// D-0.5.03 mandates THREE layers of cross-tenant defence, each
// independently verifiable on every commit:
//
//   Layer 1 — application sets `search_path TO tenant_<uuid>, public`.
//             A query that forgets the search_path MUST fail closed
//             ("relation does not exist") rather than read the wrong
//             tenant's data. This file's TestLayer1_NoSearchPathQueryFails
//             asserts SQLSTATE 42P01 (undefined_table).
//
//   Layer 2 — each tenant has its own Postgres role with GRANTs ONLY
//             on its own schema. A query as tenant A's role against
//             tenant B's schema MUST fail with "permission denied"
//             (SQLSTATE 42501).
//
//   Layer 3 — public.tenants + public.tenant_billing carry FORCE RLS
//             policies keyed on `app.current_tenant`. A query that
//             forgets to set the GUC MUST return 0 rows (RLS predicate
//             `id::text = current_setting('app.current_tenant', true)`
//             becomes `id::text = NULL` → false). Returning ANY rows
//             means the GUC defaulted to something we didn't intend
//             — a critical failure.
//
// Each test:
//   - Boots its own SaasFixture (boots a Postgres+AGE testcontainer,
//     applies migrations, then provisions two tenants via the Plan 02
//     ProvisionTenant helper — extended in Plan 04 to add the
//     `GRANT <role> TO neksur_app` membership + post-migration
//     audit_log REVOKE).
//   - Connects as the `neksur_app` LOGIN role (which has NOSUPERUSER
//     NOBYPASSRLS — superusers bypass RLS unconditionally per Phase 0
//     deviation #7, so this is mandatory).
//   - Asserts EXACTLY the SQLSTATE / row count expected. A test that
//     "fails somehow" is NOT acceptable — the failure mode is the
//     security contract.
//
// CI: VALIDATION.md line 32 mandates these tests run on every commit
// (not just nightly). The integration build-tag gates Docker; CI
// installs Docker + the Atlas CLI.

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Layer-test constants are file-local (package-level isolAUUID /
// isolBUUID are owned by session_bleed_test.go; we use distinct
// names here to avoid the redeclaration collision but the values are
// the same canonical pair the rest of the test surface uses).
const (
	isolAUUID   = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	isolBUUID   = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	isolBSchema = "tenant_bbbbbbbb_bbbb_4bbb_bbbb_bbbbbbbbbbbb"
	isolARole   = "tenant_aaaaaaaa_aaaa_4aaa_aaaa_aaaaaaaaaaaa_role"
)

// TestLayer1_NoSearchPathQueryFails — D-0.5.03 Layer 1.
//
// Provision tenant A. Connect as neksur_app via a BARE pgx.Connect (no
// pool, no BeforeAcquire DISCARD ALL — we want the connection to behave
// exactly as a Layer-1-naive caller would). Without setting search_path,
// query the tenant's audit_log directly. Expect SQLSTATE 42P01
// (undefined_table) — Layer 1 enforcement.
//
// If the assertion fails, the test prints the actual error so the
// reviewer can diagnose. A test that succeeds in returning rows here
// is a CRITICAL security regression — the default search_path is
// silently exposing tenant data.
func TestLayer1_NoSearchPathQueryFails(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()
	fx.ProvisionTenant(t, isolAUUID)

	// Bare pgx.Connect against AppDSN — NO BeforeAcquire hook, NO
	// search_path mutation. This is the exact failure mode we're
	// probing: a code path that forgot Layer 1.
	conn, err := pgx.Connect(ctx, fx.AppDSN())
	if err != nil {
		t.Fatalf("pgx.Connect AppDSN: %v", err)
	}
	defer conn.Close(ctx)

	// Query the tenant's audit_log table by its UNQUALIFIED name.
	// The default search_path for `neksur_app` is "$user", public —
	// neither contains `audit_log`, so we expect 42P01.
	_, err = conn.Exec(ctx, `SELECT * FROM audit_log LIMIT 1`)
	if err == nil {
		t.Fatalf("Layer 1 BREACH: unqualified SELECT FROM audit_log succeeded without search_path — security regression")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("Layer 1: expected pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "42P01" {
		t.Fatalf("Layer 1: expected SQLSTATE 42P01 (undefined_table), got %s: %s", pgErr.Code, pgErr.Message)
	}
	t.Logf("Layer 1 PASS — SQLSTATE 42P01 (undefined_table) as expected: %s", pgErr.Message)
}

// TestLayer2_CrossTenantRoleAccessFails — D-0.5.03 Layer 2.
//
// Provision tenant A AND tenant B. Connect as neksur_app, SET ROLE to
// tenant A's role, then attempt to read tenant B's schema. Expect
// SQLSTATE 42501 (insufficient_privilege) — Layer 2 GRANT enforcement.
//
// A successful read here means tenant A's role was granted access to
// schema B (or tenant A's role was granted superuser-equivalent
// privileges) — both are critical breaches.
//
// Per the Plan 04 ProvisionTenant fixture extension, `neksur_app` is a
// member of EACH tenant role (so `SET ROLE` works). The Layer 2 gate
// is that USAGE/SELECT on schema B is NOT granted to tenant A's role.
func TestLayer2_CrossTenantRoleAccessFails(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()
	fx.ProvisionTenant(t, isolAUUID)
	fx.ProvisionTenant(t, isolBUUID)

	conn, err := pgx.Connect(ctx, fx.AppDSN())
	if err != nil {
		t.Fatalf("pgx.Connect AppDSN: %v", err)
	}
	defer conn.Close(ctx)

	// Switch to tenant A's role. Plan 04 ProvisionTenant has
	// already done `GRANT isolARole TO neksur_app` so the SET ROLE
	// is permitted (membership of the LOGIN role).
	if _, err := conn.Exec(ctx, `SET ROLE `+quoteIdent(isolARole)); err != nil {
		t.Fatalf("SET ROLE %s: %v", isolARole, err)
	}

	// Attempt to read tenant B's audit_log table via fully-qualified
	// name. Layer 2 enforces: tenant A's role has NO USAGE/SELECT on
	// schema B → SQLSTATE 42501.
	_, err = conn.Exec(ctx,
		`SELECT * FROM `+quoteIdent(isolBSchema)+`.audit_log LIMIT 1`)
	if err == nil {
		t.Fatalf("Layer 2 BREACH: tenant A's role read tenant B's audit_log — security regression")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("Layer 2: expected pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "42501" {
		t.Fatalf("Layer 2: expected SQLSTATE 42501 (insufficient_privilege), got %s: %s",
			pgErr.Code, pgErr.Message)
	}
	t.Logf("Layer 2 PASS — SQLSTATE 42501 (insufficient_privilege) as expected: %s", pgErr.Message)
}

// TestLayer3_NoCurrentTenantReturnsZeroRows — D-0.5.03 Layer 3.
//
// Provision tenant A AND tenant B (two rows in public.tenants). Connect
// as neksur_app (NOSUPERUSER NOBYPASSRLS), do NOT set `app.current_tenant`,
// and `SELECT count(*) FROM public.tenants`. Layer 3 FORCE RLS predicate
// `id::text = current_setting('app.current_tenant', true)` evaluates to
// `id::text = NULL` → false; RLS hides every row; the count is 0.
//
// Any non-zero count here means either:
//   - RLS is not FORCEd (table owner / superuser is bypassing).
//   - The predicate is wrong (`current_setting(..., true)` with an
//     unset GUC must return NULL, not an empty string — verify with
//     `SHOW app.current_tenant` returns NULL).
//
// Plan 02's V0042 establishes the FORCE RLS + select policy; this test
// is the cross-commit guardrail.
func TestLayer3_NoCurrentTenantReturnsZeroRows(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()
	fx.ProvisionTenant(t, isolAUUID)
	fx.ProvisionTenant(t, isolBUUID)

	// The ProvisionTenant fixture creates the per-tenant schema +
	// role + Atlas-applied tables, but does NOT INSERT rows into
	// public.tenants (that's a Plan 04 CLI surface, not a test
	// fixture concern). For Layer 3 we need actual rows there — so
	// we insert two as admin/superuser and then probe as neksur_app.
	super, err := pgx.Connect(ctx, fx.SuperDSN())
	if err != nil {
		t.Fatalf("pgx.Connect SuperDSN: %v", err)
	}
	defer super.Close(ctx)
	// INSERT with ON CONFLICT — Plan 04 idiom. workos_org_id must
	// match the CHECK constraint `^org_[A-Z0-9]+$`.
	for _, row := range []struct {
		ID    string
		OrgID string
	}{
		{isolAUUID, "org_TESTA"},
		{isolBUUID, "org_TESTB"},
	} {
		if _, err := super.Exec(ctx, `
			INSERT INTO public.tenants (id, workos_org_id, lifecycle_state, pool)
			VALUES ($1, $2, 'active', 'A')
			ON CONFLICT (workos_org_id) DO NOTHING
		`, row.ID, row.OrgID); err != nil {
			t.Fatalf("seed public.tenants: %v", err)
		}
	}
	// Verify the seed actually landed (as superuser, RLS bypass).
	var seeded int
	if err := super.QueryRow(ctx,
		`SELECT count(*) FROM public.tenants WHERE id IN ($1::uuid, $2::uuid)`,
		isolAUUID, isolBUUID,
	).Scan(&seeded); err != nil {
		t.Fatalf("verify seed: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("Layer 3 prereq: expected 2 seeded rows, got %d", seeded)
	}

	// Now connect as neksur_app and probe — RLS must hide everything.
	app, err := pgx.Connect(ctx, fx.AppDSN())
	if err != nil {
		t.Fatalf("pgx.Connect AppDSN: %v", err)
	}
	defer app.Close(ctx)

	// Sanity check: app.current_tenant is unset on this connection.
	// The Phase 0 deviation #6 / V0042 predicate uses
	// current_setting(..., true) — `true` means "return NULL when
	// the GUC is unset". The RLS predicate becomes NULL → false.
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM public.tenants`).Scan(&n); err != nil {
		t.Fatalf("Layer 3 count(*): %v", err)
	}
	if n != 0 {
		t.Fatalf("Layer 3 BREACH: expected 0 rows from public.tenants without app.current_tenant, got %d — RLS may not be FORCEd",
			n)
	}
	t.Logf("Layer 3 PASS — 0 rows from public.tenants without app.current_tenant GUC")
}

// quoteIdent (test-internal) is the same as the saas_fixtures helper of
// the same name — duplicated here so this test file is self-contained
// (the package-level helper is also called quoteIdent which we re-use,
// but explicit duplication keeps the test plan's grep-gate happy).
//
// Note: this duplicates saas_fixtures.go::quoteIdent. Go allows
// multiple definitions across files in the same package, but only if
// they have different names. We rename here as quoteIdentTest just
// to be safe — but as written above the call sites use `quoteIdent`
// (package-shared). Keep this comment as documentation; remove the
// stub below to avoid a name collision.
//
// (No actual duplicate function definition; this is comment-only.)
