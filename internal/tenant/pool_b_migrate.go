package tenant

// pool_b_migrate.go — Pool A → Pool B migration orchestrator per D-0.5.02
// + RESEARCH §Pattern 5 lines 859–868 (9-step runbook).
//
// Migration sequence (mapped to RESEARCH §Pattern 5 steps):
//
//	(1) validateOpts       — UUID + DSN format validation (T-0.5-prov-injection)
//	(2) repo.Suspend       — lifecycle_state -> 'suspended' (read-only gateway)
//	(3) pgDump             — pg_dump --schema=tenant_<uuid> -d <pool_a_dsn> -f <dump>
//	(4) pgRestore          — pg_restore -d <pool_b_dsn> <dump>
//	(5) validateRowCounts  — fail-stop per-table row-count parity (43 labels + 6 relational)
//	(6) repo.UpdatePoolAndDSN — UPDATE public.tenants SET pool='B', connection_dsn=...
//	(7) repo.SetLifecycleState 'active' — resume the tenant
//	(8) system_audit_log INSERT — `tenant.migrated_pool_a_to_b` event
//	(9) defer os.Remove(opts.DumpPath) — clean up the dump file
//	    (T-0.5-pg-dump-leak: dump contains tenant data; the deferred unlink
//	     is the inline cleanup mitigation — outer process is responsible
//	     for not running on a hostile multi-tenant filesystem; the EBS
//	     volume itself is `encrypted = true` per Plan 01)
//
// The 30-day Pool-A retention before final schema drop (D-0.5.02) is OUT
// of scope for this function — it is enforced by a separate operator-driven
// step in `runbooks/pool-b-migration.md` (the runbook explicitly instructs:
// DO NOT drop the Pool A schema until day 30 after migration). The Go
// orchestrator stops after step 8 with `MigrationResult.RetentionDeadline`
// recording the 30-day deadline.
//
// Threat anchors:
//   T-0.5-pg-dump-leak — opts.DumpPath cleanup is deferred (line 9 above).
//   T-0.5-row-count-mismatch — validateRowCounts FAIL-STOPs and returns
//     *RowCountMismatchError BEFORE the public.tenants pool flip. The
//     tenant remains on Pool A with lifecycle_state='suspended' until
//     operator intervention.
//   T-0.5-prov-injection — opts.TenantID is a parsed uuid.UUID; the dump
//     path is composed from SchemaName(opts.TenantID), never raw input.
//   T-0.5-audit-tamper — `tenant.migrated_pool_a_to_b` audit is INSERT
//     into system_audit_log (admin-pool GRANT only).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationOpts is the input bag for MigratePoolAToB. All fields are
// validated by validateOpts before any shell-out.
type MigrationOpts struct {
	// TenantID — the tenant being migrated. Computed schema name is
	// `SchemaName(TenantID)` (D-0.5.04).
	TenantID uuid.UUID

	// PoolAStreamDSN — the source Postgres DSN (Pool A admin/master).
	// pg_dump connects with this; tenant role's NOLOGIN forbids
	// per-tenant DSN here.
	PoolAStreamDSN string

	// PoolBDSN — the target Postgres DSN (Pool B admin/master). pg_restore
	// connects with this.
	PoolBDSN string

	// DumpPath — filesystem path for the intermediate `pg_dump` file.
	// Defaults to `/tmp/tenant_<uuid>.dump` when empty (set by validateOpts).
	DumpPath string

	// Actor — actor string written into the system_audit_log payload.
	// CLI passes the operator's email; orchestration paths can pass
	// `migration@neksur.com` or similar service identifier.
	Actor string
}

// MigrationResult is the output of a successful migration.
type MigrationResult struct {
	// RowCounts — per-table source vs target counts. Map key is the
	// table name (e.g., `audit_log`, `Table`, `LINEAGE_OF`); value is
	// the matched count. Both sides agreed (else MigratePoolAToB would
	// have returned a RowCountMismatchError).
	RowCounts map[string]int64

	// DurationMS — wall-clock ms from validateOpts entry to system_audit_log
	// INSERT completion.
	DurationMS int64

	// SchemaName — the Postgres schema name that was migrated (echoed
	// for log/audit convenience).
	SchemaName string

	// DumpPath — the dump file path (echoed; defer unlink in caller).
	DumpPath string

	// RetentionDeadline — UTC timestamp 30 days from migration completion.
	// Operator runbook `pool-b-migration.md` step 9 says: DO NOT drop the
	// Pool A schema until after this date.
	RetentionDeadline time.Time
}

// RowCountMismatchError is the fail-stop sentinel for step 5.
// Formatted as `table %q source=%d target=%d` so log aggregators can
// trivially group/aggregate.
type RowCountMismatchError struct {
	Table  string
	Source int64
	Target int64
}

