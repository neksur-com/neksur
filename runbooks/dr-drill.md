# Runbook: Monthly DR Drill

**Owner:** Phase 0 SRE / on-call (rotating monthly)
**Scope:** Neksur Postgres + AGE metadata store + pgBackRest repo (Plan 00-04)
**Contract:** **D-OQ.04** (LOCKED, supersedes ADR-001 D-001.13) —
**RTO 1 hour / RPO 15 minutes** unified across all metadata stores.
**Validation tests:** `tests/dr/run_monthly_restore_drill.sh` wrapped by
Go test `TestMonthlyRestoreDrill` in `tests/dr/dr_targets_test.go`
(`//go:build dr`); RPO arm validated separately by `TestRPO_ChaosRestore`
(via `tests/dr/chaos_restore.sh`); RTO arm by `TestRTO_RestorePIT` (via
`tests/dr/restore_pit.sh`).

> **Note on the contract.** ADR-001 §8.1 originally specified RTO 30min /
> RPO 1h. The operative contract is **D-OQ.04: RTO 1h / RPO 15min**. If
> you read the original ADR-001 and see different numbers, **D-OQ.04 wins**.

---

## 1. Why this drill exists

REQ-NFR-dr (in `.planning/REQUIREMENTS.md`) requires the restore path
to be exercised **monthly** — not because we expect a disaster monthly,
but because the gap between "we have backups" and "we can actually
restore from them" is where DR contracts die.

The drill validates four things at once:

1. **Restore mechanics work.** `pgbackrest restore` succeeds against
   a real recent backup.
2. **RTO contract holds.** The drill measures wall-clock from
   `pg_ctl stop` to `pg_isready` and asserts < 3600s.
3. **Schema integrity post-restore.** `migrations/check.sql` confirms
   the D-003.06 schema (19 vlabels + 24 elabels) survived.
4. **Runbook accuracy.** A human (the on-call) executes the runbook;
   any deviation is captured in the drill report and fed back into
   `runbooks/restore-pitr.md` and this file.

## M3 First Drill (Phase 0.5 — D-0.5.16)

This M3 First Drill subsection documents the FIRST scheduled execution
of this drill against Phase 0.5 Pool A — gating before any paying
customer. Per D-0.5.16 the team executes the M3 First Drill in M3 of
Phase 0.5 (target 2026-08-14 OR first paying tenant onboarding,
whichever earlier).

### Why M3

D-0.5.16 forces the team to actually exercise the restore path before
any paying customer is on the system. Without an M3 drill, REQ-saas-observability's
"quarterly DR drill executes against Pool A and verifies RPO ≤5min /
RTO ≤1h" criterion stays "Pending" and Plan 07's pen-test sign-off
gate cannot complete.

### M3 first-drill checklist

1. **Provision a SANDBOX Pool A** (not production!) via
   `terraform apply` against the sandbox AWS account.
2. **Provision two test tenants** via `./scripts/provision-tenant.sh`
   (Plan 04). Wait at least 15 minutes after provisioning so PITR has
   WAL coverage for the new tenants.
3. **Deliberately corrupt one of the test tenant schemas** —
   `psql -d $POOL_A_DSN -c "TRUNCATE tenant_<uuid_underscored>.audit_log"`.
4. **Trigger PITR restore** for the timestamp ~5 min BEFORE the
   corruption per `runbooks/restore-pitr.md` §3 (use the sandbox
   restored instance — NOT promote into the live cluster).
5. **Smoke tests against the restored cluster:**
   - Connect via psql, run `SELECT count(*) FROM tenant_<corrupted_uuid>.audit_log`
     — assert non-empty (the truncate is undone).
   - 3-layer isolation tests still pass against the restored cluster
     (re-run `tests/integration/tenant_isolation_test.go` against the
     sandbox).
   - WorkOS auth handshake still works against the restored cluster.
6. **Record wall-clock** in the drill report
   (`runbooks/dr-drill-m3-attestation.md`). Assert
   `wall_clock_seconds < 3600` (RTO 1h per D-0.5.16).
7. **Verify RPO independently:** confirm restored data is from ≤5 min
   before corruption (D-0.5.16 RPO target).
