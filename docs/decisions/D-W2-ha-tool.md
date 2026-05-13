# D-W2-ha-tool — Patroni vs pg_auto_failover for Phase 0 HA

**Status:** LOCKED
**Decided:** 2026-05-13
**Decision-makers:** Phase 0 Wave 2 executor (per RESEARCH §Open Question #1 recommendation)
**Supersedes:** Open Question #1 in 00-RESEARCH.md
**Applies to:** Phase 0 Wave 2 (and forward — every HA Postgres+AGE deployment in
Phases 0–7 unless an ADR amendment supersedes this).

---

## Decision

**Use Patroni 4.1+ over pg_auto_failover.**

Patroni manages a 3-node Postgres 16 + AGE 1.6.0 cluster with etcd 3.5+ as the
distributed configuration store (DCS). HAProxy 2.x routes write traffic to the
current Patroni leader via the Patroni REST `/master` health-check endpoint,
and read traffic to followers via `/replica`.

This decision satisfies REQ-NFR-availability and the D-001.15 contract
(<30s failover from primary kill to new leader serving writes).

---

## Context — what was on the table

Postgres has two production-grade HA orchestrators in 2026:

1. **Patroni** (citusdata/patroni, Apache-2.0) — Python daemon co-located with
   each Postgres instance; uses an external DCS (etcd, Consul, ZooKeeper, or
   Kubernetes API) for leader election and cluster state. Exposes a REST API
   that both load balancers (HAProxy via `/master` health check) and operators
   (`patronictl`) consume.
2. **pg_auto_failover** (citusdata/pg_auto_failover, PostgreSQL license) —
   single "monitor" Postgres process tracks cluster state in its own database;
   `pg_autoctl` daemons on each data node coordinate via the monitor.

Both can deliver sub-30s failover in lab conditions. The decision is about
operational shape, not raw failover speed.

---

## Decision rule

Per RESEARCH §Open Question #1:

> "Patroni is more featureful and battle-tested at scale; pg_auto_failover is
> simpler but the monitor is a SPoF unless mirrored."

The single-monitor SPoF is the load-bearing argument: any HA system that
introduces its own coordination SPoF erodes the simplicity advantage that was
its main attraction. Once you mirror the pg_auto_failover monitor, you've
re-introduced the same DCS-style complexity Patroni already solved with
battle-tested etcd integration — without inheriting Patroni's mature
operational tooling.

---

## Why Patroni is the right shape for Phase 0

| Property | Patroni 4.1 | pg_auto_failover 2.x |
|----------|-------------|----------------------|
| Coordinator SPoF? | No — etcd 3-node Raft quorum | Yes — single monitor (mirror = extra ops + still SPoF on the mirror sync) |
| Sub-30s failover at scale | Documented — multiple Citus / Crunchy Data / Zalando production reports | Documented in lab; fewer at-scale reports |
| HAProxy integration | Native via `option httpchk GET /master` on REST port 8008 | Requires custom health check or Citus extension |
| Split-brain prevention | etcd quorum + `synchronous_mode: true` + `maximum_lag_on_failover` | Monitor-arbitrated; depends on monitor consistency |
| AGE-friendly? | Patroni does not introspect extensions — `shared_preload_libraries='age'` carries through bootstrap → standby creation → promotion (Pitfall 9 mitigation; A1 hinges on this — empirically validated by `TestPostFailoverCypherWorks`) | Same property — neither tool understands AGE specifically |
| Operator tooling | `patronictl list / switchover / failover / pause / resume / reload / restart / edit-config` | `pg_autoctl show / state / enable / disable maintenance` (smaller surface) |
| Watchdog support | systemd watchdog + Patroni's built-in TTL guard | Systemd-only |
| Community + ecosystem | Zalando, Citus, Crunchy Data, GitLab, IBM Cloud — large public deployments | Citus / community; smaller production footprint |

For Phase 0 the decisive properties are:

- **Sub-30s failover with a HAProxy front door** (D-001.15 contract).
  Patroni's `/master` endpoint is the textbook integration shape — `option
  httpchk GET /master` on port 8008 is documented in Patroni's own ops guide
  and used identically by every Patroni-fronted HAProxy deployment.
- **No coordinator SPoF.** etcd 3-node quorum (3 nodes survives 1 failure)
  matches the 3-node Postgres cluster topology — same fault domain, same
  ops surface.
- **Patroni does not introspect Postgres extensions.** This is the same
  property as pg_auto_failover, but Patroni's surface area is wider — meaning
  more operational levers when AGE behaviour at promotion time has to be
  inspected (`patronictl edit-config` to amend `shared_preload_libraries`
  cluster-wide, `patronictl reload` to push the change without restart).
- **Empirical validation of A1 (AGE-on-promotion) is testable** — chaos test
  `TestPostFailoverCypherWorks` exercises the exact promote-then-Cypher path
  this decision rests on.

---

## What pg_auto_failover would have given up

- Operator can't cluster-wide-edit Postgres parameters via `pg_autoctl` —
  must SSH to each node.
- HAProxy integration requires either a custom polling script against the
  monitor or Citus's extension, neither of which is in our anti-stack.
- Monitor mirror = additional ops surface that re-introduces the very SPoF
  pg_auto_failover was supposed to eliminate.

---

## Configuration invariants enforced by Patroni template

Per RESEARCH §Patroni Configuration sketch and §Pitfall 9, the following
invariants are encoded in `infra/patroni/patroni.yml.template` AND in
`infra/postgres/postgresql.ha.conf`. They are the four non-negotiable
properties of the Phase 0 HA cluster:

| Invariant | Value | Rationale |
|-----------|-------|-----------|
| `bootstrap.dcs.ttl` | `30` | D-001.15 — failover SLO upper bound (TTL is the floor; actual measured time is `kill → new leader` which is bounded by `ttl` + `loop_wait`) |
| `bootstrap.dcs.synchronous_mode` | `true` | Graph integrity — committed writes survive promotion (no silent rollback to lower-XID standby) |
| `postgresql.parameters.shared_preload_libraries` | `'age,pgaudit,pg_stat_statements'` | A1 mitigation — AGE is loaded at server start on every standby BEFORE promotion, eliminating the "function cypher does not exist" failure mode (Pitfall 9) |
| `postgresql.parameters.archive_timeout` | `'60'` | Wave 3 DR carryover — bounds the WAL replay floor at ~60s + push latency |

Additional safety / performance settings (loop_wait=10, retry_timeout=10,
maximum_lag_on_failover=1MB, max_wal_senders=10, wal_level=replica,
hot_standby=on) follow the RESEARCH sketch verbatim.

---

## What this implies for downstream waves

- **Wave 3 (Plan 00-04 — pgBackRest):** Patroni's `archive_command` is the
  same `pgbackrest --stanza=neksur archive-push %p` placeholder Plan 00-04
  resolves. The Patroni template ships with this placeholder string
  pre-populated; Plan 04 only needs to provision the stanza and confirm.
- **Wave 4 (Plan 00-05 — observability):** Patroni REST `/cluster` and
  `/health` endpoints are scrape-friendly; node-exporter on each cluster
  member captures the rest. PromQL alerts for `patroni_master_change_total`
  and `patroni_xlog_replayed_location_lag` come into scope here.
- **Phase 0.5 (SaaS pilot):** The Patroni template is the per-tenant deploy
  unit. ADR-004 §6 references this decision for the per-tenant data plane.

---

## Reversibility

If a future phase finds pg_auto_failover a better fit (e.g., Phase 7
self-managed Enterprise on-prem ships a reduced ops surface where the
monitor-as-SPoF tradeoff is acceptable), this decision can be revisited via
ADR amendment. Until then Patroni 4.1+ is the floor.

The decision was made BEFORE rather than AFTER an empirical spike because
Patroni's production track record at the operational scale Phase 0 targets
(3-node cluster, single region, sub-30s failover) is well-documented in the
public sources cited below; a spike would only re-confirm what those reports
already establish. The 30s failover contract IS empirically validated by
`tests/chaos/failover_test.go::TestKillPrimaryFailoverUnder30s`.

---

## Related artifacts in this plan

- `infra/patroni/patroni.yml.template` — Patroni config (Jinja2 placeholders)
- `infra/patroni/systemd/patroni.service` — systemd unit
- `infra/etcd/etcd.yml.template` — 3-node etcd DCS
- `infra/etcd/systemd/etcd.service` — systemd unit
- `infra/haproxy/haproxy.cfg.template` — HAProxy with `/master` + `/replica` health checks
- `infra/postgres/postgresql.ha.conf` — HA-mode Postgres config (extends `postgresql.base.conf`)
- `infra/docker-compose.ha.yml` — local 3-node HA cluster for chaos testing
- `tests/chaos/patroni_chaos.go` — Go chaos library (StartCluster / KillPrimary / WaitForNewLeader / TimeFailover / StopCluster)
- `tests/chaos/failover_test.go` — `//go:build chaos` — TestKillPrimaryFailoverUnder30s + TestPostFailoverCypherWorks
- `tests/integration/replication_test.go` — `//go:build integration` — TestReplicaLagUnderLoad
- `runbooks/failover.md` — operator runbook

---

## See also

- 00-RESEARCH.md §Open Question #1 (the question this ADR closes)
- 00-RESEARCH.md §Standard Stack — Patroni / etcd / HAProxy rows
- 00-RESEARCH.md §Patroni Configuration sketch (the template's source of truth)
- 00-RESEARCH.md §Pitfall 9 — Patroni failover triggers AGE extension reload
- 00-RESEARCH.md §Assumptions A1 + A8 — empirically validated by Plan 00-03 chaos tests
- PROJECT.md D-001.15 — <30s failover SLO
- REQUIREMENTS.md REQ-NFR-availability — the requirement this plan satisfies

## Sources (HIGH confidence — production reports)

- Patroni docs: https://patroni.readthedocs.io/en/latest/ (citusdata/patroni)
- Zalando Patroni in production: https://github.com/zalando/patroni/blob/master/docs/SETTINGS.rst
- Crunchy Data Postgres Operator (Patroni-based): https://access.crunchydata.com/documentation/postgres-operator/latest/
- pg_auto_failover (for comparison): https://github.com/citusdata/pg_auto_failover
