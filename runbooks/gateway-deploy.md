# Runbook: Phase 1 L1 Catalog Gateway Deploy

**Owner:** Phase 1 SRE / first-on-call
**Scope:** Deploy / scale / roll back the Neksur L1 Catalog Gateway —
the per-commit Iceberg REST proxy that enforces P1/P2/P3 CEL policies
and emits the D-003.06 audit graph + `audit_log` row in the same tx.
**Closes:** Phase 1 acceptance criterion §5
(REQ-write-l1-catalog-gateway). Maps to ROADMAP.md Phase 1
§Success Criteria #3 (commit overhead ≤5%).

> **Per `D-1.09` (ADR-003 + 01-CONTEXT line 174 fail-closed mitigation)**
> the gateway runs in an HA topology — **≥2 EC2 replicas behind ALB**.
> A single-replica deployment is acceptable for staging tenants only;
> production tenants MUST have ≥2 replicas so a single fail-closed
> replica does not page on every commit.

> **Per `D-1.10` (in-process detection pool)** the L3 detection workers
> live in the SAME `cmd/neksur-server` process as the gateway; the
> `NEKSUR_L3_WORKERS` env (default `4`) caps the goroutine pool — this
> is documented here so operators understand the gateway and the
> detection worker share a CPU/memory budget per replica.

---

## 1. Topology

```
                              ┌────────────────────────────┐
                              │ AWS ALB (TLS termination)  │
                              │ Target group health check: │
                              │   GET /readyz (NEW Phase 1)│
                              └─────────────┬──────────────┘
                                            │
                  ┌─────────────────────────┼─────────────────────────┐
                  │                         │                         │
        ┌─────────▼─────────┐     ┌─────────▼─────────┐     ┌─────────▼─────────┐
        │ EC2 replica 1     │     │ EC2 replica 2     │ ... │ EC2 replica N     │
        │ (cmd/neksur-server)│    │                   │     │                   │
        │ ┌───────────────┐ │     │                   │     │                   │
        │ │L1 gateway     │ │     │  (identical)      │     │                   │
        │ │/v1/iceberg/*  │ │     │                   │     │                   │
        │ ├───────────────┤ │     │                   │     │                   │
        │ │L3 detection   │ │     │                   │     │                   │
        │ │goroutine pool │ │     │                   │     │                   │
        │ │NEKSUR_L3_     │ │     │                   │     │                   │
        │ │WORKERS=4      │ │     │                   │     │                   │
        │ └───────────────┘ │     │                   │     │                   │
        └─────────┬─────────┘     └─────────┬─────────┘     └─────────┬─────────┘
                  │                         │                         │
                  └─────────────────────────┼─────────────────────────┘
                                            │
                          ┌─────────────────┴─────────────────┐
                          │                                   │
              ┌───────────▼────────────┐         ┌────────────▼────────────┐
              │ Phase 0.5 Pool A       │         │ SQS queue (S3 events)   │
              │ Postgres 16 + AGE 1.6  │         │ Phase 1 Plan 01-07      │
              │ pgxpool with BeforeAcq │         │ Consumed COMPETITIVELY  │
              │   DISCARD ALL + restore│         │ across replicas; only   │
              │   search_path. ONE pool│         │ ONE replica wins each   │
              │   per replica. CC3.    │         │ msg (V0062 UNIQUE       │
              │                        │         │ enforces cross-replica  │
              │                        │         │ dedup — Pitfall 10).    │
              └────────────────────────┘         └─────────────────────────┘
```

- **ALB:** TLS termination; routes all `POST /v1/iceberg/*`
  + `POST /v1/iceberg/{prefix}/transactions/commit` + `POST /v1/lineage`
  + `POST /v1/webhooks/polaris` to the gateway target group. ALB
  health-check probe uses `GET /readyz` (NEW Phase 1, see §3) so that
  policy-engine-unavailable replicas are removed from rotation —
  D-1.09 fail-closed: a broken replica should not see commits.

- **EC2 replicas:** Minimum **2** for production tenants
  (D-1.09 — HA topology is the operational mitigation for fail-closed
  semantics). Each replica runs a single `cmd/neksur-server` binary
  (Phase 0.5 + Plan 01-06 wiring); the gateway, detection pool,
  Polaris webhook handler, and OpenLineage receiver are co-resident
  inside the same process. Auto-Scaling Group sized 2 → 4 (see §4).

