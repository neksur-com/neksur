//go:build integration

// Plan 01-01 Task 4 [BLOCKING] — Phase 1 relational migrations end-to-end.
//
// TestPhase1MigrationsAppliedPerTenant boots the full 4-container Phase 1
// fixture (Postgres+AGE, Polaris, Nessie, LocalStack) and provisions two
// tenants. It verifies that V0060-V0066 land in each per-tenant schema:
//
//   1. atlas_schema_revisions records versions 0060-0066.
//   2. catalog_credentials has the expected columns.
//   3. catalog_credentials has FORCE RLS attached (V0066).
//   4. detection_runs UNIQUE (snapshot_metadata_location) raises
//      SQLSTATE 23505 on duplicate insert (Pitfall 10 mitigation).
//
// PASS exit-0 proves Atlas's tenant-loop + Plan 01-01's
// internal/migrate.ApplyTenantGraph (called via Phase1Fixture.ProvisionTenant)
// applied the entire Phase 1 relational substrate cleanly per tenant.

package integration

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	phase1TenantA = "11111111-1111-4111-1111-111111111111"
	phase1TenantB = "22222222-2222-4222-2222-222222222222"
)

// expectedV0060V0066 is the list of Atlas revisions Plan 01-01 lands
// in each tenant schema. The trailing string is what Atlas records in
// the `version` column of <schema>.atlas_schema_revisions.
var expectedV0060V0066 = []string{
	"0060", "0061", "0062", "0063", "0064", "0065", "0066",
}

func TestPhase1MigrationsAppliedPerTenant(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	schemaA := fx.ProvisionTenant(t, phase1TenantA)
	schemaB := fx.ProvisionTenant(t, phase1TenantB)

	for _, schema := range []string{schemaA, schemaB} {
		t.Run("tenant_"+schema, func(t *testing.T) {
			conn, err := pgx.Connect(fx.ctx, fx.Container.SuperuserDSN)
			if err != nil {
				t.Fatalf("pgx.Connect: %v", err)
			}
			defer conn.Close(fx.ctx)

			assertAtlasRevisionsPresent(t, conn, schema)
			assertCatalogCredentialsColumns(t, conn, schema)
			assertCatalogCredentialsForceRLS(t, conn, schema)
			assertDetectionRunsUniqueConstraint(t, conn, schema)
		})
	}
}

// assertAtlasRevisionsPresent verifies <schema>.atlas_schema_revisions
// records the V0060-V0066 versions. Atlas records the version as the
// 4-digit prefix; the test compares the substring.
func assertAtlasRevisionsPresent(t *testing.T, conn *pgx.Conn, schema string) {
	t.Helper()
	qSchema := pgx.Identifier{schema}.Sanitize()
	rows, err := conn.Query(t.Context(),
		fmt.Sprintf(`SELECT version FROM %s.atlas_schema_revisions ORDER BY version`, qSchema))
	if err != nil {
		t.Fatalf("query atlas_schema_revisions: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	// Atlas may format versions with file-path suffixes; verify each
	// expected V0060-V0066 substring appears at least once.
	for _, want := range expectedV0060V0066 {
		found := false
		for _, g := range got {
			if g == want || (len(g) >= 4 && g[:4] == want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("schema %s: missing revision %q in %v", schema, want, got)
		}
	}
}

// assertCatalogCredentialsColumns verifies the table has the expected
// columns from V0060.
func assertCatalogCredentialsColumns(t *testing.T, conn *pgx.Conn, schema string) {
	t.Helper()
	rows, err := conn.Query(t.Context(),
		`SELECT column_name
		   FROM information_schema.columns
		  WHERE table_schema = $1
		    AND table_name   = 'catalog_credentials'
		  ORDER BY ordinal_position`,
		schema)
	if err != nil {
		t.Fatalf("query catalog_credentials columns: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan column_name: %v", err)
		}
		got = append(got, c)
	}
	want := []string{"id", "catalog_kind", "nickname", "endpoint", "config_json", "encrypted_secret", "created_at"}
	for _, w := range want {
		present := false
		for _, g := range got {
			if g == w {
				present = true
				break
			}
		}
		if !present {
			t.Errorf("schema %s: missing catalog_credentials column %q (got %v)", schema, w, got)
		}
	}
}

// assertCatalogCredentialsForceRLS verifies V0066 attached FORCE RLS to
// catalog_credentials. Reads pg_class.relrowsecurity (RLS enabled) and
// pg_class.relforcerowsecurity (FORCE bit).
func assertCatalogCredentialsForceRLS(t *testing.T, conn *pgx.Conn, schema string) {
	t.Helper()
	var rowsec, forced bool
	if err := conn.QueryRow(t.Context(), `
		SELECT c.relrowsecurity, c.relforcerowsecurity
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1
		   AND c.relname = 'catalog_credentials'`, schema).Scan(&rowsec, &forced); err != nil {
		t.Fatalf("query pg_class for catalog_credentials: %v", err)
	}
	if !rowsec {
		t.Errorf("schema %s: catalog_credentials.relrowsecurity = false; expected true (V0066)", schema)
	}
	if !forced {
		t.Errorf("schema %s: catalog_credentials.relforcerowsecurity = false; expected true (V0066 FORCE RLS)", schema)
	}
}

// assertDetectionRunsUniqueConstraint verifies V0062's UNIQUE on
// snapshot_metadata_location (Pitfall 10 mitigation). Inserts two
// rows with the same metadata_location; the second MUST raise
// SQLSTATE 23505 (unique_violation).
func assertDetectionRunsUniqueConstraint(t *testing.T, conn *pgx.Conn, schema string) {
	t.Helper()
	qSchema := pgx.Identifier{schema}.Sanitize()
	const loc = "s3://test-bucket/metadata/00001-abc.metadata.json"
	insertSQL := fmt.Sprintf(`
		INSERT INTO %s.detection_runs (run_id, snapshot_metadata_location, scan_strategy, sample_size)
		VALUES (gen_random_uuid(), $1, 'regex', 100)`, qSchema)
	if _, err := conn.Exec(t.Context(), insertSQL, loc); err != nil {
		t.Fatalf("schema %s: first detection_runs insert: %v", schema, err)
	}
	_, err := conn.Exec(t.Context(), insertSQL, loc)
	if err == nil {
		t.Fatalf("schema %s: second detection_runs insert with same metadata_location should fail; got nil error", schema)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("schema %s: unexpected error type %T: %v", schema, err, err)
	}
	if pgErr.SQLState() != "23505" {
		t.Errorf("schema %s: expected SQLSTATE 23505 (unique_violation); got %s (%s)", schema, pgErr.SQLState(), pgErr.Message)
	}
}
