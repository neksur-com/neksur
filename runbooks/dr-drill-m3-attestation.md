# M3 DR Drill Attestation — PENDING

**Status:** PENDING — drill not yet executed.
**Contract:** **D-0.5.16** (LOCKED) — RPO 5 min / RTO 1 h.
**Scope:** First scheduled execution of the DR restore path against
Phase 0.5 sandbox Pool A.
**Validation tests:** `tests/dr/restore_pit.sh --assert-rto-under 3600`
wrapped by `scripts/ci/dr-drill.sh` (the cron driver; one-shot operator-
driven for the M3 drill).
**Closes:** ROADMAP §Phase 0.5 success criterion 6 ("quarterly DR drill
executes against Pool A and verifies RPO ≤5min / RTO ≤1h"); referenced
by Plan 07's pen-test sign-off gate.

> **Operator instructions.** This file is a TEMPLATE. Fill in the
> [TBD] markers as the drill proceeds, then commit the completed file
> with `docs(00.5-06-dr-drill): M3 attestation`. Once committed and
> reviewed, this drill is closed. Until then the status MUST remain
> `PENDING`.

---

## First-drill target

**First drill target date:** M3 of Phase 0.5 (target 2026-08-14 OR
first paying tenant onboarding, whichever is earlier).
**Drill scheduling reference:** `.planning/phases/00.5-saas-pilot-infrastructure/00.5-VALIDATION.md`
Manual-Only Verifications row "First DR drill (M3 trigger)".

---

## Operator(s)

**Drill operator(s):** [TBD — operator email + WorkOS internal_admin ID]
**Drill reviewer:** [TBD — second operator who independently verifies the result]
**Date executed (UTC):** [TBD — YYYY-MM-DDTHH:MM:SSZ]
**Sandbox AWS profile:** [TBD — must NOT be the production profile]

---

## Pre-drill state

| Field | Value |
|-------|-------|
| Pool A primary endpoint (sandbox) | [TBD — `terraform output pool_a_primary_private_dns`] |
| Pool A pgBackRest stanza | `neksur` |
| Pool A pgBackRest backup label used | [TBD — e.g., `20260814-130000F`] |
| Sandbox AWS account | [TBD — must NOT be 964775859511 production pilot] |
| Test tenants provisioned (count) | [TBD — must be ≥ 2; one to corrupt, one as a control] |
| Tenant chosen for corruption | [TBD — tenant UUID] |
| Tenant chosen as control | [TBD — tenant UUID] |

---

## Drill execution

### Step 1 — record T0

| Field | Value |
|-------|-------|
| T0 (wall-clock start) | [TBD — `date -u +%s`] |
| T0 (ISO 8601 UTC) | [TBD] |
| Corruption operation chosen | [TBD — e.g., `TRUNCATE tenant_<uuid>.audit_log` OR `DELETE FROM tenant_<uuid>.policies`] |
| Corruption SQL executed | [TBD — exact SQL run, NOT redacted] |
| Corruption timestamp (T0_corruption, UTC) | [TBD] |

### Step 2 — invoke restore_pit.sh

| Field | Value |
|-------|-------|
| Restore target_time (UTC) | [TBD — must be ~5 min before T0_corruption per RPO 5min] |
| Restore command | `bash tests/dr/restore_pit.sh --assert-rto-under 3600 --target-time '<target_time>' --stanza neksur --yes` |
| Restored host identifier | [TBD — distinct from production e.g. `neksur-pool-a-pitr-test`] |

### Step 3 — record T1

| Field | Value |
|-------|-------|
| T1 (wall-clock end of restore + pg_isready) | [TBD] |
| T1 (ISO 8601 UTC) | [TBD] |

## Wall-Clock

| Field | Value |
|-------|-------|
| Wall-clock (T1 - T0, seconds) | [TBD — target < 3600] |
| Wall-clock (formatted) | [TBD — Nmin Ks] |
| CloudWatch metric posted? | [TBD — yes/no; the cron driver `scripts/ci/dr-drill.sh` auto-posts to `Neksur/dr_drill_wallclock_seconds`] |

---

## Smoke tests after restore

The restored cluster MUST pass these checks before the drill is considered
complete. Operator records PASS/FAIL/SKIP per row:

- [ ] **WorkOS auth handshake** against the restored cluster — connect
      via the application with a real WorkOS session and confirm
      `public.tenants` row lookup returns the expected tenant.
- [ ] **Tenant query** (random tenant) — `SELECT count(*) FROM tenant_<uuid>.audit_log`
      returns the expected count within the RPO window.
- [ ] **Corruption is undone** — `SELECT count(*) FROM tenant_<corrupt_uuid>.audit_log`
      returns the pre-corruption row count (the truncate IS undone).
- [ ] **audit_log INSERT-only contract preserved** — attempt
      `UPDATE tenant_<uuid>.audit_log SET ...` as `tenant_<uuid>_role`;
      assert permission denied (T-0.5-audit-tamper still holds post-restore).
- [ ] **3-layer isolation tests pass against restored cluster** —
      `go test -tags integration -run TestTenantIsolation ./tests/integration/`
      against the restored cluster's DSN.

---

## RPO verification

| Field | Value |
|-------|-------|
| Restore target_time (UTC) | [TBD — same as Step 2] |
| Corruption time (UTC) | [TBD — same as Step 1] |
| Time delta (seconds) | [TBD — target ≤ 300 for RPO 5min] |
| RPO verdict | [TBD — PASS or FAIL] |

- RPO verdict: [TBD — PASS or FAIL]

---

## RTO verdict

| Field | Value |
|-------|-------|
| RTO threshold | 3600s (D-0.5.16 RTO 1h) |
| RTO observed | [TBD — wall-clock from Step 3] |
| RTO verdict | [TBD — PASS or FAIL] |

- RTO verdict: [TBD — PASS or FAIL]

---

## Findings / runbook updates needed

[TBD — operator describes any deviation from the runbook. Examples:]

> "Step 4 of restore-pitr.md says pg_isready polls every 2s; on the
> sandbox this took 90s for the first poll to succeed because the
> instance was cold-booting Patroni. Should the runbook document a
> warm-up period? Filed as follow-up ticket NEKSUR-DR-001."
>
> "Step 8 of dr-drill.md says 'tear down via aws rds delete-db-instance'
> but the V3 path uses Postgres-on-EC2; the actual teardown is
> `terraform destroy -target=` the sandbox module. Updated dr-drill.md
> §M3 First Drill step 8 to clarify."

---

## Sign-off

**Operator signature:**
- Operator: [TBD — operator email]  Date: [TBD]

**Reviewer signature (independent verification):**
- Reviewer: [TBD — second operator email]  Date: [TBD]

## Observer:

[TBD — operator name + WorkOS internal_admin org membership confirmed]

## Reviewer:

[TBD — independent reviewer name + WorkOS internal_admin org membership confirmed]

---

## Teardown attestation

| Field | Value |
|-------|-------|
| Sandbox restored instance terminated | [TBD — yes/no + termination timestamp] |
| Sandbox PITR-test resources destroyed | [TBD — `terraform destroy -target=` confirmed] |
| Stranded-cost check | [TBD — `aws ec2 describe-instances --filters 'Name=tag:Name,Values=neksur-pool-a-pitr-test*'` returns empty] |

---

## Cross-references

- `runbooks/restore-pitr.md` — the operator-runnable PITR restore
  procedure (§3 + §4 invoked during this drill).
- `runbooks/dr-drill.md` §M3 First Drill — the drill checklist.
- `scripts/ci/dr-drill.sh` — the cron driver that automates the
  recurring quarterly drill (post-M3 cadence per D-0.5.16).
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-VALIDATION.md`
  Manual-Only Verifications — this attestation closes the row
  "First DR drill (M3 trigger)".
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-07-PLAN.md`
  (when written) — Plan 07's pen-test sign-off gate references this
  attestation as upstream evidence.
