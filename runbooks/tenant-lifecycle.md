# Runbook: Tenant Lifecycle (Suspend / Wind-Down / Delete)

**Owner:** Founder / on-call SRE (Phase 0.5); Customer Success (Phase 1+)
**Scope:** Operator runbook for the **D-0.5.20** state machine —
transitions between `active`, `suspended`, `wind_down`, `deleted`
across the SaaS tenant lifecycle.
**Contract:** **D-0.5.20** (LOCKED) — four-state machine; gateway
returns 503 on writes in `suspended` and `wind_down` states; `deleted`
is irreversible; 30-day RDS backup retention provides post-delete
recovery window.
**Validation tests:** `tests/integration/tenant_lifecycle_test.go`
(TestSuspendThenReadOnly + TestWindDownPreservesData + TestDeleteIrreversible +
TestInvalidStateTransition) + `internal/tenant/lifecycle_test.go`
(TestDeleteRequiresConfirm).
**Closes:** REQ-saas-tenancy-pool-a (state machine) + REQ-saas-tenancy-pool-b
(applies symmetrically to Pool B Enterprise tenants).

> **Note on the contract.** All transitions are operator-initiated via
> `./scripts/tenant-<action>.sh <uuid>` (or `./neksur-cli tenant <action>`).
> `Suspend` and `WindDown` are reversible at the database level (Phase 1+
> ships `tenant-resume.sh`); `Delete` is irreversible — only RDS PITR
> within 30 days of delete can recover the schema.

---

## State machine (D-0.5.20)

```text
                   ┌───────────┐
                   │  active   │ (default; reads + writes go through)
                   └─────┬─────┘
                Suspend()│      │WindDown()
                         ▼      ▼
                   ┌───────────────┐
                   │  suspended    │ (reads OK; 503 on writes — D-0.5.20)
                   └───────┬───────┘
                  WindDown()│
                            ▼
                   ┌───────────────┐
                   │  wind_down    │ (30-day post-cancellation read-only)
                   └───────┬───────┘
                    Delete()│ (manual: scripts/tenant-delete.sh --yes
                            ▼      OR Phase 1+ cron at day 30)
                   ┌───────────────┐
                   │   deleted     │ (irreversible; schema dropped)
                   └───────────────┘
```

**Allowed transitions** (enforced by `internal/tenant/lifecycle.go`):

| From | To | Method | Allowed predecessor states |
|------|----|--------|----------------------------|
| `active` | `suspended` | `Suspend()` | `['active']` only |
| `active` or `suspended` | `wind_down` | `WindDown()` | `['active', 'suspended']` |
| `active` or `suspended` or `wind_down` | `deleted` | `Delete()` | `['active', 'suspended', 'wind_down']` |

Any transition not in this table returns **`ErrTenantNotFound`** with NO
DB mutation (verified by `TestInvalidStateTransition`).

**Reverse transitions are NOT shipped in Phase 0.5.** `suspended → active`
and `wind_down → active` are operator-driven manual SQL until Phase 1+
ships `tenant-resume.sh`. The manual procedure:

```sql
-- Manual resume (Phase 0.5 only — Phase 1+ will provide a script).
BEGIN;
UPDATE public.tenants
   SET lifecycle_state = 'active', updated_at = now()
 WHERE id = '<tenant-uuid>' AND lifecycle_state IN ('suspended', 'wind_down');
INSERT INTO public.system_audit_log
    (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
VALUES
    (now(), 'manual-resume@neksur.com', '<tenant-uuid>', 'tenant.resumed',
     '{"reverse_transition":"manual"}'::jsonb);
COMMIT;
```

---

## 1. Suspend — non-paying after grace period, dispute, or security incident

**When to use:**
- Customer past 30-day non-payment grace period (Phase 1+ billing
  automation will trigger this; Phase 0.5 = manual).
- Active dispute; need to halt writes while a human resolves.
- Security incident; immediate halt to writes while we investigate.

**Command:**

```bash
cd /Users/evgeny/neksur-core
./scripts/tenant-suspend.sh <tenant-uuid>
```

**Behavior:**
- `public.tenants.lifecycle_state` → `'suspended'` (atomic UPDATE
  with `WHERE lifecycle_state = 'active'`).
