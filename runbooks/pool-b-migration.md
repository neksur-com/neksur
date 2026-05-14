# Runbook: Pool A → Pool B Migration

**Owner:** Phase 0.5 SRE / first-on-call
**Scope:** Operator moves an existing tenant from the shared Pool A to a
dedicated per-Enterprise-customer Pool B instance.
**Contract:** **D-0.5.02** (LOCKED) — Pool B = dedicated Postgres + AGE
per Enterprise; 5–30 min announced downtime; 30-day Pool A retention.
**Validation tests:** `tests/integration/pool_a_to_b_migration_test.go`
(two AGE testcontainers; integration build tag).
**Closes:** ROADMAP §Phase 0.5 success criterion 5 (Pool B migration
workflow documented + exercised).
**Cross-ref:** `internal/tenant/pool_b_migrate.go` Go orchestrator;
`./neksur-cli tenant migrate-to-pool-b` CLI surface.

---

## Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **Tenant in `lifecycle_state='active'` on Pool A** | yes | `SELECT lifecycle_state FROM public.tenants WHERE id = '<uuid>'` |
| **Pool B is provisioned for the customer** | yes (per `pool-b-provisioning.md`) | `terraform -chdir=environments/phase0-pilot output pool_b_endpoints` shows the customer's UUID |
| **Pool B endpoint is reachable from the jumphost** | `pg_isready -h <pool_b_endpoint>` | run from inside the Neksur VPC |
| **Downtime window is announced to the customer** | 5–30 min per D-0.5.02 | customer-confirmed in writing |
| **`pg_dump` + `pg_restore` on the jumphost** | `pg_dump --version`, `pg_restore --version` | postgresql-client package |
| **`./neksur-cli` built from current main** | `go build -o neksur-cli ./cmd/neksur-cli` | |
| **`DATABASE_URL` = Pool A admin DSN** | exported in environment | not a tenant DSN; the migration tool needs admin role |
| **`POOL_B_ADMIN_USER` + `POOL_B_ADMIN_PASSWORD`** | from Secrets Manager `neksur/pool-b/<uuid>/admin` | `aws secretsmanager get-secret-value` |
| **Operator email** | for the audit trail | passed via `--actor` to the CLI |

If any prereq fails, **HALT**. The migration tool's row-count validation
is fail-stop but the most expensive failure mode is starting the
migration before the customer is ready.

---

## 1. Announce the downtime window

Notify the customer (over their agreed channel) that you are starting
the migration. The 5–30 min window starts NOW; the customer's Spark /
gateway writes will receive **HTTP 503 on commit** for the duration
(D-0.5.20 lifecycle_state='suspended' contract).

**Read paths continue** during the window — dashboards keep working;
only mutations are blocked. This is explicit in D-0.5.20.

## 2. (If not already done) `terraform apply` Pool B

Per `runbooks/pool-b-provisioning.md` step 3. The Pool B instance must
already exist before the migration starts; the migration step here is
data-movement only, not infrastructure provisioning.

## 3. Suspend the tenant

The CLI subcommand does this AUTOMATICALLY as step 2 of the migration
sequence (`internal/tenant/pool_b_migrate.go` → `repo.Suspend`). However
some operators prefer an explicit pre-migration suspend so the customer
sees the read-only window start AT a known wall-clock time rather than
buried inside the migration flow:

```bash
# OPTIONAL — explicit pre-migration suspend (the CLI does this anyway).
./neksur-cli tenant suspend --tenant-uuid "$TENANT_UUID" --actor "$OPERATOR_EMAIL"
```

## 4. `pg_dump` the tenant schema from Pool A

The CLI does this AUTOMATICALLY as step 3 of the migration sequence —
shelling out to:

```bash
pg_dump --schema=tenant_<uuid_underscored> --no-owner --no-acl \
        -d "$DATABASE_URL" \
        -f /tmp/tenant_<uuid_underscored>.dump
```

`--no-owner --no-acl` are required because the source role
(`tenant_<uuid>_role` on Pool A) does NOT exist on Pool B; per-tenant
GRANTs are re-applied via the Pool B provisioning runbook step 6.

The dump file lives on the jumphost at `/tmp/tenant_<uuid>.dump` (default
path; override with `--dump-path` if needed). The CLI redacts the dump
path from the system_audit_log audit row but the file itself is plaintext
tenant data — defence-in-depth: the EBS volume is `encrypted = true`
per Plan 01, and the CLI defers the unlink after migration completes.

## 5. `pg_restore` into Pool B

The CLI does this AUTOMATICALLY as step 4 of the migration sequence —
shelling out to:

```bash
pg_restore -d "postgres://admin:$POOL_B_ADMIN_PASSWORD@$POOL_B_ENDPOINT/neksur" \
           /tmp/tenant_<uuid_underscored>.dump
```

If Pool B already has a partial schema (re-running a failed migration):
**STOP**, manually `DROP SCHEMA tenant_<uuid> CASCADE` on Pool B FIRST,
then re-run the CLI. The runbook's recovery path for a partially-
restored Pool B is documented in `internal/tenant/pool_b_migrate.go`
`pgRestore` function comment.

