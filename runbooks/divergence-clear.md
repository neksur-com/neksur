# Runbook: Clearing a Divergent-Suspended Engine

**Owner:** SecOps / On-Call SRE
**Scope:** Root-cause investigation steps for a `divergent_suspended` engine; operator
attestation procedure; Cypher `UPDATE CompiledPolicy SET status='active'` clearance
template. Fulfills T-3-divergence-clear-bypass mitigation (Plan 03-15 threat model).
**Closes:** 03-VALIDATION.md Manual-Only Verification §Auto-suspend → operator clear
runbook end-to-end (REQ-write-cross-engine-consistency-verifier).

---

## 1. What Is `divergent_suspended`?

Per Plan 03-11 (D-3.05), the continuous cross-engine consistency verifier (L2 commercial
feature) monitors every registered engine every 5 minutes with synthetic probe queries.
When two engines return byte-different results for the same canonical query on the same
Iceberg table with the same active `CompiledPolicy`, the verifier:

1. Emits `cross_engine_divergence_total{engine=X, table=Y, severity=critical}` metric.
2. Opens a PagerDuty alert (severity=page) + Slack notification.
3. Sets `CompiledPolicy.status = divergent_suspended` for the divergent engine on the
   affected table (the engine that disagrees with the plurality result).
4. The SQL proxy + L1 gateway returns `503 + commit_rejected_total{reason="policy_engine_divergent"}`
   for all queries against the affected table on the suspended engine.

**Important:** Only the divergent engine is suspended — not all engines. The remaining
engines that agree continue serving the table normally. This preserves the cross-engine
guarantee on the surviving engines.

---

## 2. Alert Triage

When the PagerDuty alert fires:

```
[CRITICAL] cross_engine_divergence_total{engine="dremio", table="prod.orders"} incremented
CompiledPolicy.status=divergent_suspended for engine=dremio on table=prod.orders
```

### 2.1 Assess scope

```bash
# Check which engines are currently suspended:
curl -s http://neksur-server:9100/metrics | \
    grep 'compiled_policy_status{.*divergent_suspended.*}'

# Example output:
# compiled_policy_status{engine="dremio", table="prod.orders", status="divergent_suspended"} 1
```

### 2.2 Check the DivergenceEvent log

The verifier writes `DivergenceEvent` vlabel nodes to the AGE graph (Plan 03-11).
Query them via the Neksur graph API or direct Cypher:

```bash
# Via neksur-cli (if available):
neksur-cli divergence-events --engine dremio --table prod.orders --last 5

# Via direct AGE Cypher (ExecuteInTenant pattern):
MATCH (de:DivergenceEvent {engine_kind: 'dremio', table_name: 'prod.orders'})
RETURN de.detected_at, de.engine_count, de.hash_a, de.hash_b, de.query_shape
ORDER BY de.detected_at DESC
LIMIT 5
```

Expected fields on `DivergenceEvent`:
- `detected_at` — timestamp of the first divergence
- `engine_kind` — the engine that was suspended
- `reference_engine` — the engine that agreed with the majority
- `hash_a` / `hash_b` — canonical row hashes that disagreed (Plan 03-11 diff.go)
- `query_shape` — the probe query shape (synthetic or mirrored)
- `policy_id` / `compiled_policy_id` — the affected policy

---

## 3. Root-Cause Investigation Checklist

**Do NOT clear the suspension until you have identified and fixed the root cause.**
Per T-3-divergence-suppression mitigation: the clearance Cypher commit MUST include a
runbook attestation phrase (see §4).

### 3.1 Root-cause categories

| Category | Investigation Step | Typical Fix |
|----------|--------------------|------------|
| **Dialect bug** — engine compiled the row-filter or column-mask SQL fragment differently | Compare `CompiledPolicy.artifact_body` for the suspended engine vs a passing engine | Fix the dialect compiler; re-compile and verify |
| **Version mismatch** — engine version changed and the compiled artifact is stale | Check `Engine.version` in the registry vs the actual engine version | Re-issue the compiled artifact for the new engine version |
| **Probe non-determinism** — the canonical query has a non-deterministic component | Check `DivergenceEvent.query_shape` for `NOW()`, `RAND()`, un-ordered projections | Fix the probe query exclusion ruleset (Plan 03-11 `exclude.go`) |
| **Policy author error** — the row-filter or column-mask expression has a semantic bug | Replay the probe query manually against both engines; diff the results | Fix the policy expression via `neksur-cli policy compile` + re-publish |
| **Engine-side bug** — the Iceberg REST endpoint on the engine returned a different snapshot | Check the Iceberg snapshot ID in the divergence event vs the active snapshot on both engines | Restart/patch the engine; the verifier will auto-resume on next probe |
| **Network partition** — a transient network error caused one engine to return stale data | Check `DivergenceEvent.query_shape == "transient_error"` | Wait for the next probe cycle; auto-recovery expected |

