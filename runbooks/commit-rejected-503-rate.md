# Runbook — `PolicyEngineUnavailableAlert` (commit_rejected_total 503 rate)

**Severity:** PAGE (wakes on-call within 1 minute of breach)
**Contract:** **D-1.09** (ADR-003 §4 fail-closed semantics) +
**CONTEXT line 174** (Plan 01 alert routing — 503 rate >0 sustained
1m pages SRE so customers do not silently lose write access).
**Alert rule:** [`observability/rules/phase1-commit-rejected.yml`](../observability/rules/phase1-commit-rejected.yml)
+ mirror at [`ops/prometheus/alerts/phase1-commit-rejected.yaml`](../ops/prometheus/alerts/phase1-commit-rejected.yaml)
**Source metric:** `commit_rejected_total{reason}` (CounterVec) —
emitted by [`internal/observability/metrics.go`](../internal/observability/metrics.go)
+ incremented by [`internal/gateway/iceberg/handler.go`](../internal/gateway/iceberg/handler.go)
on every 503/403 rejection.
**Verified by Go test:** `TestCommitRejected503MetricAlert` in
[`tests/integration/commit_rejected_503_metric_alert_test.go`](../tests/integration/commit_rejected_503_metric_alert_test.go)
— BLOCKING test asserts metric increments + alert rule expression
parses + matches the synthetic counter value.

---

## What this alert means

The Phase 1 L1 Catalog Gateway is rejecting customer Iceberg commits
with **HTTP 503 (policy-engine-unavailable)** at rate >0 sustained
1m. Per D-1.09 the gateway is fail-closed: when the policy engine
cannot prove a commit is allowed, the commit is REJECTED rather than
allowed-by-default. Customers writing via Spark / Trino / Snowflake
see their commit fail. **They cannot work around this.** The
gateway returns 503 with body `"policy-engine-unavailable"`; the
client treats it as a server-side outage.

Two fail-closed sub-paths both increment
`commit_rejected_total{reason="policy_engine_unavailable"}`:

1. **Policy fetch failure** —
   `store.AGEStore.LoadPoliciesForTable` returned non-nil err.
   Usually a DB transport error (pool exhausted, RDS failover
   in flight, RLS GUC unset because TenantMiddleware not in chain).
2. **Policy evaluation failure** — `cel.Evaluator.Evaluate`
   returned non-nil err. Compile error (rare in steady state since
   policies compile-tested via `neksur-cli policy compile`), eval
   error (typed-Value error inside cel-go), non-bool return, or
   eval-time panic recovered by the D-1.09 `defer` in
   `internal/policy/cel/eval.go`.

The HTTP path emits the SAME counter label for both sub-paths;
operator diagnosis distinguishes them via slog logs (see §1 diagnosis
tree).

A rejection rate >0 sustained 1m is the threshold per CONTEXT line
174 — single transient blips during DB failover should resolve
within seconds; sustained breach indicates a structural problem
(broken policy text, broken pool, broken RLS, broken CEL env).

---

## Triage Setup (one-time)

Before this runbook is useful, the observability stack must be
running with the Phase 1 rule file loaded:

```bash
# Validate the rule file is mounted into Prometheus:
curl -sS http://prometheus:9090/api/v1/rules | \
  jq '.data.groups[].rules[].name' | grep PolicyEngineUnavailableAlert
# Expected: "PolicyEngineUnavailableAlert"

# Validate AlertManager route includes severity=page → pagerduty:
curl -sS http://alertmanager:9093/api/v2/status | \
  jq '.config.route.routes[].matchers'
# Expected: a route matching `severity="page"` routed to pagerduty.
```

If the rule is not loaded, the alert never fires — verify
`infra/prometheus/prometheus.yml` includes
`rule_files: rules/phase1-commit-rejected.yml` (Plan 01-09 Task 2
wires this).

---

## 1. Triage (first 5 minutes)

Run from any operator host with VPN + session-cookie auth:

### 1.1 Check `/healthz` and `/readyz` across the fleet

```bash
for replica in $(aws elbv2 describe-target-health \
    --target-group-arn <gateway-tg-arn> \
    --query 'TargetHealthDescriptions[].Target.Id' --output text); do
  for endpoint in healthz readyz; do
    code=$(curl -sS -o /dev/null -w "%{http_code}" \
      "http://${replica}:8080/${endpoint}")
    echo "$replica $endpoint $code"
  done
done
```