- **Pool A Postgres + AGE:** the single canonical pgxpool wired in
  `cmd/neksur-server/runWithSaasAuth()` — `pgxpool.NewWithConfig` with
  `graph.WithBeforeAcquireDiscardAll(cfg)` applied and
  `cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec`.
  **NEVER add a second pool inside the gateway** (CC3 — see §7
  Forbidden additions). Each replica opens its own pool to the shared
  RDS cluster — the BeforeAcquire `DISCARD ALL` is the load-bearing
  session-bleed prevention.

- **In-process L3 detection pool (D-1.10):** the gateway and the
  detection workers share the replica's memory + CPU budget. Sizing
  rule of thumb: gateway is I/O-bound (waits on upstream Polaris +
  Postgres); detection is CPU-bound (regex over column names + future
  Phase 6 ML). At `NEKSUR_L3_WORKERS=4`, each replica reserves ~4
  goroutines for detection scans triggered by the 30s poller +
  Polaris webhook + SQS S3 events (Plan 01-07).

- **SQS (S3 events queue):** Shared across replicas; messages are
  consumed competitively (the first replica to call
  `ReceiveMessage`+`DeleteMessage` wins). The V0062 `detection_runs`
  table's UNIQUE constraint on `snapshot_metadata_location` enforces
  cross-replica dedup at the DB layer — Pitfall 10 (see Plan 01-07
  SUMMARY).

---

## 2. Env vars required

| Var | Default | Required | Notes |
|-----|---------|----------|-------|
| `DATABASE_URL` | — | YES | Pool A RDS DSN; `pgxpool.NewWithConfig` clone with `BeforeAcquire DISCARD ALL` applied. |
| `NEKSUR_SAAS_AUTH` | `0` | YES (set to `1` in prod) | Enables `runWithSaasAuth` — the gateway wiring lives inside this branch. |
| `NEKSUR_OBSERVABILITY` | `0` | YES (set to `1` in prod) | Enables OTLP trace exporter + Prometheus `/metrics` on `:9100`. |
| `NEKSUR_METRICS_ADDR` | `:9100` | no | Override metrics listener (must match the `infra/prometheus/prometheus.yml` `neksur-graph` scrape target). |
| `NEKSUR_LISTEN_ADDR` | `:8080` | no | HTTP listener for the gateway + admin routes. |
| `WORKOS_API_KEY` | — | YES | Phase 0.5 — `workosauth.NewClient` required for `TenantMiddleware`. |
| `WORKOS_CLIENT_ID` | — | YES | Phase 0.5 (same). |
| `WORKOS_WEBHOOK_SECRET` | — | YES | Phase 0.5 (same). |
| `WORKOS_INTERNAL_ADMIN_ORG_ID` | — | YES (for `/admin/*`) | Phase 0.5 Plan 00.5-05. |
| `NEKSUR_L3_WORKERS` | `4` | no | Plan 01-07 D-1.10 — goroutine-pool size for the in-process L3 detection workers. Raise to 8 if `commit_rejected_total` rate is healthy but detection lag (Plan 01-07 trigger queue depth) builds up. |
| `NEKSUR_POLARIS_WEBHOOK_ENABLED` | `1` | no | Set `=0` to return 410 Gone on `POST /v1/webhooks/polaris` when the upstream Polaris instance lacks webhook signing (Plan 01-07 Open Question 2 escape hatch). |
| `NEKSUR_S3_EVENTS_QUEUE_URL` | (unset) | no | Plan 01-07 — when set, starts the SQS long-poll goroutine. Format: `https://sqs.<region>.amazonaws.com/<acct>/<queue>`. Leaving unset is supported (only the 30s poller + Polaris webhook fire). |
| `NEKSUR_S3_EVENTS_TENANT_ID` | (unset) | conditional | REQUIRED if `NEKSUR_S3_EVENTS_QUEUE_URL` is set. Phase 1 simplification: one queue per tenant. |
| `NEKSUR_SLACK_WEBHOOK_URL` | (unset) | no | Plan 01-07 — when unset, `alerts.Slack.Post` is a silent no-op so the gateway can ship before Slack is configured. Set to `https://hooks.slack.com/services/...` to enable L3-finding alerts at confidence ≥ 0.85. |
| `STRIPE_WEBHOOK_SECRET` | — | YES (when `BILLING_ENABLED=true`) | Phase 0.5 — verified BEFORE `BILLING_ENABLED` check (D-0.5.21 T-0.5-stripe-spoof). |
| `PAGERDUTY_SERVICE_ID` | `P000000` | no | Phase 0.5 — admin UI embed. |
| `AWS_REGION` | — | YES (when `NEKSUR_S3_EVENTS_QUEUE_URL` set OR for CloudWatch metric posting) | `aws-sdk-go-v2/config` `LoadDefaultConfig`. |