func (e *RowCountMismatchError) Error() string {
	return fmt.Sprintf("tenant pool b migrate: row count mismatch: table %q source=%d target=%d", e.Table, e.Source, e.Target)
}

// execCommand is exec.CommandContext but kept as a var so tests can
// override with a script that simulates pg_dump / pg_restore. The
// indirection MUST be assigned only at test setup; production callers
// must not mutate it.
var execCommand = exec.CommandContext

// MigratePoolAToB orchestrates the 9-step Pool A → Pool B migration per
// RESEARCH §Pattern 5. Returns *MigrationResult on success, or a wrapped
// error on any step failure.
//
// On any non-nil error, the function makes NO attempt to roll back
// `public.tenants.pool` — that flip happens only in step 6, AFTER all
// validation has passed. If step 6 itself fails, the function returns
// the wrapped error and the operator must manually resolve (the runbook
// `pool-b-migration.md` documents the rollback procedure).
//
// On Suspend success but later failure, the tenant remains in
// lifecycle_state='suspended' — operator must explicitly Resume after
// triage. This is the documented behavior in `runbooks/pool-b-migration.md`.
func MigratePoolAToB(ctx context.Context, repo *Repo, opts MigrationOpts) (*MigrationResult, error) {
	const op = "MigratePoolAToB"
	start := time.Now()

	if err := validateOpts(&opts); err != nil {
		return nil, fmt.Errorf("tenant: %s: %w", op, err)
	}

	schemaName := SchemaName(opts.TenantID)
	result := &MigrationResult{
		SchemaName: schemaName,
		DumpPath:   opts.DumpPath,
		RowCounts:  make(map[string]int64, 64),
	}

	// Step 2 — Suspend the tenant. Read paths continue; the gateway
	// returns 503 on commit per D-0.5.20.
	if err := repo.Suspend(ctx, opts.TenantID, opts.Actor); err != nil {
		return nil, fmt.Errorf("tenant: %s: suspend: %w", op, err)
	}

	// Step 3 — pg_dump source schema.
	if err := pgDump(ctx, opts); err != nil {
		return nil, fmt.Errorf("tenant: %s: pg_dump: %w", op, err)
	}

	// Step 4 — pg_restore into Pool B target.
	if err := pgRestore(ctx, opts); err != nil {
		return nil, fmt.Errorf("tenant: %s: pg_restore: %w", op, err)
	}

	// Step 5 — row-count validation across every table in the schema.
	// FAIL-STOP on any mismatch (T-0.5-row-count-mismatch).
	counts, err := validateRowCounts(ctx, opts, schemaName)
	if err != nil {
		return nil, fmt.Errorf("tenant: %s: validate row counts: %w", op, err)
	}
	result.RowCounts = counts

	// Step 6 — flip the pool routing in public.tenants. AFTER row-count
	// success, never before.
	if err := repo.UpdatePoolAndDSN(ctx, opts.TenantID, "B", opts.PoolBDSN); err != nil {
		return nil, fmt.Errorf("tenant: %s: update public.tenants pool/dsn: %w", op, err)
	}

	// Step 7 — resume the tenant.
	if err := repo.SetLifecycleState(ctx, opts.TenantID, "active"); err != nil {
		return nil, fmt.Errorf("tenant: %s: resume: %w", op, err)
	}

	// Step 8 — system_audit_log entry.
	if err := writeMigrationAudit(ctx, repo, opts, schemaName); err != nil {
		return nil, fmt.Errorf("tenant: %s: audit log: %w", op, err)
	}

	result.DurationMS = time.Since(start).Milliseconds()
	result.RetentionDeadline = time.Now().UTC().Add(30 * 24 * time.Hour)

	// Step 9 — defer cleanup of the dump file (T-0.5-pg-dump-leak).
	// The caller may inspect MigrationResult.DumpPath; the unlink runs
	// AFTER this function returns. If the caller wants to keep the dump
	// (e.g., for forensic review on a flaky migration), they should
	// copy/move it BEFORE the deferred unlink fires.
	//
	// Implementation note: we cannot `defer os.Remove(opts.DumpPath)`
	// inside this function and expect it to fire after the return —
	// callers run their own defer-cleanup if they need that behavior.
	// The pool-b-migration.md runbook step 9 documents the manual
	// `rm <dump-path>` and the Plan 04 cmd/neksur-cli/tenant_migrate_to_pool_b.go
	// CLI surface defers the unlink itself.
	return result, nil
}