Interpretation:

| `/healthz` | `/readyz` | Meaning | Action |
|------------|-----------|---------|--------|
| 200 | 200 | Replica fully green | Continue diagnosis |
| 200 | 503 | CEL compiler init failed OR detection pool not started | Restart replica via ASG instance refresh (`runbooks/gateway-deploy.md` §5) |
| 500 | 503 | DB unreachable (pgxpool / `SELECT 1` against AGE LOAD failed) | **Page DBA** — RDS / Pool A issue |
| 200 | 200 (some replicas) + 503 (others) | Mixed — ALB pulled bad replicas via `/readyz` (D-1.09 routing working as designed) | Verify the 503 replicas via §1.2; ASG should be replacing them |

### 1.2 Check slog logs on the misbehaving replica

```bash
# Find the replica IPs that are returning 503:
aws elbv2 describe-target-health \
  --target-group-arn <gateway-tg-arn> \
  --query 'TargetHealthDescriptions[?TargetHealth.State==`unhealthy`].Target.Id'

# SSH (via Session Manager) and grep slog:
aws ssm start-session --target i-<id>
# Inside the session:
journalctl -u neksur-server -n 1000 --no-pager | grep -E \
  'policy fetch failed|policy eval failed|cel: eval panic|policy_engine_unavailable'
```

The slog messages distinguish the two D-1.09 sub-paths:

| Log substring | Sub-path | Likely root cause |
|---------------|----------|-------------------|
| `gateway: policy fetch failed (fail-closed)` | (1) Policy fetch | DB transport / RLS / pool exhaustion |
| `gateway: policy eval failed (fail-closed)` | (2) Policy eval | Bad CEL text / runtime type error / panic |

### 1.3 Check the `commit_rejected_total` reason split

```bash
curl -sS http://<replica>:9100/metrics | \
  grep '^commit_rejected_total'
```

Expected output during a healthy steady state:

```
commit_rejected_total{reason="policy_denied"} 12345    # normal 403s
commit_rejected_total{reason="policy_engine_unavailable"} 0  # alert threshold = >0 / 1m rate
```

If `policy_engine_unavailable` is incrementing while `policy_denied`
is flat — the engine is broken; if both are climbing — a recent
policy push may be ill-formed (the `Evaluate` step is failing on
many tables).

---

## 2. Diagnosis tree

Follow the branches in order; stop at the FIRST matching condition.

### A. DB unreachable (page DBA)

```bash
curl -sS http://<replica>:8080/healthz
# Returns 500
```

**Root cause:** pgxpool cannot reach Pool A RDS — either the pool is
exhausted (too many waiting commits), the RDS instance is in failover,
or the AGE `LOAD 'age'` is failing because the binary library is
missing.

**Action:**
1. Page the on-call DBA — RDS / Pool A is the upstream owner.
2. Check `pg_stat_activity` from the DBA side: are there long-running
   transactions blocking the pool?
3. Check CloudWatch RDS metrics for the Pool A cluster — CPU,
   connections, DiskQueueDepth.
4. If RDS failover: wait 60-120s for the replica to be promoted;
   the gateway's pgxpool will reconnect via `BeforeAcquire`.
5. The ALB is already pulling the broken replicas via `/readyz`;
   ASG should be replacing them within 5min.

### B. CEL compiler uninitialised (restart replica)

```bash
curl -sS http://<replica>:8080/healthz     # 200
curl -sS http://<replica>:8080/readyz      # 503
journalctl -u neksur-server | grep 'cel.NewEnv\|cel.NewCompiler'
# Look for: "cel.NewEnv: ..." or "cel.NewCompiler: ..." errors
```

**Root cause:** Extremely rare — typically only happens if the
deployed binary is corrupted or a cel-go version mismatch creeps in
via a botched dependency bump (Phase 1 forbidden — `runbooks/gateway-deploy.md`
§7).

**Action:**
1. Capture the slog stack-trace.
2. Trigger ASG instance refresh to replace the bad replica
   (`runbooks/gateway-deploy.md` §5.2 — typically resolves within
   60s warmup).
3. If multiple replicas exhibit the same error, the AMI may be bad —
   roll back per `runbooks/gateway-deploy.md` §5.4.

### C. CEL eval panic (customer policy malformed — page policy author)

```bash
journalctl -u neksur-server | grep 'cel: eval panic\|gateway: policy eval failed'
# Look for log lines with policy_id="..."
```

