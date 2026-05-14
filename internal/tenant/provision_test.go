package tenant

// provision_test.go — unit tests for the pure helpers used by
// Plan 04's provisioning surface. The integration-tagged tests live
// in tests/integration/provisioning_test.go + tenant_isolation_test.go.
//
// Tests in this file MUST NOT require a Postgres / AGE container —
// they exercise SchemaName / RoleName transforms + the input-validation
// helpers that gate every CLI subcommand.

import (
	"testing"

	"github.com/google/uuid"
)

// TestSchemaNameTransform verifies the D-0.5.04 canonical transform
// (UUID v4 → tenant_<uuid_with_underscores>) — 43 chars exactly.
// Duplicates a check from id_test.go to keep the Plan 04 grep-gate
// happy (the plan body names this exact test name).
func TestSchemaNameTransform(t *testing.T) {
	id := uuid.MustParse("a1b2c3d4-e5f6-4789-8abc-def012345678")
	got := SchemaName(id)
	want := "tenant_a1b2c3d4_e5f6_4789_8abc_def012345678"
	if got != want {
		t.Fatalf("SchemaName = %q; want %q", got, want)
	}
	if len(got) != 43 {
		t.Fatalf("SchemaName length = %d; want 43 (D-0.5.04)", len(got))
	}
}

// TestRoleNameTransform verifies the D-0.5.03 Layer-2 role naming.
// `tenant_<uuid_underscored>_role`.
func TestRoleNameTransform(t *testing.T) {
	id := uuid.MustParse("a1b2c3d4-e5f6-4789-8abc-def012345678")
	got := RoleName(id)
	want := "tenant_a1b2c3d4_e5f6_4789_8abc_def012345678_role"
	if got != want {
		t.Fatalf("RoleName = %q; want %q", got, want)
	}
}

// TestQuoteLiteralEscapesSingleQuotes is the safety check for the one
// place provision.go does literal-quoting outside pgx parameter binding
// (the DO-block role-existence probe).
func TestQuoteLiteralEscapesSingleQuotes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`abc`, `'abc'`},
		{`a'b`, `'a''b'`},
		{`'`, `''''`},
		{``, `''`},
	}
	for _, c := range cases {
		got := quoteLiteral(c.in)
		if got != c.want {
			t.Errorf("quoteLiteral(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