---

## 3. Health-check endpoints

Two endpoints — operators MUST wire the ALB target group health check
to `/readyz`, not `/healthz`, so policy-engine-unavailable replicas
are pulled from rotation (D-1.09 fail-closed):

| Endpoint | Phase | Returns 200 iff … | Use for |
|----------|-------|-------------------|---------|
| `GET /healthz` | Phase 0.5 (existing) | pgxpool is reachable AND a `SELECT 1` against AGE LOAD succeeds | Liveness probe — ASG instance-replacement trigger |
| `GET /readyz` | **Phase 1 (NEW)** | `/healthz` AND CEL compiler initialised AND detection pool started | ALB target group health check — D-1.09 routing |

The `/readyz` distinction is load-bearing: when the CEL compiler env
fails to initialise (cel-go bootstrap error — extremely rare but
possible if a binary is corrupted), the replica's `/healthz` still
returns 200 (Postgres is fine) but `/readyz` returns 503. The ALB
removes the replica from rotation; ASG eventually replaces it. The
sibling replica handles 100% of commit traffic without paging on
fail-closed 503s.

**ALB target-group config:**

```hcl
health_check {
  enabled             = true
  path                = "/readyz"   # NOT /healthz — D-1.09 routing
  protocol            = "HTTP"
  matcher             = "200"
  interval            = 15
  timeout             = 5
  healthy_threshold   = 2
  unhealthy_threshold = 2
}
```

---

## 4. Scaling triggers

The gateway is I/O bound (most time spent waiting on upstream Polaris
+ Postgres); the dominant scaling signal is **commit_rejected_total
rate**, NOT CPU:

| Metric | Threshold | ASG action | Why |
|--------|-----------|------------|-----|
| `sum(rate(commit_rejected_total{reason="policy_engine_unavailable"}[1m])) > 0` sustained 30s | trigger | scale-out 2 → 4 replicas | A replica that's fail-closed-503-ing should be replaced; ALB pulls it via `/readyz`, ASG launches a fresh one. Concurrent replicas absorb the traffic during the swap. |
| `histogram_quantile(0.95, sum(rate(neksur_gateway_commit_duration_ms_bucket[5m])) by (le)) > 250` | trigger | scale-out +1 replica | P95 commit latency >250ms while baseline is <50ms suggests upstream Polaris backpressure or detection-pool CPU contention. |
| Replica count < 2 for any reason | page | manual investigation | D-1.09 contract violation — production tenants MUST have ≥2 replicas. |

When `PolicyEngineUnavailableAlert` fires (see
`runbooks/commit-rejected-503-rate.md`), SRE is paged within 1 min
(severity=page); the ASG scale-out runs in parallel.

---

## 5. Deploy procedure (blue-green via ASG instance refresh)

The gateway uses **rolling blue-green per replica** — one replica at
a time; the new replica MUST pass `/readyz` before the old replica is
deregistered. This guarantees ≥2 replicas during the entire deploy
window (D-1.09 contract preserved).

### 5.1 Build + push AMI

```bash
# From a build host:
git clone git@github.com:neksur-com/neksur-core.git /opt/neksur-core
cd /opt/neksur-core
git checkout phase-1-release-vX.Y.Z     # tag created by /gsd-verify-work green
go build -o /usr/local/bin/neksur-server ./cmd/neksur-server
go version    # MUST report >= go1.24

# Bake the AMI (Packer or HashiCorp Image Builder):
packer build infra/packer/neksur-gateway.pkr.hcl
# → ami-XXXXX for the production region
```

### 5.2 Trigger ASG instance refresh

```bash
aws autoscaling start-instance-refresh \
  --auto-scaling-group-name neksur-gateway-prod \
  --strategy Rolling \
  --preferences '{
    "MinHealthyPercentage": 50,
    "InstanceWarmup": 60,
    "CheckpointPercentages": [50, 100],
    "CheckpointDelay": 120
  }'
```

