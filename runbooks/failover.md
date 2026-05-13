# Runbook: Patroni HA Failover Incident Response

**Owner:** Phase 0 SRE / on-call
**Scope:** Neksur 3-node Patroni-managed Postgres + AGE cluster (Plan 00-03)
**SLO contract:** D-001.15 — kill-to-new-leader < 30 seconds
**Validation tests:** `tests/chaos/failover_test.go` (chaos build tag)

---

## 1. When to use this runbook

Trigger this runbook when ANY of the following occur:

- **Alert fires:** `up{job="patroni"} == 0` for >1 minute on any cluster member.
- **Alert fires:** `patroni_master_change_total` increments unexpectedly
  (a leader change without an operator-issued `patronictl switchover`).
- **Application reports:** "connection lost" / "could not connect" errors
  on writes against the HAProxy primary endpoint (`postgres://...:5000/...`).
- **Application reports:** Cypher queries returning
  `function cypher(unknown, unknown) does not exist` post-leader-change —
  this is the **A1 failure mode** (jump to §4).
- **You observe:** `patronictl list` shows no Leader, OR shows multiple
  members claiming Leader, OR shows a Leader with all replicas in `lag` state.

Patroni's design is that automatic failover is the FIRST line of defense.
If the cluster is functioning correctly, you should NOT need to take manual
action — Patroni's etcd-arbitrated leader election will pick a new Leader
within `bootstrap.dcs.ttl + loop_wait = 30 + 10 = 40s` worst-case (the
empirical floor is ~10-15s; see chaos test results in
`.planning/phases/00-metadata-graph-foundation/00-03-SUMMARY.md`).

This runbook covers the cases where automatic failover is delayed,
incomplete, or has produced an inconsistent post-failover state.

---

## 2. First 30 seconds — observe, don't intervene

Patroni's automatic failover should already be in progress. Resist the
urge to immediately run `patronictl failover` — manual intervention
during automatic failover can race with etcd and prolong the outage.

### Observation commands

```bash
# Show cluster state — current leader (if any), all members + their roles + lag.
patronictl -c /etc/patroni/patroni.yml list

# Watch leader election in real time (refresh every 1s).
watch -n 1 'patronictl -c /etc/patroni/patroni.yml list'

# Check Patroni daemon health on each node — useful when one node is the
# suspected blocker.
for node in pg-node-1 pg-node-2 pg-node-3; do
    echo "=== $node ==="
    curl -s "http://${node}:8008/health" | jq .
done

# etcd health — if etcd has lost quorum, Patroni cannot elect a Leader at all.
etcdctl --endpoints=etcd-1:2379,etcd-2:2379,etcd-3:2379 endpoint health

# HAProxy stats — confirm HAProxy detected the leader change.
curl -s http://haproxy:7000/ 2>/dev/null | grep -E 'postgres_(primary|replicas)'
```

### Decision tree

| Observation | Action |
|-------------|--------|
| `patronictl list` shows 1 Leader + 2 Replicas, all `running` | No action — failover succeeded. Verify §3.c (post-failover Cypher) and close incident. |
| `patronictl list` shows no Leader, 1 minute since alert | Wait one more `loop_wait` (10s). If still no Leader after 60s total, proceed to §3 (manual intervention). |
| `etcdctl endpoint health` shows <2 healthy etcd members | etcd lost quorum — escalate. Patroni cannot elect a Leader without etcd quorum. |
| Multiple nodes claim Leader role | Split-brain — escalate IMMEDIATELY. Do not attempt manual switchover; the `synchronous_mode: true` invariant should have prevented this. |

---

## 3. Manual intervention sequence

Only proceed if §2's decision tree directed you here (no Leader after 60s,
or operator-driven switchover required).

### 3.a Inspect etcd state

The DCS source-of-truth lives under `/db/neksur-pg/`. The leader key holds
the current Leader's name and a TTL.

```bash
etcdctl --endpoints=etcd-1:2379,etcd-2:2379,etcd-3:2379 \
    get /db/neksur-pg/ --prefix --keys-only

# Inspect the leader lock:
etcdctl --endpoints=etcd-1:2379,etcd-2:2379,etcd-3:2379 \
    get /db/neksur-pg/leader

# Members (each Patroni publishes its name and connect URL here):
etcdctl --endpoints=etcd-1:2379,etcd-2:2379,etcd-3:2379 \
    get /db/neksur-pg/members/ --prefix
```

If `/db/neksur-pg/leader` is missing entirely and no member is publishing
itself with a recent TTL, etcd has likely garbage-collected stale state.
Restart Patroni on each surviving node:

```bash
systemctl restart patroni    # on each pg-node-N
```

### 3.b Force a leader change via patronictl

If the automatic election did not converge (rare — usually means a
network partition between Patroni and etcd), force the change:

```bash
# Query candidates (lag < maximum_lag_on_failover):
patronictl -c /etc/patroni/patroni.yml list

# Switchover (graceful — current leader cooperates if alive):
patronictl -c /etc/patroni/patroni.yml switchover \
    --master <current_leader_or_blank_if_none> \
    --candidate <healthy_replica_name>

# Failover (forceful — use ONLY if the current leader is unreachable
# AND switchover refuses to proceed; bypasses some safety checks):
patronictl -c /etc/patroni/patroni.yml failover \
    --candidate <healthy_replica_name>
```

The `--candidate` argument MUST be a node that:
- Is currently a `replica` in `patronictl list`
- Has `lag` reported as 0 (or below `maximum_lag_on_failover = 1 MB`)
- Returns 200 on `curl http://<candidate>:8008/replica`

### 3.c Verify AGE works post-promotion (the load-bearing check)

After a new Leader is elected (automatically or manually), connect via
HAProxy port 5000 and run a Cypher round-trip. THIS IS THE STEP THAT
VALIDATES THE A1 GUARANTEE — if it fails with the A1 failure mode, jump
to §4.

```bash
# Connect to HAProxy primary route — HAProxy's /master health check
# should already be routing to the new Leader by now (3-9s detection).
psql "postgres://neksur_app@haproxy:5000/neksur" -c \
    "SELECT * FROM cypher('neksur', \$\$ MATCH (n) RETURN count(n) \$\$) AS (c agtype);"
```

Expected output:

```
   c
-------
 12345    -- (or whatever the count is)
(1 row)
```

If the query returns a count without erroring, the failover is COMPLETE
and the A1 guarantee held. Document the incident timeline and close.

If the query errors with `function cypher does not exist`, you have hit
the A1 failure mode — proceed to §4.

---

## 4. A1 failure mode — `function cypher does not exist`

This is the failure mode that the Plan 00-03 chaos test
`TestPostFailoverCypherWorks` is designed to prevent. Seeing this error
in production means the standby that was promoted came up without
`shared_preload_libraries='age'` set, so the AGE extension is not loaded
in the new Leader's address space.

### 4.a Confirm the diagnosis

```bash
# Connect directly to the new Leader (bypassing HAProxy to be sure).
NEW_LEADER=$(patronictl -c /etc/patroni/patroni.yml list | awk '$5 == "Leader" {print $3}')
psql "postgres://postgres@${NEW_LEADER}:5432/postgres" -c \
    "SHOW shared_preload_libraries;"
```

The expected output is `age,pgaudit,pg_stat_statements` (the value enforced
by `infra/postgres/postgresql.ha.conf` and `infra/patroni/patroni.yml.template`).

If the output is missing `age` (e.g., shows only `pgaudit,pg_stat_statements`
or empty), the new Leader was promoted from a standby that did not have the
contracted preload list. This is a configuration drift — the standby's
`postgresql.conf` diverged from the Patroni cluster spec.

### 4.b Recover

```bash
# 1. Re-assert shared_preload_libraries on the cluster via Patroni's DCS edit.
patronictl -c /etc/patroni/patroni.yml edit-config

# In the editor, ensure this block is present under postgresql.parameters:
#
#   parameters:
#     shared_preload_libraries: 'age,pgaudit,pg_stat_statements'
#     # ... other parameters ...
#
# Save and exit. Patroni propagates the change via etcd to all nodes.

# 2. Restart Postgres on the affected node (Patroni manages this).
patronictl -c /etc/patroni/patroni.yml restart neksur-pg ${NEW_LEADER}

# 3. Verify post-restart.
psql "postgres://postgres@${NEW_LEADER}:5432/postgres" -c \
    "SHOW shared_preload_libraries;"

# 4. Verify Cypher works.
psql "postgres://neksur_app@${NEW_LEADER}:5432/neksur" -c \
    "SELECT * FROM cypher('neksur', \$\$ MATCH (n) RETURN count(n) \$\$) AS (c agtype);"

# 5. After Postgres restart on the new Leader, the cluster will likely
#    re-elect (the restart took the Leader briefly offline). Re-run §2
#    observation commands to confirm cluster shape.
```

### 4.c Audit the drift

If §4 was needed, file a follow-up to determine why the standby's
`shared_preload_libraries` diverged from the cluster spec:

- Was the standby provisioned outside the Plan 00-03 template?
- Did an operator manually edit `postgresql.conf` on the standby?
- Did a prior `patronictl edit-config` accidentally drop the `age` entry?

Audit `git log infra/postgres/postgresql.ha.conf
infra/patroni/patroni.yml.template` and compare against the running
cluster's `SHOW shared_preload_libraries;` output on every node.

---

## 5. Post-incident

### 5.a Document the timeline

Record (in the incident ticket):

| Time (UTC) | Event |
|------------|-------|
| T₀         | First alert / observed symptom |
| T₀ + Xs    | First on-call action |
| T₀ + Ys    | New Leader elected (`patroni_master_change_total` increment) |
| T₀ + Zs    | First successful post-failover Cypher round-trip |
| T₀ + Ws    | Cluster fully recovered (`patronictl list` 1 Leader + 2 Replicas, all `running`) |