- `public.system_audit_log` row with `event_type='tenant.suspended'`.
- **Gateway behavior:** the V0044 `tenant_by_workos_org` lookup still
  routes the tenant (`'active'` OR `'suspended'`), so reads continue.
  The gateway layer (Plan 03 + Phase 1 commit handler) checks the
  state per-request and returns HTTP 503 on POST/PUT/DELETE/PATCH —
  reads (GET) are unaffected.

**Idempotency:** Re-running on a tenant already in `'suspended'`,
`'wind_down'`, or `'deleted'` prints a WARN to stderr but exits 0 —
safe to call from a cron or retry loop.

**Customer notification template:**

> Subject: Neksur — Your account has been suspended
>
> Hello {customer-contact},
>
> Your Neksur account ({workos-org-id}) was suspended on {date} UTC.
> While suspended:
>
> - **Read access:** continues to work — you can still query your
>   metadata graph and download audit_log / policies.
> - **Writes:** rejected with HTTP 503 until the account is resumed.
>
> Reason: {non-payment / dispute / security / other}
>
> To resume: {pay outstanding invoice / contact billing / resolve
> security issue}. Once resolved, please reply to this email — our
> team will manually resume your account.
>
> — Neksur Ops

---

## 2. Wind-down — customer cancellation, 30-day countdown

**When to use:**
- Customer requests cancellation (Phase 1+ self-serve will trigger this;
  Phase 0.5 = manual via email/Slack).
- Suspended tenant that has remained suspended >30 days with no path to
  resume (transition them to wind_down so the 30-day delete clock starts).

**Command:**

```bash
./scripts/tenant-windown.sh <tenant-uuid>
```

**Behavior:**
- `public.tenants.lifecycle_state` → `'wind_down'`.
- `public.system_audit_log` row with `event_type='tenant.wind_down'`.
- 30-day post-cancellation read-only window starts now; customer can
  still download audit_log + policies. Writes rejected with 503.
- After 30 days: **Phase 0.5 = manual delete** via Section 3; Phase 1+
  cron will auto-transition.

**Customer notification template:**

> Subject: Neksur — Wind-down started for {workos-org-id}
>
> Hello {customer-contact},
>
> Your cancellation request has been processed. Your Neksur account
> entered wind-down on {date} UTC.
>
> **What this means:**
>
> - You have **30 days** of read-only access (until {date + 30d} UTC).
>   Use this window to download your audit_log, policies, and any other
>   data you need to retain.
> - Writes are rejected with HTTP 503.
> - On {date + 30d} UTC, your tenant schema will be permanently deleted.
>   30 days of RDS backups will be retained afterward (recovery window
>   if you change your mind within that period).
>
> If you change your mind before the 30-day window expires, reply to
> this email and we'll resume your account.
>
> — Neksur Ops

---

## 3. Delete — IRREVERSIBLE final transition

**When to use:**
- A `wind_down` tenant has reached day 30 (Phase 0.5 = manual until
  Phase 1+ cron ships).
- Operator approves an early-delete request (e.g., GDPR right-to-erasure
  ahead of the 30-day window).

**Command:**

```bash
./scripts/tenant-delete.sh <tenant-uuid>           # interactive prompt
./scripts/tenant-delete.sh <tenant-uuid> --yes      # unattended (cron / CI)
```

**Behavior** (atomic, FAIL-STOP on the DB side):

1. `public.tenants.lifecycle_state` → `'deleted'`.
2. `SELECT drop_graph('tenant_<uuid_underscored>', true)` —
   drops the AGE-aware tenant schema with cascade.
3. `public.system_audit_log` row with `event_type='tenant.deleted'`.
4. Shell-out `terraform destroy -target=module.customer_peering[<uuid>]`
   — destroys VPC peering connection.
5. 30-day RDS backup retention (pgBackRest, Plan 01) covers recovery.

**WARNING:** `--yes` is **MANDATORY**. Without `--yes`, the script
prompts the operator to type literally `DELETE <uuid>` to confirm. Any
other input aborts with exit 1 — the CLI itself ALSO requires `--yes`
as defence in depth (T-0.5-accidental-delete).

**Partial-state recovery (T-0.5-partial-delete-state):** If the DB delete
succeeds but `terraform destroy` fails:
- The DB row is `'deleted'` and the schema is gone.
- The peering connection remains in AWS but is unreachable from a
  non-existent tenant.