The `InstanceWarmup: 60` value gives `/readyz` 60s after launch to
flip green; the gateway typically takes ~10-15s to warm the CEL
compiler cache + AGE pool but the buffer absorbs cold-start variance.

### 5.3 Verify mid-deploy

While the ASG is refreshing, run from an operator host with VPN access:

```bash
# Watch /readyz on each replica behind the ALB:
for i in 1 2 3 4; do
  curl -sS -o /dev/null -w "%{http_code}\n" \
    https://app.neksur.com/readyz
done
# Expect: all 200. Any 503 indicates a replica failed to warm.

# Watch the commit_rejected_total counter:
curl -sS https://app.neksur.com:9100/metrics | \
  grep '^commit_rejected_total'
# Expect: rate is flat; baseline should not spike during instance refresh.
```

If `/readyz` returns 503 from any replica for >2 minutes after launch,
**ABORT** the instance refresh (`aws autoscaling
cancel-instance-refresh`) and follow §5.4 rollback.

### 5.4 Rollback procedure

```bash
# 1. Cancel any in-progress instance refresh:
aws autoscaling cancel-instance-refresh \
  --auto-scaling-group-name neksur-gateway-prod

# 2. Revert the launch template to the last-known-good AMI:
aws ec2 create-launch-template-version \
  --launch-template-name neksur-gateway-lt \
  --source-version <last-good-version> \
  --version-description "Rollback from vX.Y.Z"

aws autoscaling update-auto-scaling-group \
  --auto-scaling-group-name neksur-gateway-prod \
  --launch-template "LaunchTemplateName=neksur-gateway-lt,Version=$LATEST"

# 3. Trigger a fresh rolling refresh against the reverted AMI:
aws autoscaling start-instance-refresh \
  --auto-scaling-group-name neksur-gateway-prod \
  --strategy Rolling
```

Document the failed deploy in
`runbooks/deploy-reports/gateway-deploy-YYYYMMDD.md` and link to the
Sev-2 incident.

---

## 6. Smoke test

After ASG refresh completes, run from an operator host (VPN or
session-cookie auth required):

```bash
# 1. /readyz across the fleet — all replicas should return 200.
for i in {1..10}; do
  curl -sS -o /dev/null -w "%{http_code} " https://app.neksur.com/readyz
done; echo
# Expect: "200 200 200 200 200 200 200 200 200 200"

# 2. Single-table commit smoke — health-probe table; allow-all policy seeded.
curl -X POST https://app.neksur.com/v1/iceberg/test-polaris/namespaces/health/tables/probe \
  -H "Cookie: neksur_session=<test-session-token>" \
  -H "Content-Type: application/json" \
  -d '{"requirements":[],"updates":[]}'
# Expect: 200 + JSON CommitResult body
#  - OR 403 if a deny-all health-probe policy is in place (also healthy)
#  - 503 indicates fail-closed — see runbooks/commit-rejected-503-rate.md
#  - 401 indicates session-token rotation needed
#  - 404 indicates the health-probe table not seeded; provision via
#    `neksur-cli tenant smoke` (Phase 0.5 D-0.5.19).

# 3. Counter rate sanity check:
curl -sS https://app.neksur.com:9100/metrics | \
  grep -E '^(commit_rejected_total|cypher_duration_ms_bucket)' | head -20
# Expect: cypher_duration_ms_bucket buckets monotonically increase;
#  commit_rejected_total{reason="policy_engine_unavailable"} flat.

# 4. /v1/webhooks/polaris reachable (if NEKSUR_POLARIS_WEBHOOK_ENABLED=1):
curl -sS -X POST https://app.neksur.com/v1/webhooks/polaris \
  -H "Content-Type: application/json" \
  -d '{}' \
  -w "%{http_code}\n"
# Expect: 401 (missing HMAC signature) — proves the route is mounted
# and HMAC verification is active. NOT 410 (which would mean the
# handler is disabled per NEKSUR_POLARIS_WEBHOOK_ENABLED=0).
```

If any smoke step fails, capture the response + log lines
(`journalctl -u neksur-server -n 200` on the affected replica) and
either re-resolve or roll back per §5.4.

---

## 7. Forbidden additions