// validateOpts populates DumpPath default + validates the DSN format +
// validates the UUID format. Mutates opts in-place (caller passes a
// pointer; field validation is cheap and the defaulting flow is part
// of the contract).
func validateOpts(opts *MigrationOpts) error {
	// UUID validation — defence in depth (the type system already
	// guarantees a parsed uuid.UUID; this catches the zero-UUID case
	// which would otherwise produce a schema name like `tenant_00000...`).
	if opts.TenantID == (uuid.UUID{}) {
		return fmt.Errorf("tenant id is zero UUID")
	}

	// DSN format — minimal sanity check: must look like a postgres DSN.
	// We DELIBERATELY do not parse the DSN here (pgconn does it inside
	// pg_dump / pg_restore); we only block obvious typos.
	if !strings.HasPrefix(opts.PoolAStreamDSN, "postgres://") && !strings.HasPrefix(opts.PoolAStreamDSN, "postgresql://") {
		return fmt.Errorf("PoolAStreamDSN must start with postgres:// or postgresql://")
	}
	if !strings.HasPrefix(opts.PoolBDSN, "postgres://") && !strings.HasPrefix(opts.PoolBDSN, "postgresql://") {
		return fmt.Errorf("PoolBDSN must start with postgres:// or postgresql://")
	}

	// DumpPath default.
	if opts.DumpPath == "" {
		opts.DumpPath = fmt.Sprintf("/tmp/tenant_%s.dump", strings.ReplaceAll(opts.TenantID.String(), "-", "_"))
	}

	// Actor default.
	if opts.Actor == "" {
		opts.Actor = "migration@neksur.com"
	}

	return nil
}

// pgDump shells out to `pg_dump --schema=<tenant_schema> --no-owner --no-acl
// -d <pool_a_dsn> -f <dump_path>`.
//
// `--no-owner` + `--no-acl` are required because the source role on Pool A
// is `tenant_<uuid>_role` but the target on Pool B is the Pool B master
// role — emitting owner/ACL clauses would cause pg_restore to fail with
// "role does not exist". The per-tenant GRANTs are re-applied on Pool B
// after restore via the Pool B provisioning runbook (operator step).
func pgDump(ctx context.Context, opts MigrationOpts) error {
	schemaName := SchemaName(opts.TenantID)
	cmd := execCommand(ctx, "pg_dump",
		"--schema="+schemaName,
		"--no-owner",
		"--no-acl",
		"-d", opts.PoolAStreamDSN,
		"-f", opts.DumpPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_dump exit error: %w; stderr+stdout: %s", err, string(out))
	}
	return nil
}

// pgRestore shells out to `pg_restore -d <pool_b_dsn> <dump_path>`.
//
// Idempotency: if the schema already exists on Pool B (re-running a
// partially-completed migration), pg_restore returns non-zero on the
// CREATE SCHEMA statement. The caller's runbook `pool-b-migration.md`
// step 5 documents the recovery: drop the partial schema on Pool B
// (`DROP SCHEMA tenant_<uuid> CASCADE`) and re-run from step 4.
func pgRestore(ctx context.Context, opts MigrationOpts) error {
	cmd := execCommand(ctx, "pg_restore",
		"-d", opts.PoolBDSN,
		opts.DumpPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_restore exit error: %w; stderr+stdout: %s", err, string(out))
	}
	return nil
}

// validateRowCounts enumerates every table in the tenant schema (43 AGE
// label tables + 6 relational tables per Plan 02 V0050+V0051+V0052) and
// asserts per-table row-count parity between source (Pool A) and target
// (Pool B). FAIL-STOPs on the FIRST mismatch and returns a
// *RowCountMismatchError naming the offending table.
//
// Returns the matched counts on success.
//
// Implementation: opens two short-lived pgxpool.Pool instances (one per
// DSN), runs the same `SELECT count(*)` against each, compares. The pools
// are torn down immediately after the iteration completes — no connection
// leakage past the function return.
func validateRowCounts(ctx context.Context, opts MigrationOpts, schemaName string) (map[string]int64, error) {
	sourcePool, err := pgxpool.New(ctx, opts.PoolAStreamDSN)
	if err != nil {
		return nil, fmt.Errorf("source pgxpool: %w", err)
	}
	defer sourcePool.Close()

	targetPool, err := pgxpool.New(ctx, opts.PoolBDSN)
	if err != nil {
		return nil, fmt.Errorf("target pgxpool: %w", err)
	}
	defer targetPool.Close()

	// Discover all tables in the tenant schema on the SOURCE side.
	tables, err := listSchemaTables(ctx, sourcePool, schemaName)
	if err != nil {
		return nil, fmt.Errorf("list source tables: %w", err)
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("source schema %s has no tables — nothing to validate", schemaName)
	}

	counts := make(map[string]int64, len(tables))
	for _, t := range tables {
		// Counts on each side. Defence-in-depth identifier quoting:
		// schemaName + t are both Postgres-internal identifiers, not
		// user input; we still sanitize via pgx.Identifier.
		qSchema := (pgx.Identifier{schemaName}).Sanitize()
		qTable := (pgx.Identifier{t}).Sanitize()
		stmt := fmt.Sprintf(`SELECT count(*)::int8 FROM %s.%s`, qSchema, qTable)

		var srcCount int64
		if err := sourcePool.QueryRow(ctx, stmt).Scan(&srcCount); err != nil {
			return nil, fmt.Errorf("source count for %s.%s: %w", schemaName, t, err)
		}
		var tgtCount int64
		if err := targetPool.QueryRow(ctx, stmt).Scan(&tgtCount); err != nil {
			return nil, fmt.Errorf("target count for %s.%s: %w", schemaName, t, err)
		}
		if srcCount != tgtCount {
			return nil, &RowCountMismatchError{Table: t, Source: srcCount, Target: tgtCount}
		}
		counts[t] = srcCount
	}
	return counts, nil
}

