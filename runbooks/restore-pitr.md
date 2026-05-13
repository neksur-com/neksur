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
