package main

// tenant_migrate_to_pool_b.go — `neksur-cli tenant migrate-to-pool-b`
// subcommand. Plan 06; orchestrates the Pool A → Pool B migration via
// internal/tenant.MigratePoolAToB.
//
// Operator-driven: announced downtime window (5–30 min per D-0.5.02);
// the `--yes` flag is REQUIRED to proceed because the operation
// suspends the tenant (read-only mode) for the duration of the move.
//
// Exit codes:
//
//	0 — migration succeeded; per-table row counts printed; retention
//	    deadline (Pool A schema drop NOT before this date) printed.
//	1 — runtime failure (pg_dump / pg_restore / DB error / row-count
//	    mismatch). On row-count mismatch, prints the offending table.
//	2 — input validation failure.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantMigrateToPoolB(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant migrate-to-pool-b", flag.ContinueOnError)
	var (
		tenantUUID       = fs.String("tenant-uuid", "", "UUID v4 of the tenant to migrate (required)")
		poolBEndpoint    = fs.String("pool-b-endpoint", "", "Pool B pgwire endpoint host[:port] (required; from `terraform output -json pool_b_endpoints`)")
		poolBDSN         = fs.String("pool-b-dsn", "", "Pool B admin DSN; if empty, composed from --pool-b-endpoint + POOL_B_ADMIN_USER / POOL_B_ADMIN_PASSWORD env vars")
		kmsKeyARN        = fs.String("kms-key-arn", "", "Per-customer KMS key ARN (logged for the audit trail; the Pool B side already references it via the rds-pool-b module)")
		instanceClass    = fs.String("instance-class", "r6g.large", "Pool B instance class for the audit payload (informational; Terraform owns the resource shape)")
		dumpPath         = fs.String("dump-path", "", "filesystem path for the pg_dump file; default /tmp/tenant_<uuid>.dump")
		actor            = fs.String("actor", "", "operator identifier written into system_audit_log payload; default migration@neksur.com")
		yes              = fs.Bool("yes", false, "REQUIRED — confirms the operator has announced the downtime window per D-0.5.02")
		keepDump         = fs.Bool("keep-dump", false, "if set, do NOT delete the dump file after migration completes (default false; T-0.5-pg-dump-leak mitigation)")
	)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenantUUID == "" || *poolBEndpoint == "" || *kmsKeyARN == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nERROR: --tenant-uuid, --pool-b-endpoint, and --kms-key-arn are all required.")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ERROR: --yes is required (operator must confirm the 5–30 min downtime window has been announced; see D-0.5.02).")
		return 2
	}

	// Input validation BEFORE any DB call (T-0.5-prov-injection).
	if err := tenant.ValidateUUIDv4(*tenantUUID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	id, err := uuid.Parse(*tenantUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uuid.Parse: %v\n", err)
		return 2
	}

	// Source DSN — the admin/master DSN for Pool A is in DATABASE_URL.
	// The migration tool MUST connect with Pool A's admin role (tenant
	// role's NOLOGIN forbids per-tenant DSN here).
	sourceDSN, code := requireEnv("DATABASE_URL")
	if code != 0 {
		return code
	}

	// Target DSN — composed from --pool-b-endpoint + env-supplied
	// credentials, OR passed verbatim via --pool-b-dsn.
	targetDSN := *poolBDSN
	if targetDSN == "" {
		user := envOrEmpty("POOL_B_ADMIN_USER")
		pass := envOrEmpty("POOL_B_ADMIN_PASSWORD")
		if user == "" || pass == "" {
			fmt.Fprintln(os.Stderr, "ERROR: either --pool-b-dsn must be provided OR both POOL_B_ADMIN_USER and POOL_B_ADMIN_PASSWORD env vars must be set.")
			return 2
		}
		// Compose `postgres://<user>:<pass>@<endpoint>/neksur` (the
		// canonical Pool B database name from the user-data template).
		targetDSN = fmt.Sprintf("postgres://%s:%s@%s/neksur", user, pass, *poolBEndpoint)
	}

	pool, err := pgxpool.New(ctx, sourceDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgxpool.New(source): %v\n", err)
		return 1
	}
	defer pool.Close()

	repo := tenant.NewRepo(pool)

	opts := tenant.MigrationOpts{
		TenantID:       id,
		PoolAStreamDSN: sourceDSN,
		PoolBDSN:       targetDSN,
		DumpPath:       *dumpPath,
		Actor:          *actor,
	}

	// Echo plan to operator for last-mile sanity.
	fmt.Fprintf(os.Stdout, "Pool A → Pool B migration plan:\n")
	fmt.Fprintf(os.Stdout, "  tenant_uuid     : %s\n", id)
	fmt.Fprintf(os.Stdout, "  schema_name     : %s\n", tenant.SchemaName(id))
	fmt.Fprintf(os.Stdout, "  pool_b_endpoint : %s\n", *poolBEndpoint)
	fmt.Fprintf(os.Stdout, "  instance_class  : %s (informational)\n", *instanceClass)
	fmt.Fprintf(os.Stdout, "  kms_key_arn     : %s\n", *kmsKeyARN)

	result, err := tenant.MigratePoolAToB(ctx, repo, opts)
	if err != nil {
		var rce *tenant.RowCountMismatchError
		if errors.As(err, &rce) {
			fmt.Fprintf(os.Stderr, "FATAL: row count mismatch (FAIL-STOP per T-0.5-row-count-mismatch): %s\n", err)
			fmt.Fprintln(os.Stderr, "Tenant remains lifecycle_state='suspended' on Pool A. Investigate before manual resume.")
		} else {
			fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		}
		return 1
	}

	// Print per-table row counts in sorted order for deterministic output.
	tables := make([]string, 0, len(result.RowCounts))
	for t := range result.RowCounts {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	fmt.Fprintf(os.Stdout, "\nOK migration complete:\n")
	fmt.Fprintf(os.Stdout, "  duration_ms     : %d\n", result.DurationMS)
	fmt.Fprintf(os.Stdout, "  schema_migrated : %s\n", result.SchemaName)
	fmt.Fprintf(os.Stdout, "  dump_path       : %s\n", result.DumpPath)
	fmt.Fprintf(os.Stdout, "  retention_until : %s (DO NOT drop Pool A schema before this date — D-0.5.02 30-day retention)\n", result.RetentionDeadline.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(os.Stdout, "\nPer-table row counts (parity confirmed):\n")
	for _, t := range tables {
		fmt.Fprintf(os.Stdout, "  %-50s %12d\n", t, result.RowCounts[t])
	}

	// Step 9 — dump cleanup (T-0.5-pg-dump-leak). Default: unlink.
	// --keep-dump preserves it for operator forensic review.
	if !*keepDump {
		if err := os.Remove(result.DumpPath); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: could not unlink dump file %s: %v\n", result.DumpPath, err)
		} else {
			fmt.Fprintf(os.Stdout, "\nDump file unlinked: %s\n", result.DumpPath)
		}
	} else {
		fmt.Fprintf(os.Stdout, "\nDump file PRESERVED at: %s (operator: rm when forensics complete)\n", result.DumpPath)
	}
	return 0
}