Sample log line:

```
{"level":"ERROR","msg":"gateway: policy eval failed (fail-closed)",
  "err":"cel: policy no-pii-orders: cel: eval panic: runtime error: invalid memory address or nil pointer dereference",
  "policy_id":"no-pii-orders"}
```

**Root cause:** A customer- or SecOps-authored CEL expression
references an undefined map key chain that, at runtime, produces a
typed-Value error or nil-pointer panic. cel-go converts plain
panics to ContextEval errors; runtime.Error panics bubble through to
the D-1.09 `defer/recover` in `internal/policy/cel/eval.go` and
return `ErrEvalPanic`. Either way the gateway returns 503.

This SHOULD have been caught by `neksur-cli policy compile`
(`runbooks/policy-author.md` §2.1) but compile-test doesn't exercise
runtime values; some malformed policies pass compile but fail eval.

**Action:**
1. Read the `policy_id` from the slog line. Read the policy text
   from the graph (Phase 1 — manually via Cypher; future
   `neksur-cli policy get`).
2. Identify the SecOps owner (per-tenant policy author — Phase 1
   has SecOps as the policy author per CONTEXT line 84 — until the
   author can be reached, see §3 mitigation).
3. **Page the policy author** (typically the design-partner's SecOps
   contact — surface to the design-partner Slack channel
   `#neksur-design-partner-<tenant>`).
4. Once the policy author is on the line: have them roll back the
   most recent policy edit (delete the offending `Policy` /
   `RetentionPolicy` node from the graph) OR fix the CEL text +
   re-push.
5. The gateway picks up the new policy on the next commit
   (`cel.Compiler.CompileOrGet` cache-key hashes the text — old
   compile ages out).

### D. RLS missing GUC (page Plan 01-06 developer)

```bash
journalctl -u neksur-server | grep 'policy fetch failed' | \
  grep -E 'ErrTenantMissing|current_setting|app.current_tenant'
```

**Root cause:** The `tenant.WithTenantTx` `SET LOCAL` /
`current_setting('app.current_tenant', true)` GUC is unset — usually
because `workosauth.TenantMiddleware` was bypassed (a misconfigured
route mounting the handler outside the middleware) or the middleware
itself silently failed.

This SHOULD be impossible in production — the gateway routes are
all mounted behind `workosauth.TenantMiddleware` in
`cmd/neksur-server/main.go::runWithSaasAuth` per Plan 01-06.

**Action:**
1. Capture the failing request URL from slog.
2. Verify the route is still mounted behind TenantMiddleware:
   ```bash
   grep -E 'workosauth.TenantMiddleware' \
     /opt/neksur-core/cmd/neksur-server/main.go
   # Expect: at least 5 matches (every gateway route + /api/ + /v1/lineage)
   ```
3. If a route is missing the middleware: **Page the developer who
   last touched cmd/neksur-server/main.go** (git blame).
4. Roll back the binary per `runbooks/gateway-deploy.md` §5.4
   while the fix is prepared.

### E. None of the above (rare — collect diagnostics)

If `/healthz` is 200 and `/readyz` is 200 on all replicas AND the
counter is incrementing AND no slog grep finds a clear cause:

```bash
# Capture metric snapshot:
curl -sS http://<replica>:9100/metrics > /tmp/metrics-$(date +%s).txt

# Capture last 5000 slog lines:
journalctl -u neksur-server -n 5000 --no-pager > /tmp/slog-$(date +%s).txt

# Capture pgxpool stats (via the admin /debug/vars endpoint if wired):
curl -sS http://<replica>:8080/debug/vars 2>/dev/null | jq . > /tmp/vars-$(date +%s).json
```

**Action:** Open Sev-2 incident, attach the captured diagnostics,
escalate to Phase 1 platform engineer.

---

## 3. Mitigation

While the diagnosis tree runs, the ALB + ASG combo applies the
following mitigations automatically:

- **Single broken replica** — ALB pulls it from rotation via the
  `/readyz` 503 (D-1.09 routing); ASG launches a replacement; ALB
  re-adds when the new replica's `/readyz` returns 200. Typical
  wall-clock to full recovery: 90s-3min.

- **All replicas broken** — ALB pulls every replica → all customer
  commits 503 (and ALB returns 503 directly). Roll back to the
  last-known-good AMI per `runbooks/gateway-deploy.md` §5.4.

