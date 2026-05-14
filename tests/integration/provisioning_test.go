//go:build integration

// Package integration — Plan 04 end-to-end onboarding rehearsal tests.
//
// TestProvisioningIdempotent — exercises the 12-step rehearsal against
//   a real Postgres+AGE testcontainer. Re-runs each step twice (CreateGraph,
//   CreateRole, ApplyTenantMigrations) and verifies that:
//     a. Second invocation returns nil (idempotent).
//     b. audit_log table exists exactly once.
//     c. Atlas revisions show V0050/V0051/V0052 applied to the tenant
//        schema in public.atlas_schema_revisions.
//
// TestProvisioningRegex — table-driven exercise of all four input
//   validators (uuid v4 / workos org / vpc id / aws region). Positive
//   + negative cases per RESEARCH §Pattern 3 line 635.

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

// TestProvisioningIdempotent — runs the bootstrap steps twice on the
// same tenant and asserts:
//   1. Both invocations return nil (no error on the second pass).
//   2. The tenant_<uuid>.audit_log table exists exactly once.
//   3. public.atlas_schema_revisions contains V0050/V0051/V0052 entries
//      for the tenant pass.
//
// Uses StartSaasFixture for the testcontainer. Does NOT exercise the
// peering / cert-issue steps — those need LocalStack or AWS, and
// Plan 04's CLI wires PROVISION_MOCK_TERRAFORM=1 + an empty
// PRIVATE_CA_ARN to keep them stub-only in CI.
func TestProvisioningIdempotent(t *testing.T) {
	ctx := context.Background()
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const tenantUUID = "cccccccc-cccc-4ccc-cccc-cccccccccccc"
	const tenantSchema = "tenant_cccccccc_cccc_4ccc_cccc_cccccccccccc"
	id := uuid.MustParse(tenantUUID)

	// Build the admin pool + GraphClient mirroring the CLI wiring.
	pool, err := pgxpool.New(ctx, fx.SuperDSN())
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	gc, err := graph.NewGraphClient(ctx, fx.SuperDSN())
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	repo := tenant.NewRepo(pool)
	provisioner := tenant.NewProvisioner(gc, pool, repo, fx.SuperDSN(), "", "")

	// Plan 04 step (a/b) — INSERT public.tenants. ON CONFLICT DO NOTHING
	// makes this idempotent already.
	for i := 0; i < 2; i++ {
		if err := repo.Create(ctx, tenant.Tenant{
			ID:             id,
			WorkOSOrgID:    "org_TESTCCC",
			LifecycleState: "active",
			Pool:           "A",
		}); err != nil {
			t.Fatalf("repo.Create iter %d: %v", i+1, err)
		}
	}

	// Step (c) — CreateGraph idempotent.
	for i := 0; i < 2; i++ {
		if err := provisioner.CreateGraph(ctx, id); err != nil {
			t.Fatalf("CreateGraph iter %d: %v", i+1, err)
		}
	}

	// Step (d) — CreateRole idempotent.
	for i := 0; i < 2; i++ {
		if err := provisioner.CreateRole(ctx, id); err != nil {
			t.Fatalf("CreateRole iter %d: %v", i+1, err)
		}
	}

	// Step (e) — ApplyTenantMigrations idempotent (Atlas revisions
	// skip already-applied versions).
	for i := 0; i < 2; i++ {
		if err := provisioner.ApplyTenantMigrations(ctx, id); err != nil {
			t.Fatalf("ApplyTenantMigrations iter %d: %v", i+1, err)
		}
	}

	// Post-step REVOKE — also idempotent (REVOKE-of-a-non-existing-
	// privilege is a no-op).
	for i := 0; i < 2; i++ {
		if err := provisioner.RevokeAuditLogWrites(ctx, id); err != nil {
			t.Fatalf("RevokeAuditLogWrites iter %d: %v", i+1, err)
		}
	}

	// Assert 1: audit_log exists exactly once in the tenant schema.
	probe, err := pgx.Connect(ctx, fx.SuperDSN())
	if err != nil {
		t.Fatalf("probe pgx.Connect: %v", err)
	}
	defer probe.Close(ctx)

	var auditCount int
	if err := probe.QueryRow(ctx, `
		SELECT count(*) FROM pg_tables
		 WHERE schemaname = $1 AND tablename = 'audit_log'
	`, tenantSchema).Scan(&auditCount); err != nil {
		t.Fatalf("audit_log probe: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 audit_log in %s, got %d", tenantSchema, auditCount)
	}

	// Assert 2: V0050/V0051/V0052 in public.atlas_schema_revisions.
	// Atlas records each applied version exactly once even if the
	// migration was re-run.
	rows, err := probe.Query(ctx, `
		SELECT version FROM public.atlas_schema_revisions
		 WHERE version IN ('0050', '0051', '0052') ORDER BY version
	`)
	if err != nil {
		t.Fatalf("atlas_schema_revisions probe: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(versions) < 3 {
		t.Fatalf("expected V0050/V0051/V0052 in atlas_schema_revisions, got %v", versions)
	}

	t.Logf("Idempotency PASS — audit_log count=%d, versions=%v", auditCount, versions)
}

// TestProvisioningRegex — table-driven validator exercise. Each row is
// (description, input, validator_choice, want_err).
//
// Mirrors id_test.go's per-validator tests but consolidates them into
// the named Plan 04 acceptance gate (grep gate looks for the function
// name verbatim).
func TestProvisioningRegex(t *testing.T) {
	type validator int
	const (
		vUUID validator = iota
		vWorkOS
		vVPC
		vRegion
	)
	cases := []struct {
		name    string
		input   string
		which   validator
		wantErr bool
	}{
		// UUID v4 positive
		{"uuid_canonical_v4", "a1b2c3d4-e5f6-4789-8abc-def012345678", vUUID, false},
		{"uuid_all_a_v4", "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa", vUUID, false},
		// UUID v4 negative
		{"uuid_v1_version", "a1b2c3d4-e5f6-1789-8abc-def012345678", vUUID, true},
		{"uuid_uppercase", "A1B2C3D4-E5F6-4789-8ABC-DEF012345678", vUUID, true},
		{"uuid_too_short", "a1b2c3d4-e5f6-4789-8abc-def01234567", vUUID, true},
		{"uuid_garbage", "; DROP TABLE tenants; --", vUUID, true},
		{"uuid_empty", "", vUUID, true},

		// WorkOS org positive
		{"workos_basic", "org_ABC123", vWorkOS, false},
		{"workos_numeric_only_after_prefix", "org_001H8XYZ", vWorkOS, false},
		// WorkOS org negative
		{"workos_lowercase", "org_lowercase", vWorkOS, true},
		{"workos_upper_prefix", "ORG_ABC", vWorkOS, true},
		{"workos_empty_body", "org_", vWorkOS, true},
		{"workos_bare_garbage", "; DROP TABLE", vWorkOS, true},

		// VPC ID positive
		{"vpc_canonical_17", "vpc-0123456789abcdef0", vVPC, false},
		{"vpc_deadbeef_17", "vpc-deadbeefcafebabe1", vVPC, false},
		// VPC ID negative
		{"vpc_short_8", "vpc-12345678", vVPC, true},
		{"vpc_uppercase", "vpc-0123456789ABCDEF0", vVPC, true},
		{"vpc_upper_prefix", "VPC-0123456789abcdef0", vVPC, true},

		// Region positive
		{"region_us_east_1", "us-east-1", vRegion, false},
		{"region_eu_west_3", "eu-west-3", vRegion, false},
		{"region_ap_southeast_2", "ap-southeast-2", vRegion, false},
		// Region negative
		{"region_us_east_99", "us-east-99", vRegion, true}, // two-digit suffix outside relaxed regex
		{"region_uppercase", "US-EAST-1", vRegion, true},
		{"region_trailing_dash", "us-east-", vRegion, true},
		{"region_spaces", "us east 1", vRegion, true},
		{"region_empty", "", vRegion, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			switch c.which {
			case vUUID:
				err = tenant.ValidateUUIDv4(c.input)
			case vWorkOS:
				err = tenant.ValidateWorkOSOrgID(c.input)
			case vVPC:
				err = tenant.ValidateCustomerVPCID(c.input)
			case vRegion:
				err = tenant.ValidateAWSRegion(c.input)
			}
			if (err != nil) != c.wantErr {
				t.Errorf("input=%q wantErr=%v gotErr=%v", c.input, c.wantErr, err)
			}
			if c.wantErr && err != nil && !strings.Contains(err.Error(), "validation failed") {
				t.Errorf("input=%q: error message %q should wrap ErrValidation", c.input, err)
			}
		})
	}
}
