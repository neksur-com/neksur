//go:build integration

// Plan 01-06 Task 1 [BLOCKING] — per-tenant catalog_credentials lookup
// + RLS isolation.
//
// Two integration tests:
//   - TestGetCatalogCredentialsReturnsRow — happy path: insert a row
//     into T1's schema, look it up via T1's ctx, assert kind+endpoint
//     match.
//   - TestGetCatalogCredentialsCrossTenantBlocked — RLS isolation:
//     insert into T1, query with T2's ctx, assert ErrCredentialsNotFound
//     (V0066 RLS predicate hides cross-tenant rows).
//
// These tests live in the catalog package directory but pull the
// integration fixture (Phase1Fixture) — that's why the build tag is
// `integration` and we import the integration test helpers. The
// fixture's ProvisionTenant already seeds a `prod-polaris` row pointing
// at the Polaris testfixture; the second test additionally inserts a
// `cross-tenant-test` row to assert isolation.

package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/catalog"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
	"github.com/neksur-com/neksur/tests/testfixture"
)

// minimalFixture is a one-test-per-process Phase 0 testfixture wrapper:
// boots a Postgres+AGE testcontainer, applies V0001+V0030, then runs
// the public-tier + per-tenant Atlas migrations against two tenant
// schemas. We avoid pulling tests/integration's StartPhase1Fixture (it
// boots Polaris+Nessie+LocalStack which the tests in THIS package don't
// need); the lighter shape here keeps the test cohort fast.
//
// Per-test isolation is fine because the V0060 catalog_credentials
// table is per-tenant — two tests against two tenants don't collide.
type minimalFixture struct {
	t   *testing.T
	ctx context.Context

	c    *testfixture.AGEContainer
	pool *pgxpool.Pool
}

// startMinimal returns a fixture or skips on SKIP_DOCKER=1. Caller
// MUST defer fx.terminate().
func startMinimal(t *testing.T) *minimalFixture {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping catalog creds integration test")
	}
	ctx := context.Background()
	c, err := testfixture.Start(ctx)
	if err != nil {
		t.Fatalf("testfixture.Start: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(c.SuperuserDSN)
	if err != nil {
		_ = c.Terminate(ctx)
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	graph.WithBeforeAcquireDiscardAll(cfg)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		_ = c.Terminate(ctx)
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	return &minimalFixture{t: t, ctx: ctx, c: c, pool: pool}
}

func (fx *minimalFixture) terminate() {
	if fx.pool != nil {
		fx.pool.Close()
	}
	if fx.c != nil {
		_ = fx.c.Terminate(context.Background())
	}
}

// provisionTenant runs the minimum DDL needed for catalog_credentials
// to exist + the V0066 RLS predicate to fire. We reuse Phase 0.5's
// per-tenant Atlas runner via the provisioning code path. To avoid
// pulling internal/tenant/provision.go (which would create a cyclic
// import via tenant package back to catalog), we use a hand-rolled
// minimal subset: create_graph + schema + role + apply V0050+V0060+V0065+V0066.
//
// For Plan 01-06 Task 1 the simpler path is to pull the same migration
// runner the integration package uses.
func (fx *minimalFixture) provisionTenant(tenantUUID string) string {
	fx.t.Helper()
	schema := tenant.SchemaName(uuid.MustParse(tenantUUID))
	roleName := tenant.RoleName(uuid.MustParse(tenantUUID))

	// CREATE SCHEMA + CREATE ROLE (idempotent).
	if _, err := fx.pool.Exec(fx.ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pgx.Identifier{schema}.Sanitize())); err != nil {
		fx.t.Fatalf("create schema: %v", err)
	}
	// Role create — guarded with DO block since CREATE ROLE doesn't have IF NOT EXISTS.
	doStmt := fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') THEN
			EXECUTE 'CREATE ROLE %s';
		END IF;
	END $$`, roleName, pgx.Identifier{roleName}.Sanitize())
	if _, err := fx.pool.Exec(fx.ctx, doStmt); err != nil {
		fx.t.Fatalf("create role: %v", err)
	}
	// Grant USAGE on schema; GRANT all on tables (we only need SELECT for
	// catalog_credentials but the broader grants keep parity with prod
	// provisioning).
	grants := []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", pgx.Identifier{schema}.Sanitize(), pgx.Identifier{roleName}.Sanitize()),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", pgx.Identifier{schema}.Sanitize(), pgx.Identifier{roleName}.Sanitize()),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT USAGE, SELECT ON SEQUENCES TO %s", pgx.Identifier{schema}.Sanitize(), pgx.Identifier{roleName}.Sanitize()),
	}
	for _, g := range grants {
		if _, err := fx.pool.Exec(fx.ctx, g); err != nil {
			fx.t.Fatalf("grant %q: %v", g, err)
		}
	}

	// Create the V0060 catalog_credentials table + V0066 RLS predicate
	// inline. This mirrors the Atlas migrations but inline keeps this
	// test dependency-free (no Atlas binary required for the catalog
	// package's BLOCKING test). The shape MUST match prod V0060 + V0066.
	createStmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.catalog_credentials (
			id               uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
			catalog_kind     text          NOT NULL CHECK (catalog_kind IN ('polaris','nessie','glue','unity')),
			nickname         text          NOT NULL UNIQUE,
			endpoint         text          NOT NULL,
			config_json      jsonb         NOT NULL,
			encrypted_secret bytea,
			created_at       timestamptz   NOT NULL DEFAULT now()
		)`, pgx.Identifier{schema}.Sanitize()),
		fmt.Sprintf("ALTER TABLE %s.catalog_credentials ENABLE ROW LEVEL SECURITY", pgx.Identifier{schema}.Sanitize()),
		fmt.Sprintf("ALTER TABLE %s.catalog_credentials FORCE ROW LEVEL SECURITY", pgx.Identifier{schema}.Sanitize()),
		// RLS predicate: must have app.current_tenant set; matching tenant
		// (this schema's UUID) is the only allowed value. The predicate
		// matches V0066 prod shape — we splice the tenant_uuid via fmt
		// because the schema name CONTAINS the UUID (so it's a known constant
		// at policy creation time).
		fmt.Sprintf(`CREATE POLICY catalog_credentials_tenant_isolation ON %s.catalog_credentials
			USING (current_setting('app.current_tenant', true) = '%s')
			WITH CHECK (current_setting('app.current_tenant', true) = '%s')`,
			pgx.Identifier{schema}.Sanitize(), tenantUUID, tenantUUID),
		fmt.Sprintf("GRANT SELECT, INSERT ON %s.catalog_credentials TO %s",
			pgx.Identifier{schema}.Sanitize(), pgx.Identifier{roleName}.Sanitize()),
	}
	for _, s := range createStmts {
		if _, err := fx.pool.Exec(fx.ctx, s); err != nil {
			fx.t.Fatalf("create table/policy %q: %v", s, err)
		}
	}

	return schema
}