The following are explicitly **NOT permitted** in a Phase 1 gateway
deploy without an ADR amendment + CONTEXT line 84 discretion-update:

- **A second pgxpool** inside `cmd/neksur-server` (CC3). The single
  pool with `BeforeAcquire DISCARD ALL` (Plan 00.5-03 + Phase 1
  Plan 01-04) is the load-bearing session-bleed prevention; adding a
  second pool would silently bypass the DISCARD ALL hook for whichever
  code-path uses the new pool. Plan 01-06 wired the gateway to share
  the existing pool — keep that invariant.

- **Replacing `gc.ExecuteInTenant` with raw `pool.Query` for any
  gateway code-path** (CC2). The graph client wraps every AGE call
  with the tenant-RLS `SET LOCAL`-equivalent +
  `LOAD 'age' + SET search_path = ag_catalog, "$user", public` + tx
  boundary. Raw `pool.Query` would bypass all three and silently leak
  across tenants.

- **A policy authoring UI** (web form that lets customers paste CEL
  into the graph) **without ADR-005 cypher-hardening contract Phase 5
  ratification**. CEL policies are operator-curated in Phase 1 — see
  `runbooks/policy-author.md` for the audited authoring workflow via
  `neksur-cli policy compile`. Premature UI surface bypasses the
  compile-dogfood + manual review.

- **Bumping `iceberg-go` past v0.5.0** without an ADR amendment.
  Phase 1 success criterion §3 (commit overhead ≤5%) is benchmarked
  against v0.5.0 (`tests/load/gateway-overhead-baseline.json`); a
  version bump invalidates the baseline and requires a fresh nightly
  CI baseline.

- **Adding a graph engine sidecar** (Memgraph, Neo4j, …). The Phase
  0 forbidden-additions list (`runbooks/phase0-deploy.md` §Forbidden)
  applies verbatim — the D-001.10 graph-engine migration trigger
  evaluation is Phase 2 work.

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

## 8. References

- **D-1.09** ADR-003 §4 — *Fail-closed semantics on policy engine
  failure*. The gateway HA topology (≥2 replicas behind ALB +
  `/readyz`-gated rotation + `PolicyEngineUnavailableAlert` at
  severity=page) is the operational mitigation that makes
  fail-closed safe in production.
- **D-1.10** ADR-003 §5 — *In-process worker pool* for L3 detection.
  `NEKSUR_L3_WORKERS=4` default; CONTEXT line 76 documents Phase 6
  may extract `neksur-l3-worker` binary if scan latency dominates.
- **D-1.11** ADR-003 §5 — *Hybrid trigger sources* (poller + Polaris
  webhook + SQS S3 events) feeding ONE in-process channel with
  `sync.Map` dedup. See Plan 01-07.
- **D-1.12** ADR-003 §6 — *Slack over PagerDuty for L3 detection
  alerts* (dashboard-trend signal not paging). Plan 01-07.
- **CONTEXT line 174** Plan 01 — *Alert routing requirement* for
  the L1 gateway fail-closed path:
  `commit_rejected_total{reason="policy_engine_unavailable"}` rate >0
  sustained 1m → PagerDuty severity=page.
- **ADR-003 §3.3** — *Commit overhead contract* (gateway ≤5% P95
  overhead vs direct Polaris). Validated by
  `tests/load/gateway_overhead.go` + nightly CI per
  `scripts/ci/phase1-gateway-overhead.sh`.
- **Plan 01-06 SUMMARY** — `internal/gateway/iceberg/handler.go`
  10-step pipeline + `audit.go` D-003.06 + Open Q 4 same-tx audit
  emission + `multi_table.go` Pitfall 6 Reject-All.
- **Plan 01-08 SUMMARY** — gateway overhead baseline +
  `scripts/ci/phase1-gateway-overhead.sh` nightly cron driver +
  `tests/load/gateway-overhead-baseline.json` PENDING_FIRST_RUN seed.
- **`runbooks/commit-rejected-503-rate.md`** — incident response
  playbook when `PolicyEngineUnavailableAlert` fires.
- **`runbooks/policy-author.md`** — CEL authoring workflow for
  SecOps + Pitfall 7 mitigation via `neksur-cli policy compile`.

---

*Phase 1 gateway deploy runbook — closes REQ-write-l1-catalog-gateway
acceptance criterion §5. Updated 2026-05-15 by Plan 01-09 Task 1.*