- **Customer-authored policy at fault (sub-path C)** — there is NO
  automatic mitigation; the gateway is correctly fail-closed on
  the malformed policy. SRE coordinates with the policy author to
  delete or fix the policy in the graph; the gateway picks up the
  fix on the next commit (no restart needed). If the policy author
  cannot be reached within 15min and the alert volume is high, an
  emergency operator may delete the offending `Policy` /
  `RetentionPolicy` node from the graph directly via
  `neksur-cli` + `gc.ExecuteInTenant` Cypher DELETE (audit-log
  emission required — every emergency policy deletion MUST file a
  follow-up incident report).

---

## 4. Post-incident

After the alert resolves:

1. **Capture metrics graph** — Prometheus dashboard snapshot of the
   1h preceding + 30min following the alert window. Attach to the
   incident report.

2. **Root-cause classification:**
   - (A/B/D) Phase 1 code or config bug → file a gap-closure plan
     under the Phase 1 phase directory; the bug becomes a Phase 1
     bug-fix plan (not a Phase 2 deliverable).
   - (C) Customer-authored policy bug → surface to the design-partner
     Slack channel; ensure the policy author updated their
     compile-test workflow (`neksur-cli policy compile` was either
     skipped or the policy passed compile-test but failed at eval —
     the latter is a Phase 1 doc-gap).
   - (E) Unknown → escalate to Phase 1 platform engineer; the
     fail-closed semantics worked correctly (customers were not
     silently allowed) but the diagnosis tree needs a new branch.

3. **Update CloudWatch dashboard** with any new metric panel needed
   to catch the same shape next time.

4. **If the alert fires >3 times in 30 days from the same
   sub-path**, file an ADR amendment proposal — fail-closed
   semantics may need a more graceful path (e.g., per-policy
   circuit breaker that surface-marks a policy as "broken" and
   skips it instead of failing the whole commit). This is a
   D-1.09 contract evolution, NOT a runbook fix.

---

## 5. False-positive cases (resolve, don't escalate)

- **RDS failover transient** — During an RDS Multi-AZ failover, the
  pgxpool emits a burst of connection-reset errors for ~30-60s. The
  `for: 1m` requirement on `PolicyEngineUnavailableAlert` filters
  most of these; if the alert DOES fire it auto-resolves once the
  pool reconnects.

- **Replica cold start during deploy** — A fresh replica may briefly
  return 503 between launch and CEL compiler warm-up. The ALB
  `/readyz` health check pulls cold replicas from rotation; this
  should NOT increment `commit_rejected_total` because no commits
  reach a cold replica. If the counter increments anyway,
  investigate via §1.

- **Customer-authored policy with a 1-time eval-time edge case** —
  A policy may pass compile-test AND succeed against 99.9% of
  traffic but fail on a rare commit shape. The first failure pages,
  the policy author fixes the policy, no further failures.

---

## References

- **D-1.09** ADR-003 §4 — fail-closed semantics; the operational
  mitigation contract.
- **CONTEXT line 174** Plan 01 — alert routing requirement that this
  runbook closes.
- **Plan 01-05 SUMMARY** — CEL engine + 4 sentinels (ErrCompileFailed
  / ErrPolicyEvalFailed / ErrPolicyReturnedNonBool / ErrEvalPanic)
  + the d109PanicRecoverAuditAnchor + ReasonPolicyEngineUnavailable
  named constant.
- **Plan 01-06 SUMMARY** — gateway 10-step pipeline (steps 9 + 10
  are the D-1.09 fail-closed paths); audit_log emission requires the
  same-tx contract.
- **Plan 01-07 SUMMARY** — Slack alerts for L3 detection findings
  (separate from this PagerDuty alert; Slack is for trend signal at
  severity=warn, PagerDuty is for fail-closed at severity=page).
- **`runbooks/gateway-deploy.md`** — HA topology, scaling triggers,
  rollback procedure.
- **`runbooks/policy-author.md`** — CEL authoring workflow + Pitfall
  7 mitigation (the customer-side prevention for sub-path C).
- **`runbooks/cypher-latency-breach.md`** — sibling Phase 0 alert
  with the same severity=page + on-call pattern.

---

*Phase 1 commit-rejected 503-rate incident runbook — closes D-1.09
operational mitigation + CONTEXT line 174 alert routing requirement.
Updated 2026-05-15 by Plan 01-09 Task 1.*
