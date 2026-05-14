//go:build integration

// Package integration — SaaS (Phase 0.5) test fixtures.
//
// Extends the Phase 0 `testfixture` package with helpers that exercise
// the multi-tenant migration path:
//   * StartSaasFixture(t) — boots an apache/age testcontainer (re-uses
//     testfixture.Start) and runs the Atlas public-tier migrations on
//     top of the Phase 0 schema.
//   * ProvisionTenant(t, uuid) — provisions a single tenant end-to-end:
//     create_graph(tenant_<uuid_underscored>) → guarded role create →
//     GRANTs → ApplyTenant via the internal/migrate package.
//   * Terminate() — tears down the container.
//
// SaasFixture is referenced by tests/integration/atlas_loop_test.go
// (TestAtlasLoopApplyRollbackApply) and will also be consumed by Plan 03
// (WorkOS middleware integration test) + Plan 04 (provisioning end-to-end).
//
// The fixture is intentionally not concurrency-safe — each test that
// needs a SaaS-shape DB should call StartSaasFixture itself; the
// per-package TestMain in main_test.go keeps a Phase 0 fixture alive
// for the Phase 0 RLS tests.
package integration

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/migrate"
	"github.com/neksur-com/neksur/tests/testfixture"
)

// SaasFixture is the Phase 0.5 integration test target. It owns the
// testcontainer + a long-lived superuser connection used to run the
// per-tenant provisioning DDL.
type SaasFixture struct {
	Container *testfixture.AGEContainer

	// Reserved for future plans; Plan 03 wires WorkOSMock in
	// internal/auth/workos/middleware_test.go and Plan 04 wires
	// LocalStack for the S3 + Secrets Manager side of provisioning.
	WorkOSMock *httptest.Server
	LocalStack *localStackPlaceholder

	ctx    context.Context
	cancel context.CancelFunc
}

// localStackPlaceholder reserves the name for Plan 04 without forcing a
// dependency on a LocalStack container at this layer.
type localStackPlaceholder struct{}

