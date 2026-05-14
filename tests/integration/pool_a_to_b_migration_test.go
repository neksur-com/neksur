//go:build integration

// Package integration — Pool A → Pool B migration orchestration test
// (Plan 06). Uses TWO testcontainers Postgres+AGE instances to simulate
// the source and target clusters; exercises pg_dump → pg_restore →
// row-count validation end-to-end.
//
// What this test proves:
//   - The Go orchestration in internal/tenant.MigratePoolAToB drives
//     pg_dump + pg_restore correctly against real Postgres clusters.
//   - The per-table row-count validation discovers every tenant-schema
//     table and reports parity (or returns *RowCountMismatchError on
//     drift).
//   - public.tenants.pool flips from 'A' to 'B' after row-count success.
//
// What this test does NOT prove:
//   - The AWS-side Terraform module shape (covered by `terraform
//     validate` in CI + the nightly dry-run script).
//   - The 30-day Pool-A retention before final schema drop (operator-
//     driven runbook step; not in the Go orchestration).

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
	"github.com/neksur-com/neksur/tests/testfixture"
)

// TestPoolAToBMigration provisions a tenant in source-fixture (Pool A
// simulant), seeds data into its schema, then runs
// tenant.MigratePoolAToB with target-fixture (Pool B simulant). Asserts
// row counts match + public.tenants on the source side has pool='B'.
//
// Skipped without Docker (SKIP_DOCKER=1) AND if pg_dump/pg_restore CLI
// is not available on PATH (the migration shell-outs require both).
func TestPoolAToBMigration(t *testing.T) {
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — TestPoolAToBMigration requires Docker for the two AGE testcontainers")
	}

	// pg_dump / pg_restore must exist on PATH — the migration code
	// shells out to both.
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skipf("pg_dump not on PATH: %v — install postgresql-client to exercise this test", err)
	}
	if _, err := exec.LookPath("pg_restore"); err != nil {
		t.Skipf("pg_restore not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Bring up two containers — one for Pool A, one for Pool B.
	source, err := testfixture.Start(ctx)
	if err != nil {
		t.Fatalf("source testfixture.Start: %v", err)
	}
	defer func() { _ = source.Terminate(ctx) }()

	target, err := testfixture.Start(ctx)
	if err != nil {
		t.Fatalf("target testfixture.Start: %v", err)
	}
	defer func() { _ = target.Terminate(ctx) }()

	// Test tenant.
	tenantID, err := uuid.Parse("aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("uuid.Parse: %v", err)
	}
	schemaName := tenant.SchemaName(tenantID)

	// --- seed schema + minimal data on the SOURCE side -------------------
	//
	// We do NOT route through the StartSaasFixture path here because
	// that requires Atlas + the full V0041-V0044 + V0050-V0052 migration
	// chain — heavy for a focused migration test. Instead we synthesise
	// a minimal per-tenant schema directly: one table with a couple of
	// rows, sufficient to prove the pg_dump → pg_restore → row-count
	// orchestration shape.
	srcPool, err := pgxpool.New(ctx, source.SuperuserDSN)
	if err != nil {
		t.Fatalf("source pgxpool.New: %v", err)
	}
	defer srcPool.Close()

	qSchema := (pgx.Identifier{schemaName}).Sanitize()
	if _, err := srcPool.Exec(ctx, "CREATE SCHEMA "+qSchema); err != nil {
		t.Fatalf("source CREATE SCHEMA: %v", err)
	}
	auditTbl := qSchema + ".audit_log"
	queryTbl := qSchema + ".query_history"
	if _, err := srcPool.Exec(ctx, `CREATE TABLE `+auditTbl+` (
		id bigserial PRIMARY KEY,
		occurred_at timestamptz NOT NULL DEFAULT now(),
		actor text NOT NULL,
		event_type text NOT NULL,
		payload jsonb
	)`); err != nil {
		t.Fatalf("create audit_log: %v", err)
	}
	if _, err := srcPool.Exec(ctx, `CREATE TABLE `+queryTbl+` (
		id bigserial PRIMARY KEY,
		started_at timestamptz NOT NULL,
		duration_ms int NOT NULL,
		statement text NOT NULL
	)`); err != nil {
		t.Fatalf("create query_history: %v", err)
	}

	// Seed 150 audit rows + 50 query rows so row-count parity is a
	// non-trivial check.
	if _, err := srcPool.Exec(ctx, `
		INSERT INTO `+auditTbl+` (actor, event_type, payload)
		SELECT 'test@neksur.com', 'test.event', jsonb_build_object('n', g)
		FROM generate_series(1, 150) g
	`); err != nil {
		t.Fatalf("seed audit_log: %v", err)
	}
	if _, err := srcPool.Exec(ctx, `
		INSERT INTO `+queryTbl+` (started_at, duration_ms, statement)
		SELECT now() - (g * interval '1 minute'), g % 1000, 'SELECT 1; -- seed ' || g
		FROM generate_series(1, 50) g
	`); err != nil {
		t.Fatalf("seed query_history: %v", err)
	}

	// Apply Plan 02's public-tier minimum (just public.tenants + system_audit_log)
	// directly via DDL so the migration tool's UpdatePoolAndDSN +
	// writeMigrationAudit calls succeed.
	if _, err := srcPool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.tenants (
		id uuid PRIMARY KEY,
		workos_org_id text UNIQUE NOT NULL,
		lifecycle_state text NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active','suspended','wind_down','deleted')),
		pool text NOT NULL DEFAULT 'A' CHECK (pool IN ('A','B')),
		connection_dsn text,
		onboarded_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create public.tenants: %v", err)
	}
	if _, err := srcPool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.system_audit_log (
		id bigserial PRIMARY KEY,
		occurred_at timestamptz NOT NULL DEFAULT now(),
		actor_user_id text NOT NULL,
		target_tenant_id uuid,
		event_type text NOT NULL,
		payload jsonb
	)`); err != nil {
		t.Fatalf("create public.system_audit_log: %v", err)
	}
	if _, err := srcPool.Exec(ctx, `
		INSERT INTO public.tenants (id, workos_org_id, pool, connection_dsn)
		VALUES ($1, 'org_TESTAB', 'A', $2)
		ON CONFLICT (workos_org_id) DO NOTHING
	`, tenantID, source.SuperuserDSN); err != nil {
		t.Fatalf("insert public.tenants: %v", err)
	}

	// --- prepare TARGET side -----------------------------------------------
	tgtPool, err := pgxpool.New(ctx, target.SuperuserDSN)
	if err != nil {
		t.Fatalf("target pgxpool.New: %v", err)
	}
	defer tgtPool.Close()

	// Target has nothing in it — pg_restore will run CREATE SCHEMA +
	// CREATE TABLE + INSERTs as the migration proceeds.

	// --- run the migration --------------------------------------------------
	repo := tenant.NewRepo(srcPool)
	opts := tenant.MigrationOpts{
		TenantID:       tenantID,
		PoolAStreamDSN: source.SuperuserDSN,
		PoolBDSN:       target.SuperuserDSN,
		DumpPath:       "", // exercises default `/tmp/tenant_<uuid>.dump`
		Actor:          "integration-test@neksur.com",
	}
	defer os.Remove(strings.Replace("/tmp/tenant_"+tenantID.String()+".dump", "-", "_", -1))

	result, err := tenant.MigratePoolAToB(ctx, repo, opts)
	if err != nil {
		t.Fatalf("MigratePoolAToB: %v", err)
	}

	// --- assertions --------------------------------------------------------

	// Per-table parity.
	gotAuditCount := result.RowCounts["audit_log"]
	if gotAuditCount != 150 {
		t.Errorf("audit_log row count: got %d, want 150", gotAuditCount)
	}
	gotQueryCount := result.RowCounts["query_history"]
	if gotQueryCount != 50 {
		t.Errorf("query_history row count: got %d, want 50", gotQueryCount)
	}

	// public.tenants flipped to pool='B'.
	var gotPool, gotDSN string
	if err := srcPool.QueryRow(ctx,
		`SELECT pool, connection_dsn FROM public.tenants WHERE id = $1`, tenantID).
		Scan(&gotPool, &gotDSN); err != nil {
		t.Fatalf("select post-migration public.tenants: %v", err)
	}
	if gotPool != "B" {
		t.Errorf("public.tenants.pool: got %q, want %q", gotPool, "B")
	}
	if gotDSN != target.SuperuserDSN {
		t.Errorf("public.tenants.connection_dsn: got %q, want %q", gotDSN, target.SuperuserDSN)
	}

	// public.system_audit_log received the `tenant.migrated_pool_a_to_b` row.
	var gotEventType string
	if err := srcPool.QueryRow(ctx,
		`SELECT event_type FROM public.system_audit_log WHERE target_tenant_id = $1 AND event_type LIKE 'tenant.migrated%' LIMIT 1`,
		tenantID).Scan(&gotEventType); err != nil {
		t.Fatalf("select migration audit row: %v", err)
	}
	if gotEventType != "tenant.migrated_pool_a_to_b" {
		t.Errorf("audit event_type: got %q, want %q", gotEventType, "tenant.migrated_pool_a_to_b")
	}

	// RetentionDeadline is roughly 30 days out.
	if days := time.Until(result.RetentionDeadline).Hours() / 24; days < 29.5 || days > 30.5 {
		t.Errorf("RetentionDeadline not ~30 days out: got %.2f days", days)
	}
}