Compare T₀+Y minus the kill-time (if known) against the D-001.15 contract
(< 30s). Compare T₀+Z minus T₀+Y against the 5s post-failover Cypher
budget that `TestPostFailoverCypherWorks` validates.

### 5.b File a chaos-test gap if the failure mode was not covered

If this incident's failure mode was NOT exercised by an existing chaos
test, file a ticket to add coverage. The current chaos test surface is:

- `TestKillPrimaryFailoverUnder30s` — SIGKILL the leader, assert <30s
  failover under load. Validates D-001.15.
- `TestPostFailoverCypherWorks` — same setup, assert AGE works on the
  new Leader within 5s. Validates A1 + A8 (Pitfall 9).
- `TestReplicaLagUnderLoad` — 1K writes/sec for 5min, assert max replay
  lag <10s. Validates the read-path freshness invariant.

The chaos suite runs nightly via
`.github/workflows/load-chaos-restore.yml`; ad-hoc local invocation:

```bash
# Just the kill-primary failover test.
go test -tags chaos -run TestKillPrimaryFailoverUnder30s -timeout 5m ./tests/chaos/...

# All chaos tests.
go test -tags chaos -timeout 30m -count=1 ./tests/chaos/...

# 5-minute replication lag test.
REPLICATION=1 go test -tags integration -timeout 10m \
    -run TestReplicaLagUnderLoad ./tests/integration/...
```

Test gaps to consider:
- Network partition between Patroni and etcd (vs. process kill — different
  recovery profile).
- etcd member loss (currently no test for etcd quorum-recovery).
- Slow standby (large `replay_lag` at the moment of kill — does Patroni
  pick the right candidate?).
- Multiple-kill (kill the leader, then immediately kill a replica —
  does the cluster recover with only one healthy node?).

---

## 6. Appendix: HA cluster topology reference

```
                    +------------------+
                    |     HAProxy      |
                    |  :5000 primary   |
                    |  :5001 replicas  |
                    |  :7000 stats     |
                    +--------+---------+
                             |
                             v
              +---------+----------+----------+
              |         |          |          |
         +----v----+ +--v---+  +--v---+   +--v-----+
         | pg-node | | pg-  |  | pg-  |   | (HAProxy
         |   -1    | |node-2|  |node-3|    health
         | :5432   | | :5432|  | :5432|    checks
         | :8008   | | :8008|  | :8008|    against
         +----+----+ +--+---+  +--+---+    /master)
              |         |          |
              +---------+----------+
                        |
                        v
              +---------+----------+
              |   etcd 3-node DCS  |
              | etcd-1 etcd-2      |
              |        etcd-3      |
              +--------------------+
```

Each pg-node-N runs:
- Postgres 16 + AGE 1.6.0 (`apache/age:release_PG16_1.6.0` base image)
- Patroni 4.1+ daemon (manages Postgres lifecycle, owns leader election)
- pgBackRest stub (Plan 00-04 will fill `archive_command`)

Patroni REST API on each node (port 8008):
- `GET /master` — 200 iff this node is the Leader
- `GET /replica` — 200 iff this node is a healthy follower
- `GET /cluster` — JSON view of all members (used by `patronictl list`
  and by the Plan 00-03 chaos test's poll loop)
- `GET /health` — 200 iff Postgres on this node is healthy

---

## 7. Escalation

| Symptom | Escalate to | Why |
|---------|-------------|-----|
| etcd quorum loss (< 2 healthy etcd nodes) | Infrastructure team | Patroni cannot function without etcd quorum; rebuilding etcd is an infra-team task |
| Split-brain (2 nodes claim Leader) | DBA + Infrastructure | The `synchronous_mode: true` + etcd quorum should prevent this; if observed, surface as critical bug |
| A1 failure mode after §4 recovery | DBA + Phase 0 lead | Configuration-drift root-cause analysis; `shared_preload_libraries` drift is a deployment hygiene problem |
| Failover took >30s | Phase 0 lead | D-001.15 contract violation; file as engineering bug; review chaos test results for trend |

---

## Cross-references

- `docs/decisions/D-W2-ha-tool.md` — Patroni-vs-pg_auto_failover decision (LOCKED)
- `infra/patroni/patroni.yml.template` — Patroni config template
- `infra/postgres/postgresql.ha.conf` — Postgres HA-mode config
- `infra/docker-compose.ha.yml` — local 3-node cluster for chaos testing
- `tests/chaos/patroni_chaos.go` — chaos library (StartCluster / KillPrimary / WaitForNewLeader / TimeFailover / StopCluster)
- `tests/chaos/failover_test.go` — `TestKillPrimaryFailoverUnder30s` + `TestPostFailoverCypherWorks` (`//go:build chaos`)
- `tests/integration/replication_test.go` — `TestReplicaLagUnderLoad` (`//go:build integration`)
- `.planning/phases/00-metadata-graph-foundation/00-RESEARCH.md` — §Pitfall 9 (A1) + §Patroni Configuration sketch
- `PROJECT.md` D-001.15 — <30s failover SLO