8. **Tear down the test restored instance** to avoid stranded cost
   (`aws rds delete-db-instance --db-instance-identifier
   neksur-pool-a-pitr-test --skip-final-snapshot`). On Postgres-on-EC2
   V3 the equivalent is `terraform destroy -target=` the sandbox
   instance OR manually terminating the EC2 instances.
9. **Document any rough edges** — append back to
   `runbooks/dr-drill.md` and `runbooks/restore-pitr.md` so the next
   drill operator has a smoother path.

### Success criteria

- **RTO**: full Pool A restore from yesterday's PITR snapshot
  completes in < 1h (3600s) per D-0.5.16.
- **RPO**: data loss ≤ 5 min (300s) from the corruption timestamp.
- **Smoke**: all four post-restore checks PASS.
- **Wall-clock recorded** to CloudWatch custom metric
  `Neksur/dr_drill_wallclock_seconds` (via the automated cron driver
  `scripts/ci/dr-drill.sh`).

### Cadence (post-M3)

After the M3 drill, drill cadence shifts to **quarterly** per
D-0.5.16. The `scripts/ci/dr-drill.sh` cron driver runs every quarter
(currently scheduled in `.github/workflows/dr-drill-quarterly.yml`;
Phase 1 hardening — Phase 0.5 ships the script + the runbook + the
attestation template only).

### Attestation file

The M3 drill outcome is captured in
`runbooks/dr-drill-m3-attestation.md` (a template until the actual
drill runs in M3). The attestation file is REFERENCED by Plan 07's
pen-test sign-off gate — Plan 07 cannot complete without an attestation
on file (PASS or documented FAIL with follow-up ticket).



---

## 2. Who runs it, when, where

| Field | Value |
|-------|-------|
| **Owner** | On-call SRE for the calendar month |
| **Cadence** | Once per month, on the 1st (or first weekday after) |
| **Time-box** | 90 minutes (60 min RTO budget + 30 min for setup, validation, report) |
| **Environment** | Isolated restore host (NOT the production primary) — typically a transient VM provisioned for the drill |
| **Tooling** | `tests/dr/run_monthly_restore_drill.sh` + `tests/dr/restore_pit.sh` (called transitively) |
| **Output** | `runbooks/drill-reports/drill-report-YYYYMM.md` |

Check `runbooks/drill-reports/` before starting — if a report for the
current YYYYMM already exists, this month's drill was already run by
another SRE; coordinate before re-running.

---

## 3. Pre-flight

| Check | How | Failure mode if skipped |
|-------|-----|--------------------------|
| Restore host provisioned | Cloud / hypervisor checklist | Drill blocked — file procurement ticket |
| pgBackRest installed on restore host | `pgbackrest --version ≥ 2.50` | Drill aborts at restore step |
| Repo credentials available on restore host | `pgbackrest --stanza=neksur info` exits 0 | Drill aborts at restore step |
| Postgres + AGE installed | `psql --version`, `apt list --installed | grep postgresql-16-age` | Drill aborts at start step |
| Repo root has go.mod | `ls /path/to/neksur-core/go.mod` | Wrapper script can't resolve repo root |
| Disk free ≥ 200 GB | `df -h <pg-data>` | Restore aborts mid-flight |

If any pre-flight fails, **do not proceed** — file an incident
documenting the gap and re-attempt next business day.

---

## 4. Invoke the drill

```bash
cd /path/to/neksur-core
bash tests/dr/run_monthly_restore_drill.sh \
    --target-time '24h ago' \
    --rto-threshold 3600 \
    --report-dir runbooks/drill-reports \
    --check-sql migrations/check.sql \
    --psql 'psql postgres://postgres@localhost:5432/postgres' \
    --pg-data /var/lib/postgresql/16/main \
    --pg-bin /usr/lib/postgresql/16/bin \
    --stanza neksur \
    --yes   # bypasses the restore_pit.sh wipe prompt — REQUIRED for the drill
```

The script:

