package testfixture

// snowflake.go — Snowflake live-account skip-when-absent fixture.
//
// Snowflake does not offer a free testcontainer (no "localstack for
// Snowflake" exists as of Phase 3). The fixture follows the D-2.09
// PENDING_FIRST_RUN + D-3.01 nightly-CI-deferral pattern: read live
// credentials from environment variables and call t.Skipf if any are
// absent.
//
// Required environment variables:
//   - NEKSUR_SNOWFLAKE_ACCOUNT   — Snowflake account identifier
//     (e.g. "myorg-myaccount" or "xy12345.us-east-1")
//   - NEKSUR_SNOWFLAKE_USER      — Snowflake username
//   - NEKSUR_SNOWFLAKE_PASSWORD  — Snowflake password
//   - NEKSUR_SNOWFLAKE_WAREHOUSE — compute warehouse (e.g. "COMPUTE_WH")
//
// When all four are present, StartSnowflake returns a *SnowflakeClient
// for downstream test use. When any is absent, the test is skipped.
//
// D-3.01 scope: Snowflake in Phase 3 is Snowflake-as-Iceberg-REST-client
// reading Polaris-managed tables only (Mode 1). No Snowflake Horizon-
// managed Iceberg governance is attempted — that is Phase 5 (ADR-005
// ordering). Tests exercise the 4-way cross-engine read (ROADMAP §3 SC §1)
// by configuring Snowflake's external catalog integration pointing at the
// staging Polaris instance, then issuing the canonical read query.
//
// Nightly CI only: Snowflake tests are tagged `nightly` and gated on
// the `NEKSUR_SNOWFLAKE_*` env vars. They do NOT run on every PR to avoid
// Snowflake credit burn. The `.github/workflows/nightly.yml` workflow
// sets these vars via GitHub Secrets.
//
// Threat T-3-04-credential-leak (PLAN threat model — mitigate):
// Fixtures call t.Skipf on absence; tests never log env-var values;
// CI secret-scanning gates per Phase 0.5 invariant. Do NOT add any
// log.Printf or t.Logf that includes the credential values.

import (
	"os"
	"testing"
)

// SnowflakeClient carries the live Snowflake account coordinates for use
// in integration tests that configure the 4-way cross-engine read proof
// (ROADMAP §3 SC §1). This is a thin data struct — no live connections
// are established at construction time.
type SnowflakeClient struct {
	Account   string
	User      string
	Warehouse string
	// password is intentionally unexported — callers obtain it via
	// DSN() which returns a properly-formatted connection string.
	// This prevents accidental logging of the struct value.
	password string
}

// StartSnowflake reads Snowflake credentials from environment variables
// and returns a *SnowflakeClient for use in integration tests. If any
// required variable is absent, the test is skipped via t.Skipf.
//
// This function must be called from a *testing.T context (integration
// tests only, nightly CI only). It is safe to call concurrently.
func StartSnowflake(t *testing.T) *SnowflakeClient {
	t.Helper()

	account := os.Getenv("NEKSUR_SNOWFLAKE_ACCOUNT")
	user := os.Getenv("NEKSUR_SNOWFLAKE_USER")
	password := os.Getenv("NEKSUR_SNOWFLAKE_PASSWORD")
	warehouse := os.Getenv("NEKSUR_SNOWFLAKE_WAREHOUSE")

	if account == "" || user == "" || password == "" || warehouse == "" {
		t.Skipf("snowflake credentials not set — skipping per D-3.01 nightly-CI-only pattern "+
			"(set NEKSUR_SNOWFLAKE_ACCOUNT, NEKSUR_SNOWFLAKE_USER, "+
			"NEKSUR_SNOWFLAKE_PASSWORD, NEKSUR_SNOWFLAKE_WAREHOUSE to enable)")
		return nil // unreachable; t.Skipf panics, but satisfies the compiler
	}

	return &SnowflakeClient{
		Account:   account,
		User:      user,
		Warehouse: warehouse,
		password:  password,
	}
}

// DSN returns a gosnowflake-compatible connection string for the
// Snowflake account. The password is embedded; callers MUST NOT log
// the returned string.
//
// Format: `{user}:{password}@{account}/{warehouse}` per the gosnowflake
// driver DSN convention (Phase 3 D-3.02 adapter uses gosnowflake).
func (s *SnowflakeClient) DSN() string {
	if s == nil {
		return ""
	}
	return s.User + ":" + s.password + "@" + s.Account + "/" + s.Warehouse
}

// AccountURL returns the Snowflake account URL used for external catalog
// configuration. Format: `https://{account}.snowflakecomputing.com`.
// Used when configuring Snowflake's external catalog integration to point
// at the Polaris / Neksur Iceberg REST proxy (D-3.01 Mode 1).
func (s *SnowflakeClient) AccountURL() string {
	if s == nil {
		return ""
	}
	return "https://" + s.Account + ".snowflakecomputing.com"
}
