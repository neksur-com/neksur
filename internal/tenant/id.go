// Package tenant carries the per-request tenant identity that flows
// from the WorkOS session middleware through to the per-tx 3-layer
// isolation discipline (D-0.5.03). It is the single point of definition
// for the tenant schema/role naming convention (D-0.5.04) and the
// provisioning-script input validators (D-0.5.21 T-0.5-prov-injection).
//
// Package boundaries:
//   - id.go        — pure helpers: SchemaName / RoleName + regex validators.
//   - context.go   — context propagation: TenantIDKey + WithID + IDFromContext.
//   - dbsession.go — WithTenantTx: per-request transaction wrapping Layer 1+2+3.
//   - repo.go      — public.tenants CRUD; ByWorkOSOrgID uses the V0044 SECURITY DEFINER lookup.
//   - errors.go    — sentinel errors used by middleware + provisioning.
//
// This file (id.go) is pure: no I/O, no logger, no globals beyond
// the compiled regex literals (PATTERNS.md line 731).
package tenant

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// SchemaName returns the Postgres schema name for a tenant per D-0.5.04.
// The UUID's hyphens are replaced with underscores so the result is a
// valid SQL identifier without requiring quoting. The schema name is
// 41 characters: literal `tenant_` (7) + 32 hex + 4 underscores = 43,
// minus the hyphen-to-underscore-of-equal-length transform = 43? Let's
// be precise: a v4 UUID rendered as canonical 36-char hex with 4 dashes,
// dashes→underscores keeps length 36; so SchemaName length = 7 + 36 = 43.
//
//	uuid:        a1b2c3d4-e5f6-4789-8abc-def012345678   (36 chars)
//	SchemaName:  tenant_a1b2c3d4_e5f6_4789_8abc_def012345678  (43 chars)
//
// One place to change format if Phase 1 wants e.g., a hash prefix instead.
func SchemaName(id uuid.UUID) string {
	return "tenant_" + strings.ReplaceAll(id.String(), "-", "_")
}

// RoleName returns the Postgres role name for a tenant per D-0.5.03 Layer 2.
// Format: <SchemaName>_role. Used by tenant/dbsession.go::WithTenantTx
// for `SET LOCAL ROLE` and by Plan 04 provisioning for `CREATE ROLE`.
func RoleName(id uuid.UUID) string {
	return SchemaName(id) + "_role"
}

// --- Regex validators (D-0.5.21 T-0.5-prov-injection) -----------------
//
// These validators are exported from id.go so the Plan 04 CLI
// subcommands (tenant_create.go / tenant_peer.go) can reject malformed
// CLI inputs BEFORE any psql / terraform interpolation. RESEARCH §Pattern 3
// lines 635–639 lists the regex literals; we re-host them here so all
// Phase 0.5 callers share one canonical regex.

// uuidV4Regex matches a canonical lowercase RFC-4122 UUID v4: 8-4-4-4-12
// hex chars with version nibble = 4 and variant nibble in {8,9,a,b}.
var uuidV4Regex = regexp.MustCompile(
	`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`,
)

// ValidateUUIDv4 returns nil if s is a canonical lowercase UUID v4.
// Returned errors wrap a sentinel so callers can `errors.Is` against
// a generic "validation failed" predicate.
func ValidateUUIDv4(s string) error {
	if !uuidV4Regex.MatchString(s) {
		return fmt.Errorf("tenant: %w: not a UUID v4: %q", ErrValidation, s)
	}
	return nil
}

// workosOrgRegex matches WorkOS organization IDs: `org_` prefix + uppercase
// alphanumeric. WorkOS dashboard generates these for every Organization.
var workosOrgRegex = regexp.MustCompile(`^org_[A-Z0-9]+$`)

// ValidateWorkOSOrgID returns nil if s is a syntactically valid
// WorkOS organization ID.
func ValidateWorkOSOrgID(s string) error {
	if !workosOrgRegex.MatchString(s) {
		return fmt.Errorf("tenant: %w: not a WorkOS org id: %q", ErrValidation, s)
	}
	return nil
}

// customerVPCRegex matches AWS VPC IDs of the modern (17-hex-char) form
// used since 2016. AWS guarantees this prefix length; older 8-char VPC
// IDs are out of scope (no customer environment created before 2016
// should be peering with Neksur).
var customerVPCRegex = regexp.MustCompile(`^vpc-[0-9a-f]{17}$`)

// ValidateCustomerVPCID returns nil if s is a syntactically valid 17-char
// AWS VPC ID.
func ValidateCustomerVPCID(s string) error {
	if !customerVPCRegex.MatchString(s) {
		return fmt.Errorf("tenant: %w: not an AWS VPC id (modern 17-char form): %q", ErrValidation, s)
	}
	return nil
}

// awsRegionRegex matches AWS region codes such as `us-east-1`, `eu-west-3`,
// `ap-southeast-2`. The relaxed form `^[a-z]{2}-[a-z]+-[0-9]$` is sufficient
// for input-validation purposes (the actual region list is enforced by AWS
// when the Terraform provider validates).
var awsRegionRegex = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[0-9]$`)

// ValidateAWSRegion returns nil if s syntactically resembles an AWS region.
func ValidateAWSRegion(s string) error {
	if !awsRegionRegex.MatchString(s) {
		return fmt.Errorf("tenant: %w: not an AWS region: %q", ErrValidation, s)
	}
	return nil
}