### 3.2 Confirm fix is in place

After identifying the root cause and applying the fix:

```bash
# Manually fire the probe against the formerly-suspended engine:
neksur-cli verifier probe --engine dremio --table prod.orders

# Expected: "Probe result: PASS (hash matches reference engines)"
```

If the manual probe passes, the fix is confirmed.

---

## 4. Clearance Procedure (Cypher Write with Attestation)

**MANDATORY:** The clearance commit MUST include the attestation phrase
`"ATTESTATION: root-cause identified and fixed — <brief description>"` in the
`clearance_note` field. This is the T-3-divergence-suppression mitigation.

### 4.1 Template Cypher for single engine + table

```cypher
-- AGE 1.6 write via ExecuteInTenant (schema-qualified per D-004.03):
-- Replace $policy_id, $engine_kind, $attestation_note with real values.
MATCH (cp:CompiledPolicy {
    policy_id: $policy_id,
    engine_kind: $engine_kind
})
WHERE cp.status = 'divergent_suspended'
SET cp.status = 'active',
    cp.clearance_at = now(),
    cp.clearance_note = $attestation_note
RETURN cp.policy_id, cp.engine_kind, cp.status, cp.clearance_at
```

### 4.2 Example clearance via neksur-cli (if available)

```bash
neksur-cli policy clear-suspension \
    --policy-id rls-prod-orders-v3 \
    --engine dremio \
    --attestation "ATTESTATION: root-cause identified and fixed — Dremio dialect compiler emitted wrong quoting for column-mask SQL fragment; fix landed in commit abc1234; re-compiled policy verified PASS on probe"
```

### 4.3 Verify clearance took effect

```bash
# Check CompiledPolicy status reverted to active:
curl -s http://neksur-server:9100/metrics | \
    grep 'compiled_policy_status{.*dremio.*}'
# Expected: compiled_policy_status{engine="dremio", table="prod.orders", status="active"} 1

# Check 503 errors have stopped for Dremio on the affected table:
curl -s http://neksur-server:9100/metrics | \
    grep 'commit_rejected_total{reason="policy_engine_divergent"}'
# Expected: counter does not increment after clearance
```

---

## 5. Monitoring After Clearance

After clearing the suspension, monitor the table for 1 hour:

```bash
# Watch for divergence re-occurrence:
watch -n 30 'curl -s http://neksur-server:9100/metrics | grep cross_engine_divergence_total'
```

If `cross_engine_divergence_total` increments again within 1 hour, the fix was incomplete.
Re-investigate before clearing again. If it increments 3+ times on the same engine+table,
escalate to platform engineering — this is a systemic issue.

---

## 6. PENDING_FIRST_RUN Gate

Per 03-ACCEPTANCE.md §9, the "auto-suspend divergent engine end-to-end" acceptance row
is `PENDING_FIRST_RUN` until the nightly-cross-engine.yml workflow exits 0 with the
`TestDivergenceVerifierAutoSuspend` test passing against a staging cluster with all 4
engines configured. This runbook covers the manual verification step required after the
automated gate passes.

---

## 7. Pass / Fail Checklist

| # | Check | Pass Criterion |
|---|-------|----------------|
| 1 | Divergence event in AGE graph | `DivergenceEvent` node exists with correct `engine_kind` + `table_name` |
| 2 | Root cause identified | One of the §3.1 categories confirmed with evidence |
| 3 | Fix applied and probed | Manual probe exits PASS before clearance |
| 4 | Clearance Cypher includes attestation | `clearance_note` field contains `ATTESTATION:` prefix |
| 5 | Status reverted to `active` | Prometheus metric shows `status=active` for engine |
| 6 | No new divergences in 1 hour | `cross_engine_divergence_total` counter stable |

---

*Phase 3 operator runbook — divergent_suspended clearance + attestation procedure.
T-3-divergence-clear-bypass mitigation per Plan 03-15 threat model.
Closes 03-VALIDATION.md Manual-Only §Auto-suspend end-to-end.*
