//go:build integration

// Package integration — Phase 0.5 Atlas tenant-loop integration test.
//
// TestAtlasLoopApplyRollbackApply verifies the [BLOCKING] success
// criterion of Plan 02:
//
//   1. Boot a fresh Postgres+AGE testcontainer with the Phase 0 schema.
//   2. Baseline-import Atlas at version 0030 (Phase 0 high-water mark),
//      then apply the Phase 0.5 public-tier migrations (V0041–V0044).
//   3. Provision a tenant schema (create_graph + role + GRANTs + tenant
//      Atlas apply via internal/migrate.RunForTenant).
//   4. Drop the tenant schema CASCADE (simulating a recreate scenario).
//   5. Re-provision the tenant. The second apply MUST succeed without
//      replaying the public-tier migrations (atlas.sum + public.atlas_schema_revisions
//      records prevent re-execution).
//   6. Assert public.atlas_schema_revisions contains rows for V0041,
//      V0042, V0043, V0044.
//
// Maps to:
//   * REQ-saas-onboarding (Atlas multi-tenant rollout is the substrate)
//   * REQ-saas-tenancy-pool-a (Pool A schema-per-tenant migration path)
//   * Plan 02 §success-criteria — "Atlas migration apply succeeds
//     end-to-end against a testcontainers Postgres+AGE"
//
// Build tag `integration` is required so `go test ./...` from the
// developer laptop doesn't accidentally try to boot Docker.
package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestAtlasLoopApplyRollbackApply is the BLOCKING gate from Plan 02 Task 4.
func TestAtlasLoopApplyRollbackApply(t *testing.T) {
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const tenantUUID = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	expectedSchema := "tenant_aaaaaaaa_aaaa_4aaa_aaaa_aaaaaaaaaaaa"

	// ----- Phase 1: initial provisioning -----------------------------
	schema := fx.ProvisionTenant(t, tenantUUID)
	if schema != expectedSchema {
		t.Fatalf("ProvisionTenant returned schema %q; expected %q", schema, expectedSchema)
	}

	// Public-tier revisions should be present after StartSaasFixture's
	// ApplyPublic + ProvisionTenant's tenant apply.
	assertRevisions(t, fx, []string{"0041", "0042", "0043", "0044"})

	// Tenant schema must exist in pg_namespace.
	if !schemaExists(t, fx, schema) {
		t.Fatalf("tenant schema %q missing after ProvisionTenant", schema)
	}

	// ----- Phase 2: drop the tenant schema CASCADE -------------------
	dropSchemaCascade(t, fx, schema)
	if schemaExists(t, fx, schema) {
		t.Fatalf("tenant schema %q still present after DROP SCHEMA CASCADE", schema)
	}

	// ----- Phase 3: re-provision (idempotency check) -----------------
	// The second ProvisionTenant must succeed; Atlas will see that
	// public.atlas_schema_revisions already records the public-tier
	// versions and skip them. The tenant-scoped revisions row(s) will
	// be re-recorded because the schema was dropped (revision rows
	// for tenant-only migrations live under search_path=<schema>,public
	// but the migration directory currently has no tenant-only files —
	// Plan 04 adds those — so the apply is a no-op revision-row-wise).
	schema2 := fx.ProvisionTenant(t, tenantUUID)
	if schema2 != expectedSchema {
		t.Fatalf("Re-ProvisionTenant returned schema %q; expected %q", schema2, expectedSchema)
	}

	// Public-tier revisions are still intact and non-duplicated.
	assertRevisions(t, fx, []string{"0041", "0042", "0043", "0044"})
}

// assertRevisions asserts that public.atlas_schema_revisions contains
// exactly one row for each of the expected versions. Atlas writes the
// version column as the migration's numeric prefix.
func assertRevisions(t *testing.T, fx *SaasFixture, versions []string) {
	t.Helper()
	conn, err := pgx.Connect(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("assertRevisions: pgx.Connect: %v", err)
	}
	defer conn.Close(fx.ctx)

	for _, v := range versions {
		var count int
		err := conn.QueryRow(fx.ctx,
			`SELECT COUNT(*) FROM public.atlas_schema_revisions WHERE version = $1`, v).Scan(&count)
		if err != nil {
			t.Fatalf("assertRevisions: query version %s: %v", v, err)
		}
		if count != 1 {
			t.Errorf("assertRevisions: expected 1 row for version %s, got %d", v, count)
		}
	}
}

// schemaExists returns true iff the named schema is present in
// pg_namespace. Used to verify create + drop transitions.
func schemaExists(t *testing.T, fx *SaasFixture, schema string) bool {
	t.Helper()
	conn, err := pgx.Connect(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("schemaExists: pgx.Connect: %v", err)
	}
	defer conn.Close(fx.ctx)

	var present bool
	err = conn.QueryRow(fx.ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&present)
	if err != nil {
		t.Fatalf("schemaExists: query: %v", err)
	}
	return present
}

// dropSchemaCascade tears down the tenant schema + any owned objects.
// For AGE-managed schemas, the canonical path is `drop_graph(<name>, true)`
// which cleans both the schema and the ag_graph catalog row in one go.
// A bare DROP SCHEMA CASCADE fails because AGE protects label tables
// (SQLSTATE 2BP01).
func dropSchemaCascade(t *testing.T, fx *SaasFixture, schema string) {
	t.Helper()
	conn, err := pgx.Connect(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("dropSchemaCascade: pgx.Connect: %v", err)
	}
	defer conn.Close(fx.ctx)

	// LOAD 'age' is required so drop_graph resolves.
	if _, err := conn.Exec(fx.ctx, "LOAD 'age'"); err != nil {
		t.Fatalf("dropSchemaCascade: LOAD 'age': %v", err)
	}
	if _, err := conn.Exec(fx.ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		t.Fatalf("dropSchemaCascade: SET search_path: %v", err)
	}

	// drop_graph(<name>, cascade => true) — AGE removes the schema +
	// the ag_graph entry transactionally. Returns NULL on success.
	if _, err := conn.Exec(fx.ctx,
		fmt.Sprintf(`SELECT drop_graph(%s, true)`, quoteLiteral(schema))); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			// Already gone; nothing to do.
			return
		}
		t.Fatalf("dropSchemaCascade: drop_graph(%s): %v", schema, err)
	}

	// The tenant role isn't dropped — ProvisionTenant's IF NOT EXISTS
	// guard handles the re-run case. Leaving the role around keeps the
	// test surface focused on schema lifecycle (which is the gate).
}
