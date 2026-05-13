# pgBackRest Sizing — Phase 0 Wave 3 (Plan 00-04)

**Contract:** D-OQ.04 (LOCKED) — RTO 1h / RPO 15min unified across all
metadata stores; supersedes ADR-001 D-001.13's split RPO 1h / RTO 30min.

**Source:** `.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md`
§pgBackRest Configuration for 15-min RPO + §Pitfall 6 + §Assumptions A4 + A7.

---

## 1. The 15-min RPO math

The 15-min RPO contract bounds the maximum data loss observed when the
primary fails and the surviving cluster restores from the most recent
WAL pushed to the pgBackRest repo.

Three settings compose the contract:

| Setting | Value | Layer | Role |
|---------|-------|-------|------|
| `archive_timeout` | `60` (seconds) | Postgres (patroni.yml.template) | Forces a WAL segment switch every 60s. Caps maximum data-loss-on-crash to ~60s of WAL on the primary itself (segment not yet handed to pgBackRest). |
| `archive-async` | `y` | pgBackRest (pgbackrest.conf.template) | Pitfall 6 — sync mode would block PostgreSQL on each archive_command call; async lets WAL push proceed concurrently. CRITICAL for the 15-min RPO under burst write load. |
| `archive-push-queue-max` | `4GB` | pgBackRest (pgbackrest.conf.template) | A7 — sized for ~15 min of WAL @ 4 MB/s sustained write rate. The queue is the async-push buffer; if it fills, archive-push starts dropping segments and the RPO contract breaks. |

The composition: a primary crash loses at most `archive_timeout +
in-flight async queue depth` of WAL = ~60s + (≤4GB queue residency
~= 0 at steady state, ~15min at saturation) ≈ 15 min worst-case.
The 15-min ceiling is the design target; routine crashes lose
1-2 minutes of WAL.

---

## 2. The 4GB queue size — A7 assumption

**Assumption A7 (RESEARCH.md):** the Phase 0 envelope sustains 10K
lineage events/sec × ~400 B average per event = ~4 MB/s WAL output. A
4GB async-push queue therefore holds:

```
4 GB / 4 MB/s = 1024 seconds ≈ 17 minutes of WAL headroom
```

…which clears the 15-min RPO contract with a ~2-min safety margin.

**This number is an A7 assumption from research. Plan 00-04 empirically
validates it before sign-off** by running the Phase 0 envelope write
workload against a live Postgres + AGE instance and measuring the
actual WAL output rate during a sustained 5-minute burst.

The validation tool: `tests/dr/wal_throughput.go::MeasureWalThroughput`
(declared `package dr`).

### How to run the A7 validation

```bash
# Bring up a Postgres+AGE container per integration tests:
DATABASE_URL="postgres://postgres:postgres@localhost:5432/neksur?sslmode=disable" \
go test -tags dr -run TestMeasureWalThroughput -timeout 10m -v ./tests/dr/...
```

The test:

1. Spins a writer goroutine emitting Cypher inserts at 1000 writes/sec
   for 300 seconds (the Phase 0 envelope sustained rate, scaled for
   single-host CI).
2. Polls `pg_current_wal_lsn()` every 5 seconds and computes the
   per-interval bytes-per-second.
3. Returns a `WalThroughputReport` with `AvgMBps`, `PeakMBps`, and
   `Implied15MinWalGB = AvgMBps * 60 * 15 / 1024`.
4. Asserts `Implied15MinWalGB < 4.0` (the A7 confirmation gate).
5. Writes the JSON report to `tests/dr/_wal_throughput_report.json`
   as the A7 evidence file.

### Reading the evidence

After the test runs, `tests/dr/_wal_throughput_report.json` looks like:

```json
{
  "AvgMBps": 3.7,
  "PeakMBps": 5.1,
  "Implied15MinWalGB": 3.25,
  "Samples": [
    {"At": "2026-05-13T10:00:05Z", "LSN": "0/1A2B3C00", "BytesPerSec": 3500000.0},
    {"At": "2026-05-13T10:00:10Z", "LSN": "0/1B2C4D00", "BytesPerSec": 3700000.0},
    ...
  ]
}
```