// StartSaasFixture boots a fresh Postgres+AGE testcontainer, applies
// the Phase 0 schema (V0001 + V0010 + V0020 + V0025 + V0030 via the
// testfixture.Start path), then applies the Phase 0.5 public-tier
// migrations (V0041–V0044) via the Atlas migration runner.
//
// On any failure the container is terminated and t.Fatal is invoked.
func StartSaasFixture(t *testing.T) *SaasFixture {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping Phase 0.5 SaaS fixture")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	c, err := testfixture.Start(ctx)
	if err != nil {
		cancel()
		t.Fatalf("StartSaasFixture: testfixture.Start: %v", err)
	}

	// Apply the Phase 0.5 public-tier migrations (V0041 + V0042 +
	// V0043 + V0044). The Phase 0 testfixture already applied V0001
	// + V0030 directly via psql, so Atlas first needs a baseline at
	// version 0030 to skip them. We invoke atlas with --baseline 0030
	// once.
	if err := atlasBaseline(ctx, c.SuperuserDSN, "0030"); err != nil {
		_ = c.Terminate(ctx)
		cancel()
		t.Fatalf("StartSaasFixture: atlas baseline 0030: %v", err)
	}

	if err := migrate.ApplyPublic(ctx, c.SuperuserDSN); err != nil {
		_ = c.Terminate(ctx)
		cancel()
		t.Fatalf("StartSaasFixture: migrate.ApplyPublic: %v", err)
	}

	return &SaasFixture{
		Container: c,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// ProvisionTenant creates the per-tenant schema, role, and GRANTs, then
// applies the migration loop's tenant pass. Returns the resulting
// Postgres schema name (`tenant_<uuid_with_underscores>`).
//
// The function is idempotent for the schema + role steps (IF NOT EXISTS
// guards); Atlas itself records the applied revisions in
// public.atlas_schema_revisions so re-running is a no-op.
//
// tenantUUID is expected in canonical 36-char hex form
// (e.g., "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"). The function maps
// hyphens to underscores per D-0.5.04.
func (f *SaasFixture) ProvisionTenant(t *testing.T, tenantUUID string) string {
	t.Helper()
	if tenantUUID == "" {
		t.Fatalf("ProvisionTenant: tenantUUID must be non-empty")
	}

	schema := schemaNameFromUUID(tenantUUID)

	// Superuser connection — needed for create_graph (which requires
	// LOAD 'age' which requires superuser).
	conn, err := pgx.Connect(f.ctx, f.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("ProvisionTenant: pgx.Connect: %v", err)
	}
	defer conn.Close(f.ctx)

	// LOAD 'age' so create_graph resolves.
	if _, err := conn.Exec(f.ctx, "LOAD 'age'"); err != nil {
		t.Fatalf("ProvisionTenant: LOAD 'age': %v", err)
	}
	if _, err := conn.Exec(f.ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		t.Fatalf("ProvisionTenant: SET search_path: %v", err)
	}

	// Step (a) — create_graph. AGE create_graph fails if the graph
	// already exists; the test cycle drops + re-creates, so we do
	// not pre-check existence here. Callers that want idempotency at
	// this layer should pre-check pg_namespace.
	if _, err := conn.Exec(f.ctx, fmt.Sprintf("SELECT create_graph(%s)", quoteLiteral(schema))); err != nil {
		// If the schema already exists, AGE returns "graph already exists"
		// — treat as idempotent.
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("ProvisionTenant: create_graph(%s): %v", schema, err)
		}
	}

	// Step (b) — per-tenant role. PATTERNS.md idempotent SQL line 800.
	roleName := schema + "_role"
	createRoleSQL := fmt.Sprintf(`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
				CREATE ROLE %s WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
			END IF;
		END
		$$ LANGUAGE plpgsql`,
		quoteLiteral(roleName), quoteIdent(roleName))
	if _, err := conn.Exec(f.ctx, createRoleSQL); err != nil {
		t.Fatalf("ProvisionTenant: create role %s: %v", roleName, err)
	}

	// Step (c) — GRANTs per RESEARCH §Pattern 1 lines 464–500.
	// Tenant role gets USAGE on the tenant schema + CRUD on tables;
	// USAGE on ag_catalog so it can run cypher() + agtype operators.
	// `GRANT <tenant_role> TO neksur_app` enables `SET ROLE` from the
	// app role (Plan 04 Layer 2 isolation test path).
	grants := []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, quoteIdent(schema), quoteIdent(roleName)),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA ag_catalog TO %s`, quoteIdent(roleName)),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s`, quoteIdent(schema), quoteIdent(roleName)),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s`, quoteIdent(schema), quoteIdent(roleName)),
		fmt.Sprintf(`GRANT %s TO neksur_app`, quoteIdent(roleName)),
	}
	for _, s := range grants {
		if _, err := conn.Exec(f.ctx, s); err != nil {
			t.Fatalf("ProvisionTenant: %s: %v", s, err)
		}
	}

	// Step (d) — Atlas tenant-loop. Apply migrations into the tenant
	// schema with revisions written to public.atlas_schema_revisions.
	if err := migrate.RunForTenant(f.ctx, f.Container.SuperuserDSN, schema); err != nil {
		t.Fatalf("ProvisionTenant: migrate.RunForTenant(%s): %v", schema, err)
	}

	// Step (e) — Plan 04 T-0.5-audit-tamper post-migration REVOKE.
	// Atlas creates audit_log via V0050 with the role-set default
	// privileges (SELECT, INSERT, UPDATE). For Plan 04 compliance we
	// REVOKE UPDATE, DELETE on audit_log so the tenant role can
	// INSERT-only. Idempotent.
	auditTbl := quoteIdent(schema) + ".audit_log"
	revokes := []string{
		fmt.Sprintf(`REVOKE UPDATE, DELETE ON %s FROM %s`, auditTbl, quoteIdent(roleName)),
		fmt.Sprintf(`REVOKE TRUNCATE ON %s FROM %s`, auditTbl, quoteIdent(roleName)),
	}
	for _, s := range revokes {
		if _, err := conn.Exec(f.ctx, s); err != nil {
			t.Fatalf("ProvisionTenant: %s: %v", s, err)
		}
	}

	return schema
}

// Terminate stops the container and releases context. Safe to call
// multiple times; subsequent calls are no-ops.
func (f *SaasFixture) Terminate() {
	if f == nil {
		return
	}
	if f.Container != nil {
		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = f.Container.Terminate(tctx)
		tcancel()
		f.Container = nil
	}
	if f.cancel != nil {
		f.cancel()
	}
}

// SuperDSN returns the superuser DSN — exposed for tests that need to
// inspect public.atlas_schema_revisions directly.
func (f *SaasFixture) SuperDSN() string { return f.Container.SuperuserDSN }

// AppDSN returns the neksur_app role DSN — exposed for tests that
// exercise Layer 2/3 RLS.
func (f *SaasFixture) AppDSN() string { return f.Container.AppDSN }

// atlasBaseline runs `atlas migrate apply --baseline <version>` to
// teach Atlas that <version> is the highest historically-applied
// revision (Phase 0 was deployed via raw psql / sqitch, predating
// Atlas adoption). After baseline, Atlas will start applying from the
// next-higher version (here, V0041).
//
// Equivalent to: atlas migrate apply --baseline 0030 --url <dsn> --dir ...
func atlasBaseline(ctx context.Context, dsn, version string) error {
	bin := migrate.AtlasBinary
	args := []string{
		"--config", migrate.ResolveConfigURL(),
		"--env", "public",
		"migrate", "apply",
		"--url", dsn,
		"--dir", migrate.ResolveDirURL(),
		"--revisions-schema", migrate.RevisionsSchema,
		"--baseline", version,
	}
	return runExternal(ctx, bin, args...)
}

// runExternal is a tiny exec helper for the baseline command — we don't
// route through internal/migrate.ApplyPublic because that path uses the
// standard apply flags; --baseline is per-apply (one shot).
func runExternal(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// schemaNameFromUUID maps the canonical 36-char UUID form to a Postgres
// schema name per D-0.5.04. "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa" →
// "tenant_aaaaaaaa_aaaa_4aaa_aaaa_aaaaaaaaaaaa".
func schemaNameFromUUID(uuidString string) string {
	return "tenant_" + strings.ReplaceAll(uuidString, "-", "_")
}

// quoteIdent wraps an identifier in double quotes and escapes embedded
// double quotes. Sufficient for the schema/role names this fixture
// generates (which are always tenant_<hex>_role or tenant_<hex>).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteLiteral wraps a string in single quotes and escapes embedded
// single quotes. Used for the role-existence check + create_graph arg.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