## 6. Row-count validation (FAIL-STOP)

Step 5 of the migration sequence — the CLI enumerates every table in
`tenant_<uuid>.*` on the source (Pool A) AND target (Pool B) and asserts
per-table parity:

- **43 AGE label tables** (canonical Phase 0 + Phase 1 labels) — these
  may have zero rows on a fresh Pool B tenant pre-migration, which is
  fine; the assertion compares source vs target counts, not absolute
  values.
- **6 relational tables** (audit_log, query_history, policies,
  classifications, data_contracts, semantic_model_versions).

On ANY per-table mismatch, the migration FAIL-STOPs with
`*RowCountMismatchError` (formatted `table %q source=%d target=%d`).
The tenant remains in `lifecycle_state='suspended'` on Pool A; the
operator must investigate the row-count drift BEFORE resuming. Common
causes:

| Cause | Diagnostic | Fix |
|-------|------------|-----|
| pg_restore partial failure | Check `pg_restore` stderr from the CLI output | Drop partial schema on Pool B, re-run from step 5 |
| Concurrent write to Pool A during dump | `SELECT ... FROM public.system_audit_log WHERE target_tenant_id = '<uuid>' ORDER BY occurred_at DESC LIMIT 5` | Confirm the suspend took effect; if not, re-suspend + re-dump |
| AGE label table count drift | Per-label count comparison | This is a known AGE quirk if the source dump ran during a vacuum; re-dump |

## 7. Update `public.tenants` to point at Pool B

Step 6 of the migration sequence (the CLI does this automatically AFTER
row-count parity is confirmed). Single UPDATE in a transaction:

```sql
UPDATE public.tenants
   SET pool          = 'B',
       connection_dsn = '<pool_b_endpoint>',
       updated_at    = now()
 WHERE id = '<tenant_uuid>';
```

After this commit, ALL new application requests for this tenant route
to Pool B (the gateway looks up `pool` + `connection_dsn` per request
from `public.tenants`).

## 8. Resume the tenant

Step 7 of the migration sequence. `lifecycle_state` flips back to
`'active'`; HTTP 503 on commit stops. Customer's Spark writes resume.

## 9. Pool A retention (30 days)

**DO NOT drop the Pool A schema yet.** Per D-0.5.02 the Pool A schema
is kept for 30 days post-migration as a rollback safety net. The CLI
emits `MigrationResult.RetentionDeadline` (now + 30 days UTC); record
this date in the customer's onboarding ticket.

After 30 days, drop the Pool A schema:

```bash
# Wait until result.RetentionDeadline has passed.
DATABASE_URL="postgres://admin@pool-a-host:5432/neksur" \
  ./neksur-cli tenant delete --tenant-uuid "$TENANT_UUID" --pool A --yes
```

(Phase 0.5 ships `tenant_delete.go` as Plan 07; if Plan 07 hasn't
landed yet at the time of the 30-day cutoff, use raw SQL:
`DROP SCHEMA tenant_<uuid> CASCADE` via the admin pool.)

---

## 10. Audit trail

Every migration writes a `tenant.migrated_pool_a_to_b` row to
`public.system_audit_log`. Verify post-migration:

```sql
SELECT occurred_at, actor_user_id, payload
  FROM public.system_audit_log
 WHERE target_tenant_id = '<tenant_uuid>'
   AND event_type = 'tenant.migrated_pool_a_to_b'
 ORDER BY occurred_at DESC
 LIMIT 1;
```

The payload includes `from_pool`, `to_pool`, `dump_path`, and the
redacted target DSN. Pool B passwords are NEVER written into the audit
log (`redactDSN` helper).

---

## 11. Rollback procedure (if needed within 30 days)

If the migration causes customer-visible regressions WITHIN 30 days of
completion:

1. Suspend the tenant again: `./neksur-cli tenant suspend ...`.
2. Re-apply `UPDATE public.tenants SET pool='A', connection_dsn='<pool_a_endpoint>' WHERE id='<uuid>'` against Pool A's admin DSN.
3. Resume: `./neksur-cli tenant resume ...`.

The Pool A schema was retained per step 9, so this is reversible
within the retention window. After the window closes, rollback requires
PITR restore per `runbooks/restore-pitr.md` §8.b (per-tenant restore).

---

## 12. Cross-references

- `internal/tenant/pool_b_migrate.go` — Go orchestrator (9-step
  sequence; the runbook above mirrors the step numbers)
- `cmd/neksur-cli/tenant_migrate_to_pool_b.go` — CLI surface
- `runbooks/pool-b-provisioning.md` — Pool B Terraform + initial-tenant
  flow (prerequisite to this migration)
- `runbooks/restore-pitr.md` §9 — Pool B-specific PITR
- `tests/integration/pool_a_to_b_migration_test.go` — two-container
  rehearsal of this flow
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-RESEARCH.md`
  §Pattern 5 lines 859-868 — the 9-step runbook source