The fixed `4GB` queue value in `pgbackrest.conf.template` is correct
**iff** observed `Implied15MinWalGB` is consistently below `4.0` AND
`PeakMBps` does not exceed `5 MB/s` (the burst-tolerance margin).

If observed values exceed those thresholds, this doc must be updated
with the new sized value AND `pgbackrest.conf.template`'s
`archive-push-queue-max` raised to match.

---

## 3. Repo sizing

The retention policy in `pgbackrest.conf.template` keeps:

- 4 weekly fulls × ~200 GB Phase 0 fixture     = ~800 GB base backups
- 7 daily diffs   × ~10-20 GB                  = ~70-140 GB diffs
- 14 days of WAL  × ~340 GB/day @ 4 MB/s avg   = ~4.7 TB WAL

…totaling roughly **5.5 TB** of repo storage at Phase 0 envelope load.

For S3-backed repos (the production deployment shape), this is
inexpensive (~$125/month at S3 standard rates) — compression at
`compress-level=3` halves the WAL volume.

For on-prem repos, plan for **6 TB usable** with appropriate IOPS for
the restore path (a full 200 GB restore at 1 GB/s repo throughput
takes ~3.5 minutes pure I/O, well under the 1-hour RTO budget).

---

## 4. Restore-time math (RTO 1h)

A point-in-time restore (PITR) walks:

1. Restore the most recent base backup     → ~3-30 min depending on size.
2. Replay WAL forward to `--target-time`   → linear in WAL volume × replay speed.

Postgres replays WAL at roughly 50-200 MB/s on commodity SSDs. For a
typical PITR target inside the most recent diff (worst case ~14 days
of archived WAL = 4.7 TB to potentially walk, but in practice the
restore selects the most recent diff + only the WAL after it):

- Base + most recent diff restore: ~10 min (200 GB total at 333 MB/s).
- WAL replay from most recent backup tip → target time: 0-15 min for
  routine PITRs targeting the last hour; up to ~45 min for a 12h-back
  target.
- Total: well within the 1-hour RTO ceiling.

This is empirically validated by `tests/dr/restore_pit.sh
--assert-rto-under 3600`, wrapped by Go test
`TestRTO_RestorePIT` in `tests/dr/dr_targets_test.go`
(`//go:build dr`).

---

## 5. Sign-off checklist

Before declaring Plan 00-04 complete, verify:

- [ ] `tests/dr/_wal_throughput_report.json` exists and shows
      `Implied15MinWalGB < 4.0`.
- [ ] `pgbackrest --config=infra/pgbackrest/pgbackrest.conf.template
      check` against a real stanza passes.
- [ ] `bash tests/dr/restore_pit.sh --assert-rto-under 3600` exits 0
      (validates RTO 1h).
- [ ] `bash tests/dr/chaos_restore.sh --assert-rpo-under 900` exits 0
      (validates RPO 15min after a SIGKILL chaos test).
- [ ] `bash tests/dr/run_monthly_restore_drill.sh` exits 0 and writes
      a drill report under `runbooks/drill-reports/`.

The first item is the A7 gate; the remaining four are the
contract-level RTO/RPO/runbook gates wired by Tasks 2 and 3 of this
plan.

---

## Cross-references

- `infra/pgbackrest/pgbackrest.conf.template` — stanza config (this doc's
  evidence target)
- `infra/patroni/patroni.yml.template` — `archive_command` + `archive_timeout`
  on the Postgres side
- `tests/dr/wal_throughput.go` — `MeasureWalThroughput` validation function
- `tests/dr/dr_targets_test.go` — Go test harness gating RTO/RPO/drill
- `.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md` §A7
- `.planning/PROJECT.md` D-OQ.04 — unified DR contract
