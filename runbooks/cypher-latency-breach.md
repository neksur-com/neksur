# Runbook — `CypherP99LatencyBreach` alert

**Severity:** PAGE (wakes on-call)
**Contract:** [D-001.14](../docs/decisions/ADR-001.md#d-00114) (Cypher
query observability — P99 sustained > 2s breaches budget) and the early-
warning signal for [D-001.10](../docs/decisions/ADR-001.md#d-00110)
(Phase 2 graph engine migration trigger).
**Alert rule:** [`ops/prometheus/alerts/cypher-latency.yaml`](../ops/prometheus/alerts/cypher-latency.yaml)
**Source metric:** `cypher_duration_ms` (Histogram) — emitted by
[`internal/graph/cypher_wrapper.go::ExecuteCypher`](../internal/graph/cypher_wrapper.go).
**Verified by Go test:** `TestP99BreachPages` in
[`tests/integration/alerts_test.go`](../tests/integration/alerts_test.go) —
synthetic-load end-to-end firing test. Invocation:

```
CHAOS=1 go test -tags integration -timeout 15m \
  -run TestP99BreachPages ./tests/integration/...
```

---

## What this alert means

The 99th-percentile Cypher query duration over the trailing 5-minute
window is above 2000 ms, and the breach has been sustained for at
least 5 minutes (`for: 5m`). One slow query is normal; sustained P99
breach is a graph-shape or planner regression that the D-001.14
observability contract demands paging on.

## Triage Setup (one-time)

Before this runbook is useful, the observability stack must be running
and AlertManager wired to the operator's secret store:

```bash
export PAGERDUTY_INTEGRATION_KEY=<real-key>
export SLACK_WEBHOOK_URL=<real-webhook>
docker compose -f infra/otel/docker-compose.observability.yml up -d
```

Verify the rule was loaded:

```bash
curl -s http://localhost:9090/api/v1/rules | jq '.data.groups[].rules[].name' | grep CypherP99LatencyBreach
```

## 1. Triage (first 5 minutes)

1. Open the Grafana Cypher dashboard
   (Phase 1 deliverable; Phase 0 uses raw Prometheus):
   ```
   http://prometheus:9090/graph?g0.expr=histogram_quantile(0.99, sum by (le) (rate(cypher_duration_ms_bucket[5m])))
   ```
2. Identify the top-N slowest queries via per-graph breakdown:
   ```
   topk(10, histogram_quantile(0.99, sum by (graph, le) (rate(cypher_duration_ms_bucket[5m]))))
   ```
3. Check recent deploys — was a new vlabel / elabel added? Did the
   migration index (V0020 / V0025) land cleanly? `kubectl rollout
   history deployment/neksur-server`.
4. Check `cypher_errors_total` — a spike in errors often correlates
   with a planner regression (queries falling back to seq scan after
   timing out).

## 2. Mitigation

### A. Postgres autovacuum / bloat
```sql
SELECT relname, n_dead_tup, n_live_tup,
       round(100 * n_dead_tup::numeric / nullif(n_live_tup, 0), 1) AS dead_pct
  FROM pg_stat_user_tables
 WHERE schemaname = 'ag_catalog' OR relname LIKE '%_v_%' OR relname LIKE '%_e_%'
 ORDER BY dead_pct DESC NULLS LAST
 LIMIT 10;
```
If `dead_pct > 20` on any vlabel / elabel table, run a targeted
`VACUUM ANALYZE` against it.

### B. Seq Scan regression (Pitfall 2)
Pull an `EXPLAIN ANALYZE` of the slowest representative query —
specifically watch for `Seq Scan` against `_ag_label_vertex` or any
per-vlabel inherited table. The D-001.07 indexes (V0020) and the
D-001 tenant indexes (V0025) are sized so that bounded VLP queries
(`*1..3`) ALWAYS hit an index. A `Seq Scan` indicates either a
planner statistics drift (run `ANALYZE`) or a missing index (drift
from the migration).

### C. Connection pool saturation
```
sum by (state) (pg_stat_activity_count{datname="neksur"})
```
If `idle in transaction` is climbing, a caller is holding a long-
running transaction — chase it through `pg_locks` and consider a
forced `pg_terminate_backend`.

## 3. Escalation — D-001.10 graph-engine migration trigger

If the breach is **persistent** (not a transient deploy-induced spike)
**AND** the affected graph's edge count is growing (track via
`cypher_edges_traversed_count{graph=...}` over 7 days), this alert is
the FIRST of the three D-001.10 signals that trigger the Phase 2 graph-
engine migration evaluation (per ADR-001).

Action: open a follow-up note in [ADR-001 D-001.10 tracking](../docs/decisions/ADR-001.md#d-00110)
capturing the breach window, top-N slow queries, and graph-edge growth
rate. Tag the Phase-2 ADR author. **Do NOT bypass** by relaxing the
2-second budget — the budget is the canonical D-001.14 contract and
relaxing it loses the migration signal.

## 4. False-positive cases (resolve, don't escalate)

- **Cold start after deploy:** P99 recovers within 5–10 minutes as the
  planner cache and AGE session-level state warm. If `for: 5m` window
  catches a cold start, AlertManager auto-resolves on the next scrape.
- **Scheduled bulk import:** A nightly ETL run can saturate the pool
  for tens of minutes. Phase 1 will add a `cypher_duration_ms_classification`
  label (interactive vs batch) to split this; Phase 0 acknowledges the
  false-positive risk in the alert annotations.

---

## References

- [D-001.14 Cypher observability contract (ADR-001)](../docs/decisions/ADR-001.md#d-00114)
- [D-001.10 Phase 2 graph engine migration trigger (ADR-001)](../docs/decisions/ADR-001.md#d-00110)
- [Plan 00-05 SUMMARY](../.planning/phases/00-metadata-graph-foundation/00-05-SUMMARY.md)
  (when committed) — A6 outcome + bucket tuning rationale.
- [Pitfall 9 — HA replication lag interaction (00-RESEARCH §Pitfalls)](../.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md)
- [Pitfall 2 — AGE planner Seq Scan regression](../.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md)
