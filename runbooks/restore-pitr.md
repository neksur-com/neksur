# Runbook: pgBackRest Point-in-Time Restore (PITR)

**Owner:** Phase 0 SRE / on-call
**Scope:** Neksur Postgres + AGE primary metadata store (Plan 00-04)
**Contract:** **D-OQ.04** (LOCKED, supersedes ADR-001 D-001.13) —
**RTO 1 hour / RPO 15 minutes** unified across all metadata stores.
**Validation tests:** `tests/dr/restore_pit.sh` wrapped by Go test
`TestRTO_RestorePIT` in `tests/dr/dr_targets_test.go` (`//go:build dr`).

> **Note on the contract.** ADR-001 §8.1 originally specified RTO 30min /
> RPO 1h. The operative contract is **D-OQ.04: RTO 1h / RPO 15min** —
> the trade was tighter RPO at the cost of looser RTO. If you read the
> original ADR-001 and see different numbers, **D-OQ.04 wins**.

---

## 1. When to use this runbook

Trigger this runbook when ANY of the following occur:

- **Customer asks** for the metadata graph state at a specific past
  time (e.g., "what did our policy graph look like at 14:00 yesterday
  before the bad change went in?").
- **Accidental destructive action** (e.g., `DROP GRAPH`, mass-delete
  query) on the production primary that needs a partial / full rewind.
- **Patroni HA failover failed** to recover a healthy cluster (see
  `runbooks/failover.md` first; this runbook is the second-line
  remediation when failover cannot recover the data).
- **Data corruption** detected by `migrations/check.sql` or by a
  customer report — needs restore to the last known-good point.

This runbook does **NOT** cover the routine monthly drill — see
`runbooks/dr-drill.md` for that.

---

## 2. Pre-flight checklist

Before invoking the restore, confirm:

| Check | How | Why |
|-------|-----|-----|
| pgBackRest repo intact | `pgbackrest --stanza=neksur info` shows recent fulls + diffs | If repo is broken, restore is impossible |
| Repo accessible | `pgbackrest --stanza=neksur check` exits 0 | Network / credentials live |
| Target time identified | ISO8601 timestamp from incident report | "now" if you want all available WAL replayed |
| Restore-host disk free ≥ source data size | `df -h /var/lib/postgresql/16/main` | Pre-flight is faster than running out mid-restore |
| Target host has pgBackRest installed | `pgbackrest --version` shows ≥2.50 | Required tool |
| Restore-host stanza config matches prod | `diff /etc/pgbackrest/pgbackrest.conf <(rendered template)` | Template at `infra/pgbackrest/pgbackrest.conf.template`; mismatch → wrong repo / wrong retention math |
| You have time (RTO budget) | D-OQ.04 = 1h | If you're already past 30 min into an incident, escalate before continuing |

**Preferred topology:** restore to a **fresh isolated host**, not the
running primary. The Patroni HA cluster (see `runbooks/failover.md`)
should remain serving reads; the restored host is promoted into the
cluster only after validation (§5).

---

## 3. Invoke the restore

Use the canonical script — do NOT invoke `pgbackrest restore` by hand.
The script enforces the safety wipe-confirmation, records timing for
the post-mortem, and asserts the RTO contract:

```bash
cd /path/to/neksur-core
bash tests/dr/restore_pit.sh \
    --target-time '2026-05-12 14:00:00+00' \
    --assert-rto-under 3600 \
    --stanza neksur \
    --pg-data /var/lib/postgresql/16/main \
    --pg-bin /usr/lib/postgresql/16/bin
```

For unattended (CI / Go-test-driven) invocation, add `--yes` to bypass
the wipe-confirmation prompt. Operators running this manually should
**leave `--yes` off** — the prompt is the last line of defence against
typos in `--pg-data`.

### What each output line means

```
==> pgBackRest PITR drill
    stanza:       neksur
    repo:         /var/lib/pgbackrest
    pg-data:      /var/lib/postgresql/16/main
    target-time:  2026-05-12 14:00:00+00
    RTO threshold: 3600s
```

The header confirms the parameters. **READ THESE VALUES** — a typo in
`--pg-data` here is a production-data-loss event.

```
==> T0=1715520000 (2026-05-12T14:00:00Z)
==> Stopping Postgres (pg_ctl stop -m fast)...
==> Wiping /var/lib/postgresql/16/main...
==> Restoring from pgBackRest --stanza=neksur --type=time --target='2026-05-12 14:00:00+00'...
```

T0 is the wall-clock start. The wipe is the irreversible step — once
this line prints, the only way back is the restore itself succeeding.

```
==> Starting Postgres (pg_ctl start -w)...
==> Polling pg_isready (every 2s, up to 3600s total)...
==> T1=1715520420 ELAPSED=420s (threshold=3600s)
==> RTO PASS — restored to 2026-05-12 14:00:00+00 in 420s (under 3600s threshold)
```

T1 - T0 = ELAPSED. PASS means under the threshold (D-OQ.04 1h).
A FAIL line aborts with exit 1 — investigate (was the threshold too
tight? was the WAL replay slow? is the repo on the wrong storage tier?).

---

## 4. Post-restore validation

Restore success is necessary but not sufficient — the restored database
must hold the correct schema and pass a smoke query.

### 4.a Schema-integrity check

Run `migrations/check.sql` against the restored host. The contract is
**19 vlabels + 24 elabels** per the **D-003.06 amendment** to ADR-001.

```bash
psql "postgres://postgres@localhost:5432/postgres" -f migrations/check.sql
```

Expected output (excerpt):

```
 status | check_name      | actual | expected
--------+-----------------+--------+----------
 PASS   | vlabel_count    |     19 |       19
 PASS   | elabel_count    |     24 |       24
 PASS   | extensions_present |   3 |        3
 ...
```

**ANY** `FAIL` row means the restored schema diverges from the
expected contract. Stop here and investigate before promotion. A
common failure mode: the target_time landed before a recent
migration was applied — the restored schema is intentionally
"behind" production. If that's expected, document it; otherwise
re-target the restore.

### 4.b Smoke Cypher query

Confirm AGE works post-restore (the same load-bearing check from
`runbooks/failover.md` §3.c):

```bash
psql "postgres://neksur_app@localhost:5432/neksur" -c \
    "SELECT * FROM cypher('neksur', \$\$ MATCH (n) RETURN count(n) \$\$) AS (c agtype);"
```

If this returns a count without erroring, AGE is healthy on the
restored host. If it errors with `function cypher does not exist`,
this is the **A1 failure mode** — see `runbooks/failover.md` §4.

---

## 5. Promotion

If the restored host is meant to **replace** the current primary
(e.g., the original primary suffered unrecoverable corruption), join
it to the Patroni cluster as a replica first, then promote it to
leader via `patronictl switchover`. The procedure is identical to
the post-failover join described in `runbooks/failover.md` §3:

```bash
# 1. Add the restored host to the Patroni cluster (join as replica).
#    Bootstrap via pg_basebackup from the current leader (replaces
#    the restored data dir with a copy of the leader's — yes, this
#    discards the restored state; promotion is via switchover, not
#    via direct restore-then-promote).
patronictl -c /etc/patroni/patroni.yml reinit neksur-pg <restored-host>

# 2. Confirm replica health.
patronictl -c /etc/patroni/patroni.yml list

# 3. Switch leadership to the restored host (only if the restored
#    state is what you want propagated):
patronictl -c /etc/patroni/patroni.yml switchover \
    --master <current_leader> \
    --candidate <restored_host>
```

If the restored host is NOT being promoted (it's just an isolated
investigation tool — e.g., for a customer-state lookup), skip §5
entirely and tear down the host after the analysis is complete.

---

## 6. Communication template (status page)

Adjust to fit the incident shape:

> **DRAFT (do not publish without sign-off):**
>
> **2026-05-12 14:30 UTC — Metadata service degraded**
>
> We detected an issue with our metadata graph at approximately 14:00
> UTC and are restoring service from a recent backup. Customer-facing
> read APIs remain available; metadata write APIs are paused while we
> complete the restore. Estimated time to full recovery: 1 hour.
> Updates every 15 minutes.
>
> **2026-05-12 15:30 UTC — UPDATE**
>
> Restore complete. Metadata service is fully restored to the state
> as of 14:00 UTC. Any metadata writes between 14:00 and 14:30 UTC
> were lost; affected customers should re-submit. Postmortem to
> follow within 5 business days.

---

## 7. Cross-references

- `tests/dr/restore_pit.sh` — the canonical restore driver
  (referenced from §3)
- `tests/dr/dr_targets_test.go` — Go test wrappers
  (`TestRTO_RestorePIT`, `TestRPO_ChaosRestore`,
  `TestMonthlyRestoreDrill`)
- `infra/pgbackrest/pgbackrest.conf.template` — the stanza template
  (matches `--stanza=neksur` here)
- `infra/pgbackrest/sizing.md` — A7 evidence trail for the 4GB
  archive-push queue
- `runbooks/failover.md` — Patroni HA failover (first-line response
  before this runbook)
- `runbooks/dr-drill.md` — monthly DR drill (proactive validation
  of this runbook)
- `migrations/check.sql` — D-003.06 schema-integrity check (19
  vlabels + 24 elabels)
- `.planning/PROJECT.md` D-OQ.04 — unified DR contract (RTO 1h /
  RPO 15min) — supersedes ADR-001 D-001.13

### Re-running the wrapper test

To exercise the full restore drill in a CI / dev environment:

```bash
DR_LIVE=1 \
go test -tags dr -timeout 90m -run TestRTO_RestorePIT -v ./tests/dr/...
```

This brings up the local 3-node HA cluster, runs the restore drill,
and asserts the RTO contract. Without `DR_LIVE=1` the test SKIPs.

---

## 8. Pool A specifics (Phase 0.5)

This section covers Pool A specifics for the Phase 0.5 multi-tenant SaaS
deployment. Pool A is now the SHARED Postgres-on-EC2 cluster that hosts
all Team + Business tier tenants (D-0.5.01). The Phase 0 runbook above
applies as-is for the cluster-wide restore path; this section adds
Phase 0.5-specific guidance on Pool A specifics — per-tenant restore,
shared pgBackRest stanza, 35-day PITR window.

### 8.a Stanza + bucket

- **pgBackRest stanza:** `neksur` (shared across all Pool A tenants —
  one cluster, one stanza).
- **S3 backup bucket:** `neksur-pool-a-backups-${account_id}` (per the
  Pool A Terraform module `modules/rds-pool-a/main.tf`).
- **PITR window:** **35 days** (D-0.5.15 baseline; `var.backup_retention_archive_days`).
- **Full backup retention:** 7 days (`var.backup_retention_full_days`).

### 8.b Per-tenant restore

When a customer asks "what did tenant X look like at 14:00 yesterday",
the full-cluster PITR restore from §3 is overkill — it restores every
tenant. Per-tenant restore procedure:

1. Restore the entire Pool A cluster to a SANDBOX host via the §3
   procedure with `--target-time` set to the customer's desired
   timestamp.
2. From the restored host, dump JUST the customer's schema:
   ```bash
   pg_dump --schema=tenant_<uuid_underscored> \
     -d "postgres://postgres@<restored-host>:5432/postgres" \
     -f /tmp/tenant_<uuid>_at_<timestamp>.dump
   ```
3. Either return the dump to the customer for inspection, OR
   `pg_restore` it back onto the LIVE Pool A under a different schema
   name (`tenant_<uuid>_restored_<timestamp>`) so the customer can
   diff. **Never `pg_restore` over the live tenant schema** — that
   would overwrite current state.
4. Tear down the sandbox restored host once the customer is satisfied.

### 8.c Cross-references

- `modules/rds-pool-a/main.tf` — Terraform module; outputs
  `pgbackrest_bucket_name` for use in `--repo1-path`.
- `runbooks/dr-drill.md` §M3 First Drill — first scheduled execution
  of this restore path against sandbox Pool A.
- `tests/load/pool_a_capacity_test.go` — capacity benchmark that
  validates the 50-tenant ceiling at which this restore path remains
  operationally viable.

---

## 9. Pool B specifics (Phase 0.5)

This section covers Pool B specifics — per-Enterprise-customer dedicated
Postgres-on-EC2 (D-0.5.02). Each Pool B has its own pgBackRest stanza,
its own S3 backup bucket, and its own KMS key — restore for one Pool B
customer NEVER touches another customer. The Pool B specifics that
differ from Pool A: per-customer stanza, per-customer S3 bucket,
configurable PITR window up to 365 days.

### 9.a Per-customer stanza + bucket

- **pgBackRest stanza:** `neksur-${customer_uuid}` (per-customer; see
  `modules/rds-pool-b/main.tf` `local.pgbackrest_stanza`).
- **S3 backup bucket:** `neksur-pool-b-${customer_uuid}-backups-${account_id}`.
- **PITR window:** configurable per customer up to **365 days** (D-0.5.15
  footnote — Enterprise add-on). Default 35 days matches Pool A.
- **Full backup retention:** configurable per customer up to 365 days.
- **KMS key:** per-customer CMK (`var.kms_key_arn` distinct per customer).
  Restore from a customer's Pool B backup requires `kms:Decrypt` on
  THAT customer's key — the Pool A IAM role does NOT have access to
  Pool B keys.

### 9.b Per-customer restore command

The §3 restore command applies WITH per-customer substitutions:

```bash
bash tests/dr/restore_pit.sh \
    --target-time '2026-05-12 14:00:00+00' \
    --assert-rto-under 3600 \
    --stanza neksur-<customer_uuid> \
    --repo /var/lib/pgbackrest/pool-b/<customer_uuid> \
    --pg-data /var/lib/postgresql/16/main \
    --pg-bin /usr/lib/postgresql/16/bin
```

The `--stanza` and `--repo` flags name the per-customer pgBackRest
setup. The script itself is unchanged from Pool A; only the args differ.

### 9.c Customer notification template

When invoking a Pool B restore (not a drill — actual customer-impacting
restore), notify the customer BEFORE step 3 wipe:

> Subject: **[NEKSUR] Recovery initiated — service window N minutes**
>
> Hello <customer-contact>,
>
> We are initiating a point-in-time recovery for your dedicated Neksur
> database to restore service to <state>. Your database will be
> read-only for approximately N minutes; we will notify you when the
> restore completes and writes resume.
>
> Target timestamp for the restore: `<target_time>`.
> Expected completion: `<target_time + 1h>` per our RTO contract.
> Data committed between the target time and now (~<delta> minutes)
> will be lost from this restore — please re-submit if affected.
>
> — Neksur Ops

### 9.d KMS key rotation considerations

If the customer rotates their KMS key DURING the PITR window:
1. Old backups are still decryptable as long as the old key version
   exists in KMS (rotated keys keep historical versions).
2. New backups are encrypted under the new key version.
3. Restore from old backup works seamlessly because KMS handles
   version lookup transparently.

If the customer DELETES (not rotates) the KMS key, backups become
unrecoverable. This is documented in the Pool B onboarding kit as a
contractual gotcha — customer-managed key delete is irreversible.

### 9.e Cross-references

- `modules/rds-pool-b/main.tf` — per-customer Terraform module
  (variables `customer_uuid`, `kms_key_arn`, `backup_retention_*`).
- `runbooks/pool-b-provisioning.md` — bring up a new Pool B customer.
- `runbooks/pool-b-migration.md` — move a tenant from Pool A → Pool B
  (the reverse direction of the per-tenant restore in §8.b).
- `runbooks/dr-drill-m3-attestation.md` — Pool A drill attestation
  template for M3; a per-Pool-B drill attestation template will be
  added when the first Enterprise candidate signs.
