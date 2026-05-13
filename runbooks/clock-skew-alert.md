# Runbook — `ClockSkewWarn` / `ClockSkewPage` alerts

**Severity:** WARN (Slack only) / PAGE (wakes on-call)
**Contract:** [C-TECH-10](../docs/decisions/SPEC-v0.7.md#c-tech-10)
(clock-skew tolerance; `snapshot_id` is the canonical ordering key).
**Alert rule:** [`ops/prometheus/alerts/clock-skew.yaml`](../ops/prometheus/alerts/clock-skew.yaml)
**Source metric:** `chrony_tracking_last_offset_seconds` — emitted by
the [chrony-exporter sidecar](../infra/chrony/chrony-exporter.yml) on
every Postgres host, configured per [`infra/chrony/chrony.conf`](../infra/chrony/chrony.conf).
**Verified by Go tests:** `TestChronyRunning`, `TestSkewWarnThreshold`,
`TestSkewPageThreshold` in
[`tests/integration/clock_skew_test.go`](../tests/integration/clock_skew_test.go).
Invocation:

```
go test -tags integration -run TestChronyRunning ./tests/integration/...
go test -tags integration -run "TestSkewWarnThreshold|TestSkewPageThreshold" ./tests/integration/...
```

---

## What these alerts mean

`chronyd` on at least one Postgres host reports the system clock has
drifted from its NTP source. The two-tier thresholds match
[Pitfall 8](../.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md):

| Tier | Threshold | Sustained | Severity | Routing |
|------|-----------|-----------|----------|---------|
| WARN | abs(offset) > 100 ms | 1 min | warn | Slack only |
| PAGE | abs(offset) > 500 ms | 1 min | page | PagerDuty |

## Iceberg / C-TECH-10 implication

**Clock skew is NOT data-correctness-fatal in Phase 0.** Per C-TECH-10,
the Iceberg `snapshot_id` is the **canonical ordering key** for the
audit log and metadata graph events; wall-clock timestamps are
auxiliary. The alerts are an **early warning** that timestamp-based
queries (operator dashboards, debugging filters) may show out-of-order
events, NOT that the underlying data is corrupt.

This contract is documented in C-TECH-10. Do NOT block writes, fail
over, or rebuild snapshots in response to a clock-skew alert.

## 1. Triage (first 5 minutes)

SSH into the affected Postgres host (the `host` label on the alert
identifies it):

```bash
chronyc tracking
# Look for:
#   Last offset     :  ±0.NNN seconds   <-- the alert metric
#   RMS offset      :  ±0.NNN seconds   <-- long-running smoothed value
#   Frequency       :  ±NN.NNN ppm      <-- crystal drift coefficient
#   Skew            :  NN.NNN ppm       <-- uncertainty bound
#   Stratum         :  N                <-- expect 2-4; 0/1 is suspicious
```

Check the NTP source health:

```bash
chronyc sources -v
# Each source line: state (^? / ^* / ^+ / ^- / ^x) reach time poll
#  '^*' = currently-selected source (good)
#  '^?' = unreachable (BAD if it's our only source)
#  '^x' = falseticker (BAD — chronyd rejected it)
```

Check daemon health:

```bash
systemctl status chrony
journalctl -u chrony --since=-30m
```

## 2. Mitigation

### A. Single transient spike (most common)
Cloud-host NTP packets can occasionally drop or be re-ordered by a busy
hypervisor. `chronyd` corrects via slew (slow adjustment) and the next
1-minute window drops back under threshold. **No action required;
the alert auto-resolves.**

### B. Persistent drift — restart chronyd
```bash
systemctl restart chrony
# Wait for chronyc tracking to show `Reference ID` and a small Last offset.
chronyc tracking
```

### C. Source failure — switch NTP pool
Edit `/etc/chrony/chrony.conf`, change the `pool` line to a closer or
more reliable source (cloud-provider time service is usually best:
`169.254.169.123` on AWS, `metadata.google.internal` on GCP).
```bash
systemctl reload chrony
```

### D. Network / firewall
NTP needs **UDP 123 outbound** to the pool servers. If a recent
firewall change blocks it, every source falls to `^?` (unreachable)
and drift grows unbounded. Restore the egress rule, then restart
chronyd.

## 3. Escalation — PAGE tier (> 500 ms)

If `ClockSkewPage` fires, the system clock is far enough off that:
- Postgres `LOG_TIMEZONE` timestamps may misorder by half a second
  (Phase 0 acceptable, but operator dashboards will look wrong).
- Audit-log queries filtering by `event_time BETWEEN ...` may miss or
  duplicate events at the boundaries. **Use `snapshot_id` ordering
  instead** until clock is back in tolerance.

If the PAGE alert is sustained > 15 minutes after mitigation attempts:
- Open an incident channel; consider isolating the affected host (DRAIN
  via HAProxy) until chrony stabilises.
- DO NOT trigger failover purely on clock skew — that's a separate
  decision driven by application-level health, not NTP health.

## 4. False positives

- **Server boot:** chrony's `makestep 1.0 3` permits a single step
  adjustment in the first 3 measurements; if those happen WITHIN a
  1-minute window the alert may catch the tail. AlertManager
  auto-resolves on the next scrape.
- **NTP-server-side reset:** if the upstream pool rotates a stratum-1
  reference, a brief 100-300 ms spike is normal. The 1-minute `for`
  clause is calibrated to absorb this.

---

## References

- [C-TECH-10 clock skew tolerance (SPEC v0.7)](../docs/decisions/SPEC-v0.7.md#c-tech-10)
- [Pitfall 8 — clock-skew tier rationale (00-RESEARCH)](../.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md)
- [chrony-exporter docs](https://github.com/SuperQ/chrony_exporter#metrics)
- [`infra/chrony/chrony.conf`](../infra/chrony/chrony.conf) — chronyd config
- [`infra/chrony/chrony-exporter.yml`](../infra/chrony/chrony-exporter.yml) — exporter contract
- [Plan 00-05 SUMMARY](../.planning/phases/00-metadata-graph-foundation/00-05-SUMMARY.md)
  (when committed)