// insertRowAdmin uses the superuser pool to bypass tenant.WithTenantTx
// — admin-side seed for the test (mimics Plan 01-09's onboarding flow
// running under admin role). We still set app.current_tenant so the RLS
// WITH CHECK predicate passes for the INSERT.
func (fx *minimalFixture) insertRowAdmin(t *testing.T, schema, tenantUUID, kind, nickname, endpoint, configJSON string) {
	t.Helper()
	conn, err := fx.pool.Acquire(fx.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(fx.ctx,
		"SELECT set_config('app.current_tenant', $1, true)", tenantUUID); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	q := fmt.Sprintf(`
		INSERT INTO %s.catalog_credentials (catalog_kind, nickname, endpoint, config_json)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (nickname) DO NOTHING
	`, pgx.Identifier{schema}.Sanitize())
	if _, err := conn.Exec(fx.ctx, q, kind, nickname, endpoint, configJSON); err != nil {
		t.Fatalf("insert row: %v", err)
	}
}

// TestGetCatalogCredentialsReturnsRow — happy-path lookup.
func TestGetCatalogCredentialsReturnsRow(t *testing.T) {
	fx := startMinimal(t)
	defer fx.terminate()

	const tenantStr = "11111111-1111-4111-8111-aaaaaaaaaaaa"
	tenantUUID := uuid.MustParse(tenantStr)
	schema := fx.provisionTenant(tenantStr)

	fx.insertRowAdmin(t, schema, tenantStr, "polaris", "prod-polaris",
		"https://polaris.acme.example.com/api/catalog",
		`{"warehouse":"test","clientId":"u","clientSecret":"s"}`)

	repo := catalog.NewRepo(fx.pool)
	ctx := tenant.WithID(fx.ctx, tenantUUID)
	creds, err := repo.GetCatalogCredentials(ctx, "prod-polaris")
	if err != nil {
		t.Fatalf("GetCatalogCredentials: %v", err)
	}
	if creds.Kind != "polaris" {
		t.Errorf("Kind = %q; want polaris", creds.Kind)
	}
	if creds.Endpoint != "https://polaris.acme.example.com/api/catalog" {
		t.Errorf("Endpoint = %q; mismatch", creds.Endpoint)
	}
	if creds.Nickname != "prod-polaris" {
		t.Errorf("Nickname = %q; want prod-polaris", creds.Nickname)
	}
	if len(creds.ConfigJSON) == 0 {
		t.Errorf("ConfigJSON empty")
	}
}

// TestGetCatalogCredentialsCrossTenantBlocked — RLS isolation.
func TestGetCatalogCredentialsCrossTenantBlocked(t *testing.T) {
	fx := startMinimal(t)
	defer fx.terminate()

	const t1Str = "22222222-2222-4222-8222-bbbbbbbbbbbb"
	const t2Str = "33333333-3333-4333-8333-cccccccccccc"
	t1 := uuid.MustParse(t1Str)
	t2 := uuid.MustParse(t2Str)

	schemaT1 := fx.provisionTenant(t1Str)
	_ = fx.provisionTenant(t2Str)

	// Insert ONLY into T1.
	fx.insertRowAdmin(t, schemaT1, t1Str, "polaris", "shared-nickname",
		"https://t1.example.com/api/catalog", `{"warehouse":"t1"}`)

	repo := catalog.NewRepo(fx.pool)

	// T1 sees the row.
	ctxT1 := tenant.WithID(fx.ctx, t1)
	credsT1, err := repo.GetCatalogCredentials(ctxT1, "shared-nickname")
	if err != nil {
		t.Fatalf("T1 lookup: %v", err)
	}
	if credsT1.Endpoint != "https://t1.example.com/api/catalog" {
		t.Errorf("T1 endpoint mismatch: %q", credsT1.Endpoint)
	}

	// T2 does NOT see the row — RLS isolates.
	ctxT2 := tenant.WithID(fx.ctx, t2)
	_, err = repo.GetCatalogCredentials(ctxT2, "shared-nickname")
	if !errors.Is(err, catalog.ErrCredentialsNotFound) {
		t.Errorf("T2 lookup should ErrCredentialsNotFound; got %v", err)
	}
}