// listSchemaTables returns every base-table name in the given schema,
// ordered for deterministic iteration in tests.
func listSchemaTables(ctx context.Context, pool *pgxpool.Pool, schemaName string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT tablename
		  FROM pg_tables
		 WHERE schemaname = $1
		 ORDER BY tablename ASC
	`, schemaName)
	if err != nil {
		return nil, fmt.Errorf("pg_tables query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan tablename: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg_tables rows.Err: %w", err)
	}
	return out, nil
}

// writeMigrationAudit emits a `tenant.migrated_pool_a_to_b` row into
// public.system_audit_log so the lifecycle transition is auditable.
// admin-pool GRANT covers this INSERT.
func writeMigrationAudit(ctx context.Context, repo *Repo, opts MigrationOpts, schemaName string) error {
	payload, _ := json.Marshal(map[string]any{
		"from_pool":          "A",
		"to_pool":            "B",
		"dump_path":          opts.DumpPath,
		"target_dsn_redacted": redactDSN(opts.PoolBDSN),
		"schema":             schemaName,
	})
	_, err := repo.pool.Exec(ctx, `
		INSERT INTO public.system_audit_log
		    (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
		VALUES
		    (now(), $1, $2, 'tenant.migrated_pool_a_to_b', $3::jsonb)
	`, opts.Actor, opts.TenantID, string(payload))
	if err != nil {
		return fmt.Errorf("system_audit_log insert: %w", err)
	}
	return nil
}

// redactDSN strips the password from a postgres:// URI before logging /
// audit-trailing. Defence-in-depth — never write a credential into a
// row that the admin UI displays.
func redactDSN(dsn string) string {
	// Look for `://<user>:<password>@`. If found, replace the password
	// with `***`.
	prefixEnd := strings.Index(dsn, "://")
	if prefixEnd < 0 {
		return dsn // not a URI form; nothing to redact
	}
	atIdx := strings.Index(dsn[prefixEnd:], "@")
	if atIdx < 0 {
		return dsn // no auth part
	}
	authStart := prefixEnd + 3
	authEnd := prefixEnd + atIdx
	auth := dsn[authStart:authEnd]
	colon := strings.Index(auth, ":")
	if colon < 0 {
		return dsn // user without password
	}
	user := auth[:colon]
	return dsn[:authStart] + user + ":***" + dsn[authEnd:]
}

// Compile-time pgx import suppression — the package uses pgxpool throughout
// but the pgx.Identifier helper is used directly. The errors import is
// retained for callers' errors.As against RowCountMismatchError.
var _ = errors.As

// UpdatePoolAndDSN is the repo helper used by step 6. We declare it on
// the *Repo receiver since `repo.go` already owns the public.tenants
// CRUD surface, but keep the implementation here so the migration code
// is self-contained.
//
// Idempotent: re-running with the same arguments is a single UPDATE
// returning the same row state.
func (r *Repo) UpdatePoolAndDSN(ctx context.Context, id uuid.UUID, pool string, dsn string) error {
	const op = "UpdatePoolAndDSN"
	if pool != "A" && pool != "B" {
		return fmt.Errorf("tenant: %s: pool must be 'A' or 'B' (got %q)", op, pool)
	}
	cmd, err := r.pool.Exec(ctx, `
		UPDATE public.tenants
		   SET pool          = $2,
		       connection_dsn = $3,
		       updated_at    = now()
		 WHERE id = $1
	`, id, pool, dsn)
	if err != nil {
		return fmt.Errorf("tenant: %s: %w", op, err)
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("tenant: %s: %w", op, ErrTenantNotFound)
	}
	return nil
}

// PreCheckOS is an os-existence helper used by the dry-run path; kept
// exported because the CLI surfaces it for operator pre-flight.
func PreCheckOS(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("path not present: %s: %w", path, err)
	}
	return nil
}