1. Computes target-time as 24 hours ago (overridable via `--target-time`).
2. Invokes `tests/dr/restore_pit.sh --assert-rto-under 3600 --target-time
   '24h ago' --yes` and captures success/failure + elapsed wall-clock.
3. Runs `migrations/check.sql` and inspects for `FAIL` rows.
4. Writes `runbooks/drill-reports/drill-report-YYYYMM.md` with the
   full result regardless of pass/fail (the report exists even on
   failure — it captures the failure mode for postmortem).

---

## 5. Success criteria

The drill passes when **both** of:

- The drill report `Outcome:` line reads `PASS` (not `PASS-WITH-CAVEAT`,
  not `FAIL`).
- `Status: **PASS**` rows for both Restore and Schema integrity
  sections.

The schema-integrity row must show:

- `Observed vlabels`: **19** (per D-003.06)
- `Observed elabels`: **24** (per D-003.06)

Anything else — including `PASS-WITH-CAVEAT` (which means the schema
check was skipped) — requires investigation before the drill is
considered complete.

---

## 6. Failure handling

If the drill report shows `Outcome: FAIL` (either restore failed or
schema check failed):

| Step | Action |
|------|--------|
| 1 | **File an incident.** Severity: P1 if restore mechanics are broken (no recoverable backups); P2 if restore worked but schema check failed (data is recoverable but schema diverged from contract). |
| 2 | **Notify Phase 0 lead** (and CTO if P1) within 4 hours. The DR contract is broken — every minute of unfixed DR is potential customer-data exposure. |
| 3 | **Add a regression test** to `tests/dr/` covering the failure mode. The chaos / DR test surface is the safety net; if a real failure was missed by the existing tests, the gap is the bug. |
| 4 | **Re-run the drill** after fix. The drill is not closed until a green report exists for the YYYYMM. |
| 5 | **Update this runbook** if the failure exposed a procedural gap (e.g., a pre-flight check that should have caught the failure). |

---

## 7. Drill report archive

Reports live under `runbooks/drill-reports/drill-report-YYYYMM.md`.
This is the audit trail used by quarterly compliance reviews and by
the M9 PMF gate evaluation (PROJECT.md §6).

The report's `## Deviations from runbook` section is the lifeblood of
this runbook's accuracy. **If the drill required any step not
described in this runbook, capture it there.** Quarterly, the on-call
team reviews accumulated deviations and folds them back into this
runbook.

---

## 8. Cross-references

- `tests/dr/run_monthly_restore_drill.sh` — the canonical drill driver
- `tests/dr/restore_pit.sh` — wrapped by the drill driver
  (the actual restore happens here)
- `tests/dr/chaos_restore.sh` — RPO arm of the contract (separate
  drill, not run monthly — exercised by nightly CI via
  `.github/workflows/load-chaos-restore.yml`)
- `tests/dr/dr_targets_test.go` — Go wrappers
  (`TestRTO_RestorePIT`, `TestRPO_ChaosRestore`,
  `TestMonthlyRestoreDrill`); run with:
  ```bash
  DR_LIVE=1 go test -tags dr -timeout 90m ./tests/dr/...
  ```
- `runbooks/restore-pitr.md` — operator runbook for ad-hoc PITR
  (e.g., customer asks for state at 14:00 yesterday)
- `infra/pgbackrest/pgbackrest.conf.template` — stanza config
- `infra/pgbackrest/sizing.md` — A7 evidence (4GB queue ≥ 15min WAL)
- `migrations/check.sql` — D-003.06 schema-integrity check
- `.planning/REQUIREMENTS.md` — REQ-NFR-dr (the contract being validated)
- `.planning/PROJECT.md` D-OQ.04 — unified DR contract (RTO 1h /
  RPO 15min), supersedes ADR-001 D-001.13

### Manual verification cross-reference

This drill IS the implementation of the "Monthly DR drill" row in
`.planning/phases/00-metadata-graph-foundation/00-VALIDATION.md`
§Manual-Only Verifications. The wrapping `TestMonthlyRestoreDrill`
(behind `//go:build dr` and gated on `DR_LIVE=1`) is the automated
side; the human-led drill captured by this runbook is the
human-readable side. Both must remain in sync — if either changes,
update the other.
