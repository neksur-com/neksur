package tenant

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSchemaName(t *testing.T) {
	id := uuid.MustParse("a1b2c3d4-e5f6-4789-8abc-def012345678")
	got := SchemaName(id)
	want := "tenant_a1b2c3d4_e5f6_4789_8abc_def012345678"
	if got != want {
		t.Errorf("SchemaName = %q; want %q", got, want)
	}
	// 7 + 36 = 43 chars; no hyphens; lowercase prefix.
	if len(got) != 43 {
		t.Errorf("SchemaName length = %d; want 43", len(got))
	}
	if strings.ContainsRune(got, '-') {
		t.Errorf("SchemaName must not contain hyphens: %q", got)
	}
}

func TestRoleName(t *testing.T) {
	id := uuid.MustParse("a1b2c3d4-e5f6-4789-8abc-def012345678")
	got := RoleName(id)
	want := "tenant_a1b2c3d4_e5f6_4789_8abc_def012345678_role"
	if got != want {
		t.Errorf("RoleName = %q; want %q", got, want)
	}
}

func TestValidateUUIDv4(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		wantErr bool
	}{
		{"canonical v4", "a1b2c3d4-e5f6-4789-8abc-def012345678", false},
		{"version nibble wrong (v1)", "a1b2c3d4-e5f6-1789-8abc-def012345678", true},
		{"variant nibble wrong", "a1b2c3d4-e5f6-4789-cabc-def012345678", true},
		{"uppercase rejected", "A1B2C3D4-E5F6-4789-8ABC-DEF012345678", true},
		{"too short", "a1b2c3d4-e5f6-4789-8abc-def01234567", true},
		{"empty", "", true},
		{"garbage", "; DROP TABLE tenants; --", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateUUIDv4(c.s)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateUUIDv4(%q) err = %v; wantErr = %v", c.s, err, c.wantErr)
			}
			if c.wantErr && err != nil && !errors.Is(err, ErrValidation) {
				t.Errorf("ValidateUUIDv4(%q) err = %v; want errors.Is(err, ErrValidation)", c.s, err)
			}
		})
	}
}

func TestValidateWorkOSOrgID(t *testing.T) {
	cases := []struct {
		s       string
		wantErr bool
	}{
		{"org_01H8XYZABC123", false},
		{"org_ABCDEFGHIJ", false},
		{"ORG_AB", true},           // wrong prefix case
		{"org_lowercase", true},    // body must be uppercase
		{"org_", true},             // empty body
		{"abc_AB", true},           // wrong prefix
		{"; DROP TABLE", true},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			err := ValidateWorkOSOrgID(c.s)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateWorkOSOrgID(%q) err = %v; wantErr = %v", c.s, err, c.wantErr)
			}
		})
	}
}

func TestValidateCustomerVPCID(t *testing.T) {
	cases := []struct {
		s       string
		wantErr bool
	}{
		{"vpc-0123456789abcdef0", false},
		{"vpc-deadbeefcafebabe1", false},
		{"vpc-12345678", true},                  // old 8-char form
		{"vpc-0123456789ABCDEF0", true},         // uppercase
		{"VPC-0123456789abcdef0", true},         // wrong prefix case
		{"; DROP TABLE", true},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			err := ValidateCustomerVPCID(c.s)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateCustomerVPCID(%q) err = %v; wantErr = %v", c.s, err, c.wantErr)
			}
		})
	}
}

func TestValidateAWSRegion(t *testing.T) {
	cases := []struct {
		s       string
		wantErr bool
	}{
		{"us-east-1", false},
		{"eu-west-3", false},
		{"ap-southeast-2", false},
		{"US-EAST-1", true},
		{"us-east-", true},
		{"us east 1", true},
		{"", true},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			err := ValidateAWSRegion(c.s)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateAWSRegion(%q) err = %v; wantErr = %v", c.s, err, c.wantErr)
			}
		})
	}
}
