# Runbook: Phase 0 From-Scratch Deploy

**Owner:** Phase 0 SRE / first-on-call
**Scope:** Operator deploys Neksur from a clean three-node host set;
Patroni cluster comes up green; Cypher round-trip from app host
succeeds.
**Closes:** `00-VALIDATION.md` §Manual-Only Verifications row 1
("Operator deploys Neksur from scratch and Patroni cluster comes up
green"). Maps to ROADMAP.md Phase 0 §Success Criteria #1.

> **Per `D-PHASE0-stack` (PROJECT.md, 2026-05-13) Phase 0 ships ONLY:**
> Postgres 16 + Apache AGE 1.6.0 + Patroni 4.1+ + etcd 3.5+ + HAProxy +
> pgBackRest 2.50+ + chrony + the Neksur Go monorepo + the OTel
> observability stack. Any other service is a Phase 2+ deliverable;
> see `runbooks/phase0-deploy.md` "Forbidden additions" section.

---

## 1. Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **Host inventory** | 3 Postgres+AGE+Patroni nodes + 3 etcd nodes (can be co-located on the same 3 boxes for Phase 0) + 1 HAProxy node + 1 OTel collector host (can be co-located with HAProxy) | Sketch as a deploy diagram; capture in commissioning ticket. |
| **OS** | Postgres-16-compatible Linux (Debian 12 / Ubuntu 22.04 / RHEL 9 / Rocky 9) | `cat /etc/os-release` |
| **Docker** | 24.0+ | `docker --version` |
| **Go** | **Go 1.24+** (required for the Go-based DR writer + load CLIs from Plans 00-04 and 00-06) | `go version` |
| **Disk** | ≥250 GB on each Postgres host (PGDATA + WAL + scratch); ≥500 GB on the pgBackRest repo host | `df -h /var/lib/postgresql /var/lib/pgbackrest` |
| **Network** | All Patroni/etcd/HAProxy/OTel hosts reach each other on the canonical ports (Postgres 5432, Patroni REST 8008, etcd client 2379, etcd peer 2380, HAProxy 5000+5001, OTel gRPC 4317) | `nc -zv <peer> <port>` |
| **Secrets** | `PAGERDUTY_INTEGRATION_KEY`, `SLACK_WEBHOOK_URL`, `PGBACKREST_REPO1_S3_KEY` etc. exposed via `${env:VAR}` placeholders consumed by alertmanager.yml + pgbackrest.conf | Ops vault entry; do NOT inline secrets into the runbook |

If any prereq fails, **HALT** and file a procurement / config ticket;
do not proceed.

---

## 2. Clone repo + checkout Phase 0 tag

```bash
git clone git@github.com:neksur-com/neksur.git /opt/neksur-core
cd /opt/neksur-core
git checkout phase-0-acceptance   # tag created on /gsd-verify-work green
go version    # MUST report >= go1.24
go build ./...   # MUST exit 0
```

If `go build ./...` fails on the deploy host, the Go toolchain is too
old or the host is missing a system library; HALT.

---

## 3. Provision etcd cluster

Reference: `infra/etcd/etcd.yml.template` + `infra/etcd/systemd/`.

For each of the three etcd hosts:

```bash
sudo mkdir -p /etc/etcd /var/lib/etcd
sudo cp infra/etcd/etcd.yml.template /etc/etcd/etcd.yml
sudo $EDITOR /etc/etcd/etcd.yml   # set name, data-dir, listen URLs, initial-cluster
sudo cp infra/etcd/systemd/etcd.service /etc/systemd/system/etcd.service
sudo systemctl daemon-reload && sudo systemctl enable --now etcd
```

**Validate cluster green** (run from any etcd node):

```bash
ETCDCTL_API=3 etcdctl --endpoints=http://etcd-1:2379,http://etcd-2:2379,http://etcd-3:2379 \
  endpoint health
# All three endpoints report "is healthy"
```

If any endpoint is unhealthy, check `journalctl -u etcd -n 100` and
re-resolve before proceeding to Patroni — Patroni's leader election
requires etcd quorum.

---

## 4. Deploy Postgres + AGE + Patroni nodes

Reference: `infra/postgres/Dockerfile` + `infra/postgres/postgresql.base.conf`
+ `infra/postgres/postgresql.ha.conf` + `infra/patroni/patroni.yml.template`
+ `infra/patroni/patroni-pg-node-{1,2,3}.yml` + `infra/patroni/systemd/`.

Build the Postgres+AGE image once on a build host:

```bash
docker build -t neksur/postgres-age:phase0 -f infra/postgres/Dockerfile .
# Pushes apache/age:release_PG16_1.6.0 base + pgaudit + pg_stat_statements
```

For each of the three Postgres nodes:

```bash
sudo mkdir -p /var/lib/postgresql/data /etc/patroni
# Copy the per-node Patroni config (1, 2, or 3 — DO NOT mix; nodes have
# distinct rest_api / postgres listen addresses)
sudo cp infra/patroni/patroni-pg-node-${NODE_NUM}.yml /etc/patroni/patroni.yml
sudo $EDITOR /etc/patroni/patroni.yml   # set etcd hosts, replication user secret
sudo cp infra/patroni/systemd/patroni.service /etc/systemd/system/patroni.service
sudo systemctl daemon-reload && sudo systemctl enable --now patroni
```

**Validate Patroni cluster green** (from any Patroni node — `patronictl list` is the canonical health summary):

```bash
patronictl -c /etc/patroni/patroni.yml list
# Equivalent shorthand: `patronictl list` (when PATRONICTL_CONFIG_FILE env-var is set).
# Expected output:
# +-----------+-------------+--------------+--------+----+-----------+
# | Member    | Host        | Role         | State  | TL | Lag in MB |
# +-----------+-------------+--------------+--------+----+-----------+
# | pg-node-1 | 10.x.x.1    | Leader       | running|  1 |           |
# | pg-node-2 | 10.x.x.2    | Replica      | running|  1 |         0 |
# | pg-node-3 | 10.x.x.3    | Replica      | running|  1 |         0 |
# +-----------+-------------+--------------+--------+----+-----------+
```

If any node is `stopped` / `start failed` / `Lag > 1MB`, check
`journalctl -u patroni -n 200` on the affected node and re-resolve
before proceeding.

---

## 5. Configure pgBackRest stanza

Reference: `infra/pgbackrest/pgbackrest.conf.template`
+ `infra/pgbackrest/sizing.md` (A7 sizing rationale)
+ `infra/pgbackrest/crontab` + `infra/pgbackrest/systemd/`.

On the pgBackRest repo host:

```bash
sudo apt-get install -y pgbackrest    # ≥ 2.50 required
sudo mkdir -p /var/lib/pgbackrest /etc/pgbackrest
sudo cp infra/pgbackrest/pgbackrest.conf.template /etc/pgbackrest/pgbackrest.conf
sudo $EDITOR /etc/pgbackrest/pgbackrest.conf   # set repo path, S3 creds, archive-async=y
sudo -u postgres pgbackrest --stanza=neksur stanza-create
sudo -u postgres pgbackrest --stanza=neksur check
# Expected: WARN/INFO; no ERROR.

# Wire the systemd timers + crontab for the 15-min RPO contract.
sudo cp infra/pgbackrest/systemd/*.service /etc/systemd/system/
sudo cp infra/pgbackrest/systemd/*.timer   /etc/systemd/system/
sudo cp infra/pgbackrest/crontab /etc/cron.d/pgbackrest-neksur
sudo systemctl daemon-reload
sudo systemctl enable --now pgbackrest-incr.timer pgbackrest-diff.timer pgbackrest-full.timer
```

**Validate pgBackRest emits a backup**:

```bash
sudo -u postgres pgbackrest --stanza=neksur info
# Expected: at least one full backup; archive-pull active; queue-max ≥ 4GB.
```

If the `info` output shows no backups after 15 minutes, check
`journalctl -u pgbackrest-* -n 100` and re-resolve. The 15-min RPO
contract (D-OQ.04) DEPENDS on continuous archive-push.

---

## 6. Start observability stack

Reference: `infra/otel/docker-compose.observability.yml`
+ `infra/otel/otel-collector-config.yaml`
+ `infra/prometheus/` (alert rule files from Plan 00-01)
+ `infra/chrony/chrony.conf` (clock-skew per Plan 00-05).

On the observability host:

```bash
cd /opt/neksur-core/infra/otel
export PAGERDUTY_INTEGRATION_KEY=<from-vault>
export SLACK_WEBHOOK_URL=<from-vault>
docker compose -f docker-compose.observability.yml up -d
docker compose -f docker-compose.observability.yml ps
# Expected: 4 services running — otel-collector, prometheus, alertmanager, chrony-exporter
```

On each Postgres host (chrony daemon must run on every node where the
Cypher-emitted clock-skew metric will be sampled):

```bash
sudo apt-get install -y chrony
sudo cp /opt/neksur-core/infra/chrony/chrony.conf /etc/chrony/chrony.conf
sudo systemctl enable --now chronyd
chronyc sources -v   # MUST report at least one synced source
```

**Validate observability live**:

```bash
# Prometheus targets all up:
curl -s http://prometheus:9090/api/v1/targets | jq '.data.activeTargets[] | {job, health}'
# Expected: all "health":"up"

# AlertManager rules loaded:
curl -s http://prometheus:9090/api/v1/rules | jq '.data.groups[].rules[].name'
# Expected list MUST include: CypherP99Breach, ClockSkewWarn, ClockSkewPage
```

---

## 7. Run migrations

Reference: `infra/migrations/run-migrations.sh`
+ `migrations/postgres/V0001__enable_extensions.sql`
+ `migrations/graph/V0010__create_graph_and_labels.sql`
+ `migrations/graph/V0020__property_indexes.sql`
+ `migrations/graph/V0025__tenant_indexes_and_gin.sql`
+ `migrations/postgres/V0030__rls_policies.sql`.

From the deploy host, against the Patroni leader (HAProxy routes you
to it on port 5000):

```bash
export DATABASE_URL="postgresql://neksur_admin:<pass>@haproxy:5000/neksur?sslmode=require"
bash infra/migrations/run-migrations.sh
# Expected: V0001 + V0010 + V0020 + V0025 + V0030 all applied; sqitch
# logs each on stdout. No errors.
```

**Validate schema invariants** (this is the production analogue of
`tests/integration/schema_test.go::TestAllLabelsPresent`):

```bash
psql "$DATABASE_URL" -c "
  SELECT count(*) FROM ag_catalog.ag_label
   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
     AND name NOT LIKE E'\\\\_ag\\\\_label\\\\_%' ESCAPE E'\\\\';
"
# Expected: 43 rows (19 vlabels + 24 elabels per D-001.05/.06 + D-003.06)
```

---

## 8. Smoke test

```bash
# Patroni cluster green (`patronictl list` shorthand assumes PATRONICTL_CONFIG_FILE is set):
patronictl -c /etc/patroni/patroni.yml list
# Leader + 2 replicas, all "running", lag ≤ 1MB.

# Cypher round-trip from the app host (via HAProxy):
go run ./cmd/neksur-cli -- "MATCH (n) RETURN count(n)"
# OR direct psql:
psql "$DATABASE_URL" -c "
  LOAD 'age';
  SET search_path = ag_catalog, \"\$user\", public;
  SELECT * FROM cypher('neksur', \$\$ MATCH (n) RETURN count(n) \$\$) AS (result agtype);
"
# Expected: 0 (no data seeded yet) — but NO error.

# Prometheus targets healthy:
curl -s http://prometheus:9090/api/v1/targets | jq '[.data.activeTargets[] | select(.health != "up")] | length'
# Expected: 0

# AlertManager rules loaded:
curl -s http://prometheus:9090/api/v1/rules | jq '[.data.groups[].rules[].name] | sort'
# Expected: ["ClockSkewPage", "ClockSkewWarn", "CypherP99Breach", ...]
```

---

## 9. PASS / FAIL Checklist

| # | Check | Pass criterion | Plan reference |
|---|-------|----------------|----------------|
| 1 | etcd quorum green | All three endpoints healthy | Section 3 |
| 2 | Patroni cluster green | Leader + 2 replicas running, lag ≤ 1MB | Section 4 |
| 3 | pgBackRest stanza healthy | `info` shows ≥1 backup; archive-pull active | Section 5 |
| 4 | Observability stack live | Prometheus targets up; AlertManager rules loaded | Section 6 |
| 5 | Migrations applied | 19 vlabels + 24 elabels in `ag_label` | Section 7 |
| 6 | Cypher round-trip | `MATCH (n) RETURN count(n)` returns 0 with no error | Section 8 |
| 7 | Chrony running on all Postgres nodes | `chronyc sources -v` reports synced source on every node | Section 6 (per-node) |

If all 7 pass, the deploy is **green**; record the run in
`runbooks/deploy-reports/deploy-report-YYYYMMDD.md` and notify on-call.

If any check fails, capture the failing component's logs (`journalctl
-u <service> -n 200`) and either:

- (a) re-resolve the underlying issue and re-run from the failing
  section, OR
- (b) raise a Sev-2 incident if the failure is a contract violation
  (e.g., labels count ≠ 43, Cypher round-trip errors, archive-push
  not active) — these are gating blockers that prevent the deploy
  from going to production.

---

## Forbidden additions

The following are explicitly **NOT permitted** in a Phase 0 deploy
without an ADR amendment:

- Any **graph engine sidecar** (Memgraph, Neo4j, JanusGraph, Dgraph,
  ArangoDB) — Memgraph cutover is a Phase 2 D-001.10/.12 trigger,
  not a Phase 0 component.
- Any **auxiliary datastore** (Redis, MongoDB, Elasticsearch, Kafka)
  — Phase 0 is Postgres-only per REQ-NFR-graph-ops-footprint;
  asynchronous queueing belongs to Phase 1+.
- Any **surprise Postgres extension** beyond {plpgsql, age, pgaudit,
  pg_stat_statements} — pgvector / postgis / TimescaleDB / etc.
  require an ADR before Phase 6.
- Multiple **non-template databases** within the same Postgres
  cluster — the per-tenant database mode is deferred to Phase 7
  (and partly to Phase 0.5 Pool A schema-per-tenant per ADR-004).

The deployment-inventory test
(`tests/integration/deployment_test.go`, build-tag `integration`)
asserts these invariants automatically; running

```bash
go test -tags integration -run TestServiceInventory ./tests/integration/...
go test -tags integration -run TestNoUnexpectedExtensions ./tests/integration/...
go test -tags integration -run TestNoUnexpectedDatabases ./tests/integration/...
```

against the deployed cluster is part of the green-deploy criteria.

---

*Phase 0 deploy runbook — closes `00-VALIDATION.md` Manual-Only
Verification row 1. Updated 2026-05-13 by Plan 00-06 Wave 5 Task 3.*