- Operator runs the destroy manually:
  ```bash
  cd /Users/evgeny/neksur-infra/environments/phase0-pilot
  terraform destroy -target='module.customer_peering["<tenant-uuid>"]' -auto-approve
  ```
- The audit_log row already records the DB-side outcome; the partial
  state is recoverable via this manual step.

**Customer notification template (final, post-delete):**

> Subject: Neksur — Account deleted on {date}
>
> Hello {customer-contact},
>
> Your Neksur account ({workos-org-id}) was permanently deleted on
> {date} UTC, per our 30-day wind-down policy / your early-delete request.
>
> - All tenant data has been removed from the active database.
> - **Backup retention:** Encrypted RDS backups are retained for 30
>   days (until {date + 30d} UTC). If you need recovery within that
>   window, contact ops@neksur.com immediately.
> - VPC peering between Neksur's VPC and your VPC has been destroyed
>   on our side; you can remove the corresponding accepter resources
>   on your side at your convenience.
>
> Thank you for being a design partner.
>
> — Neksur Ops

---

## 4. Backup recovery (if deleted in error)

If a `deleted` was a mistake AND the operator catches it within 30 days:

1. **HALT** — do NOT re-provision a new tenant with the same UUID; that
   would create a public.tenants row in `'active'` state but the schema
   data from the original is locked in backup.
2. Follow `runbooks/restore-pitr.md` **§8 Pool A specifics** (per-tenant
   restore procedure) for a Pool A tenant, OR **§9 Pool B specifics**
   for a Pool B Enterprise tenant.
3. Specifically:
   - `pgbackrest --stanza=neksur restore --target-time '<delete-timestamp-minus-5min>'`
     to a SANDBOX host.
   - `pg_dump --schema=tenant_<uuid_underscored>` from the restored sandbox.
   - `pg_restore` into the LIVE Pool A under a DIFFERENT schema name
     (e.g., `tenant_<uuid>_recovered`).
   - INSERT a new public.tenants row with a fresh UUID and use ALTER
     SCHEMA to rename if the customer wants the original UUID back
     (rare; usually a fresh UUID is cleaner).
4. The 30-day backup retention is enforced by pgBackRest config (see
   `modules/rds-pool-a/main.tf` `var.backup_retention_archive_days = 35`).
   Past day 30 (or 35), recovery is not possible.

---

## 5. Cross-references

- `internal/tenant/lifecycle.go` — Go implementation (Repo.Suspend /
  Repo.WindDown / Repo.Delete + transitionLifecycle).
- `cmd/neksur-cli/tenant_{suspend,windown,delete}.go` — CLI surface.
- `scripts/tenant-{suspend,windown,delete}.sh` — operator wrappers
  (this runbook's commands).
- `tests/integration/tenant_lifecycle_test.go` — integration tests for
  every transition in §State Machine.
- `runbooks/saas-onboarding.md` — upstream: how tenants come INTO
  `'active'` state.
- `runbooks/restore-pitr.md` §8 + §9 — downstream: backup recovery
  procedures (called from §4 above).
- `runbooks/secret-rotation.md` — rotating the `DATABASE_URL` admin
  credential the scripts depend on.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-CONTEXT.md` —
  D-0.5.20 decision rationale.

---

## 6. Common failures + fixes

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `WARN tenant ... not in 'active' state` exit 0 | Tenant already in target state (idempotent) | No action — script returns 0 |
| `ERROR: target row not in expected state` | Hard error after a valid call (race) | Re-read state via `psql -c "SELECT lifecycle_state FROM public.tenants WHERE id='<uuid>'"`; pick the right transition |
| `confirmation phrase did not match` exit 1 (delete only) | Operator typed wrong UUID at prompt | Re-run; type exactly `DELETE <uuid>` (case-sensitive, single space) |
| `terraform destroy: error … customer_peering does not exist` | Peering already destroyed OR was never created | Pass `--skip-terraform` to the CLI (or set `SKIP_TERRAFORM=1` in the script env) |
| `repo.Delete: ... LOAD age` error | Postgres extension `age` not loaded on the admin connection | The Repo.Delete method runs `LOAD 'age'` inside a transaction; if this errors, check Pool A's `shared_preload_libraries` includes `age` (Plan 01 user-data) |
| `audit_log INSERT failed` | `public.system_audit_log` table missing | Plan 02 V0043 must be applied; re-run `migrate.ApplyPublic` |
