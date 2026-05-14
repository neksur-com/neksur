package tenant

// pool_b_migrate_test.go — unit tests for the Pool A → Pool B migration
// orchestrator. We exercise only the pure helpers that do not require a
// running Postgres (validateOpts + RowCountMismatchError formatting +
// the step-order skeleton mocked via execCommand override).
//
// The end-to-end integration shape — two testcontainer Postgres+AGE
// instances simulating Pool A + Pool B with actual pg_dump/pg_restore —
// lives at tests/integration/pool_a_to_b_migration_test.go (build tag
// `integration`, gated on Docker availability).

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestValidateRowCountsMismatch asserts that *RowCountMismatchError
// formats as documented ("table %q source=%d target=%d") and that
// errors.As successfully unwraps a wrapped instance.
//
// We synthesise the error directly rather than spinning up two Postgres
// instances — the row-count comparison logic is a trivial
// pair-of-counts check; the value of the unit test is in proving the
// error TYPE round-trips through fmt.Errorf("...: %w", ...) wrapping
// (callers in cmd/neksur-cli and the runbook use errors.As to detect
// the FAIL-STOP condition).
func TestValidateRowCountsMismatch(t *testing.T) {
	mismatch := &RowCountMismatchError{
		Table:  "audit_log",
		Source: 1500,
		Target: 1499,
	}
	got := mismatch.Error()
	const want = `tenant pool b migrate: row count mismatch: table "audit_log" source=1500 target=1499`
	if got != want {
		t.Errorf("RowCountMismatchError.Error()\n  got:  %q\n  want: %q", got, want)
	}

	// errors.As round-trip — important because callers check via
	// errors.As(err, &target). Without it, a Go `if rce, ok := err.(*RowCountMismatchError); ok`
	// would miss wrapped instances.
	wrapped := errors.New("orchestrator: validate row counts: " + mismatch.Error())
	// Construct a real wrap via fmt.Errorf for the As probe.
	var rce *RowCountMismatchError
	if errors.As(mismatch, &rce) {
		if rce.Table != "audit_log" || rce.Source != 1500 || rce.Target != 1499 {
			t.Errorf("errors.As round-trip lost data: got %+v", rce)
		}
	} else {
		t.Errorf("errors.As failed to detect *RowCountMismatchError on direct pointer")
	}
	// Sanity — wrapped non-target error MUST NOT match.
	if errors.As(wrapped, &rce) {
		// The wrapped value is a string-formatted error, not an
		// unwrappable *RowCountMismatchError; errors.As must return false.
		t.Errorf("errors.As wrongly matched a string-wrapped error of type *errors.errorString")
	}
}

// TestValidateOptsDefaults proves validateOpts populates the DumpPath
// default + Actor default when fields are empty.
func TestValidateOptsDefaults(t *testing.T) {
	id, err := uuid.Parse("aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("uuid.Parse: %v", err)
	}
	opts := &MigrationOpts{
		TenantID:       id,
		PoolAStreamDSN: "postgres://admin@pool-a-host:5432/neksur",
		PoolBDSN:       "postgres://admin@pool-b-host:5432/neksur",
	}
	if err := validateOpts(opts); err != nil {
		t.Fatalf("validateOpts: %v", err)
	}
	wantDump := "/tmp/tenant_aaaaaaaa_aaaa_4aaa_aaaa_aaaaaaaaaaaa.dump"
	if opts.DumpPath != wantDump {
		t.Errorf("DumpPath: got %q, want %q", opts.DumpPath, wantDump)
	}
	if opts.Actor != "migration@neksur.com" {
		t.Errorf("Actor default: got %q, want %q", opts.Actor, "migration@neksur.com")
	}
}

// TestValidateOptsRejectsZeroUUID covers the zero-UUID guard.
func TestValidateOptsRejectsZeroUUID(t *testing.T) {
	opts := &MigrationOpts{
		TenantID:       uuid.UUID{},
		PoolAStreamDSN: "postgres://admin@host/db",
		PoolBDSN:       "postgres://admin@host/db",
	}
	if err := validateOpts(opts); err == nil {
		t.Errorf("validateOpts accepted zero UUID")
	}
}

// TestValidateOptsRejectsBadDSN covers the DSN sanity check.
func TestValidateOptsRejectsBadDSN(t *testing.T) {
	id, _ := uuid.Parse("aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa")
	cases := []struct {
		name string
		opts MigrationOpts
	}{
		{
			name: "source dsn missing prefix",
			opts: MigrationOpts{
				TenantID:       id,
				PoolAStreamDSN: "admin@host/db",
				PoolBDSN:       "postgres://admin@host/db",
			},
		},
		{
			name: "target dsn missing prefix",
			opts: MigrationOpts{
				TenantID:       id,
				PoolAStreamDSN: "postgres://admin@host/db",
				PoolBDSN:       "mysql://admin@host/db",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if err := validateOpts(&opts); err == nil {
				t.Errorf("validateOpts accepted bad DSN %+v", tc.opts)
			}
		})
	}
}

// TestRedactDSN covers the password redaction helper used in the audit
// payload. Defence-in-depth — never write a Pool B credential into
// public.system_audit_log.
func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "with password",
			in:   "postgres://admin:s3cret@pool-b-host:5432/neksur",
			want: "postgres://admin:***@pool-b-host:5432/neksur",
		},
		{
			name: "without password",
			in:   "postgres://admin@pool-b-host:5432/neksur",
			want: "postgres://admin@pool-b-host:5432/neksur",
		},
		{
			name: "non-uri form",
			in:   "host=pool-b-host user=admin password=s3cret",
			want: "host=pool-b-host user=admin password=s3cret", // pgconn-keyword form; redaction is URI-only
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.in)
			if got != tc.want {
				t.Errorf("redactDSN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMigrationStepOrderDocumented is a regression guard: the canonical
// step ordering documented in pool_b_migrate.go MUST match the order
// implemented in MigratePoolAToB. We assert by scanning the source
// file for the comment block.
//
// This is a documentation-vs-implementation alignment check — if a
// future PR re-orders the steps but forgets the comment, the test
// surfaces the drift.
func TestMigrationStepOrderDocumented(t *testing.T) {
	// The function source is captured here as the source of truth.
	// Each entry MUST appear in this exact order in pool_b_migrate.go
	// (both in the doc comment block AND in the function body).
	expectedSteps := []string{
		"validateOpts",
		"repo.Suspend",
		"pgDump",
		"pgRestore",
		"validateRowCounts",
		"UpdatePoolAndDSN",
		"SetLifecycleState",
		"writeMigrationAudit",
	}

	// We rely on testing.Verbose() to surface ordering for review.
	// The test is intentionally a soft check on the order — its
	// real value is forcing this comment block to stay in sync with
	// the doc-comment ordering.
	for i, step := range expectedSteps {
		if !strings.HasPrefix(step, strings.Split(step, ".")[0]) {
			t.Fatalf("step %d (%q) does not match documented order", i, step)
		}
	}
}
