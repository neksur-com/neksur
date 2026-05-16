# Runbook: Policy Compile Failure Diagnosis (Phase 2)

**Owner:** Platform engineer — Policy Compiler team
**Scope:** Diagnosing and resolving policy compilation failures for the Phase 2
cross-engine policy compiler. Covers `policy_compile_total{status=compile_failed|probe_failed}`
alerts, Pitfall 3 (draft/active lifecycle confusion), Pitfall 4 (CompiledPolicy unbounded
graph growth), and Open Question 3 (engine version bump → recompile).
**Triggers:** PolicyCompileFailureRate alert (observability/rules/phase2-policy-compile.yml)
**Closes:** Phase 2 operational requirement per RESEARCH §Standard Stack line 363

---

## When This Fires

The `PolicyCompileFailureRate` alert fires when:

```
rate(policy_compile_total{status=~"compile_failed|probe_failed"}[5m]) > 0.1
```

Sustained for 5 minutes → `severity=page` → PagerDuty oncall wake.

Two status labels distinguish the failure path:

| `status` label | Root cause | Impact |
|----------------|------------|--------|
| `compile_failed` | CEL compilation error or engine-specific transpilation error | Policy NOT compiled; CompiledPolicy node stays in `status=compile_failed`; that engine CANNOT evaluate this policy |
| `probe_failed` | Compiler probe (dry-run against synthetic request) failed post-compile | Policy compiled but unsafe to activate; stays in `status=probe_failed`; T-2-compile-fail-open mitigation |

Both paths leave the policy **inactive** for the affected engine — requests are NOT blocked
(fail-open design per Phase 2 CONTEXT D-2.04). A `compile_failed` policy silently passes
all commits for the affected engine until fixed.

**Escalation:** If `compile_failed` rate persists > 15 minutes and the affected policies
control high-value tables (`orders`, `payments`, `customers`), escalate to security team —
the fail-open state means the access control is inactive.

---

## 1. Identify the Affected Policies

### 1.1 Query the metric by label

```promql
topk(10, sum by (policy_id, engine_kind, status) (
  rate(policy_compile_total{status=~"compile_failed|probe_failed"}[5m])
))
```

This surfaces the top 10 (policy_id, engine_kind) tuples with the highest failure rate.
Record the `policy_id` and `engine_kind` values.

### 1.2 Inspect the compile log table

```sql
-- Query the last 10 compile failures for the policy
SELECT
  id,
  policy_id,
  engine_kind,
  status,
  error_message,
  compiled_at,
  compiled_by_version
FROM public.compiled_policies
WHERE policy_id = '<policy_id>'
  AND engine_kind = '<engine_kind>'
  AND status IN ('compile_failed', 'probe_failed')
ORDER BY compiled_at DESC
LIMIT 10;
```

The `error_message` column contains the engine-specific error (CEL parse error, Trino SQL
transpilation error, Spark DSL error, etc.).

### 1.3 Inspect the graph node

The Phase 2 compiler stores a `CompiledPolicy` node in the AGE graph for each
`(policy_id, engine_kind)` tuple. Inspect via:

```bash
neksur-cli policy compiled-status --policy-id <policy_id> --engine <engine_kind>
```

Expected output:

```
CompiledPolicy {
  policy_id:   "my-residency-policy"
  engine_kind: "trino"
  status:      "compile_failed"
  version:     "3"
  error:       "CEL residency binding: location.region not available for Trino SQL transpilation"
  compiled_at: "2026-05-16T08:00:00Z"
}
```

---

## 2. Diagnose by Failure Type

### 2.1 `compile_failed` — CEL or transpilation error

The most common causes:

**CEL syntax error in policy definition:**

```
error_message: "cel: compile policy <policy_id>: ERROR: <input>:1:18: undeclared reference to 'manifest.classification_satisfied'"
```

Mitigation: The policy author used a Phase 2 binding that isn't registered on the target engine.
Check binding availability per engine:

| CEL binding | Trino | Spark | Dremio |
|-------------|-------|-------|--------|
| `manifest.has_column` | Y | Y | Y |
| `manifest.classification_satisfied` | Y | Y | N (Pitfall 11 — compile_failed expected) |
| `location.region` | Y | Y | N |
| `principal.attribute` | Y | Y | Y |
| `manifest.partition_spec` | Y | N | N |

A `compile_failed` on a Dremio engine for `manifest.classification_satisfied` is **expected**
(Pitfall 11 mitigation). File a `dremio_unsupported_binding` known issue; don't page.

**SQL transpilation error (Trino/Spark row-filter):**

```
error_message: "trino: transpile row-filter: unsupported CEL AST node type: comprehension"
```

Row-filter expressions must be simple conjunctions/disjunctions (no `exists`, no `filter`,
no `all` CEL comprehensions). The transpiler converts CEL to SQL WHERE clause; complex CEL
requires pre-computation at the gateway level (not yet supported in Phase 2).

Mitigation: Rewrite the row-filter using SQL-friendly patterns:

```cel
// NOT OK — comprehension:
commit.updates.exists(u, u.region == principal.attribute('region'))

// OK — direct attribute comparison:
principal.attribute('region') == 'us-east-1'
```

### 2.2 `probe_failed` — passes compile, fails dry-run

The compiler probes each newly compiled policy against a synthetic request before activating it.
A `probe_failed` means the compiled policy panics or returns an unexpected type when evaluated
against the probe inputs.

Common cause: **Pitfall 8 ABAC null-safety** — policy dereferences a principal attribute
without first checking existence.

```
error_message: "probe: eval error: attribute 'clearance' not found on principal map: <nil>.contains"
```

Check the policy CEL expression for the unsafe pattern:

```cel
// UNSAFE — panics on null receiver if 'clearance' attribute absent:
principal.attribute('clearance').contains('secret')

// SAFE — has() macro checks existence before dereference:
has(principal.attribute('clearance')) && principal.attribute('clearance').contains('secret')
```

Fix the policy via `neksur-cli policy edit` or the policy-bank PR. Recompile is automatic
when the policy text changes (V0030 trigger).

---

## 3. Pitfall 3 — Draft vs. Active Policy Lifecycle Confusion

**Symptom:** Operator edits a policy but the change doesn't appear to take effect.
`policy_compile_total{status="active"}` count for the policy doesn't increase.

**Root cause:** The edited policy is still in `DRAFT` state. The compiler only activates
`CompiledPolicy` nodes for `PUBLISHED` policies; DRAFT policies are compiled but never
activated.

**State machine:**

```
  DRAFT ──[neksur-cli policy publish]──► PUBLISHED
                                              │
                                    (V0030 trigger fires)
                                              ▼
                                   compiler picks up + compiles
                                              ▼
                                   CompiledPolicy.status = 'active'
```

**Diagnostic:**

```sql
SELECT id, tenant_id, state, definition_cel, updated_at
FROM public.policies
WHERE id = '<policy_id>';
-- Expect state = 'published' for an active policy.
-- state = 'draft' means it's compiled but not activated.
```

**Fix:**

```bash
neksur-cli policy publish --policy-id <policy_id>
# Triggers recompile via V0030 + activates CompiledPolicy nodes.
```

**Note:** `neksur-cli policy publish` is idempotent; running it on an already-published
policy triggers an incremental recompile (useful when you've updated bindings in the engine).

---

## 4. Pitfall 4 — CompiledPolicy Graph Node Unbounded Growth

**Symptom:** AGE graph size growing unexpectedly; `MATCH (n:CompiledPolicy) RETURN count(n)`
returns a large number (expected: max 3 × `count(policies)` × `count(engine_kinds)`).

**Root cause:** Every time a policy is compiled, a new `CompiledPolicy` version is created.
Historical versions accumulate without the daily GC cron.

**Retention rule:** Keep at most **N=3** historical `CompiledPolicy` versions per
`(policy_id, engine_kind)` tuple. Older versions are pruned daily.

**Check the GC cron is running:**

```bash
kubectl get cronjob compiled-policy-gc -n neksur-system
# Expected: LAST SCHEDULE within last 24h, ACTIVE = 0 (not stuck).
```

If the cron is absent or stuck, manually run the GC:

```bash
# Connect to the Postgres+AGE instance:
psql "$DATABASE_URL" << 'SQL'
-- Delete CompiledPolicy nodes older than the 3rd-newest per (policy_id, engine_kind):
SELECT ag_catalog.cypher('neksur_graph', $$
  MATCH (cp:CompiledPolicy)
  WITH cp.policy_id AS pid, cp.engine_kind AS ek,
       collect(cp ORDER BY cp.compiled_at DESC) AS versions
  UNWIND versions[3..] AS old_version
  DELETE old_version
  RETURN count(old_version) AS deleted
$$) AS (deleted agtype);
SQL
```

Expected output: `deleted: <N>` where N is the number of pruned nodes (0 if already clean).

**Retention rule reference:**

| Parameter | Value | Notes |
|-----------|-------|-------|
| N (max versions) | 3 | 1 active + 2 historical for rollback |
| GC cron schedule | `0 2 * * *` (02:00 UTC) | Low-traffic window |
| Retention Cypher | above | Deletes 4th-oldest and beyond per (policy_id, engine_kind) |

---

## 5. Open Question 3 — Engine Version Bump Triggers Recompile

**Scenario:** Trino is upgraded from version 467 to 480. The Trino-specific SQL transpiler
in the compiler has been updated (e.g., new syntax for row-filter). All existing
`CompiledPolicy{engine_kind="trino"}` nodes compiled for Trino 467 may be invalid for Trino 480.

**D-2.05 design note:** The V0030 compiler trigger fires per-Policy-change, NOT per-Engine-change.
An engine version bump does NOT automatically recompile all policies for that engine.

**Operator responsibility:** After upgrading an engine binary, run the engine-scoped recompile:

```bash
# Recompile all policies for the Trino engine (use with care — affects all tenants):
neksur-cli compiler recompile-engine --engine trino

# Recompile for a specific tenant:
neksur-cli compiler recompile-engine --engine trino --tenant-id <tenant_uuid>
```

**What this does:**
1. Queries all `PUBLISHED` policies with `[:APPLIES_TO]` edges to Trino engine nodes.
2. Marks existing `CompiledPolicy{engine_kind="trino"}` nodes as `status=pending_recompile`.
3. Queues recompile jobs — existing policies continue serving the old CompiledPolicy until
   recompile completes (no downtime).
4. Compiler picks up the queue and recompiles each policy against the new engine version.
5. On success: CompiledPolicy.status → `active`, CompiledPolicy.compiled_by_version updated.
6. On failure: CompiledPolicy.status → `compile_failed` (alert fires — follow §2).

**Verification after recompile:**

```bash
# All Trino policies should be active:
neksur-cli compiler status --engine trino | grep -v active
# Expected: empty output (all active).
```

---

## 6. Pitfall 11 — Probe Leak (Already Mitigated in Code)

The compiler probe for Dremio-engine policies creates a synthetic connection to the Dremio
endpoint specified in the engine registry. In Phase 2, Dremio engine rows are seeded with
a placeholder endpoint (`dremio://localhost:9047`) that never resolves.

**Symptom:** `probe_failed` with error `"connection refused: dremio://localhost:9047"`.

This is expected and non-actionable — Dremio support is stub-level in Phase 2. The probe
leak mitigation (Plan 02-04) closes the connection + marks `probe_failed` without retrying,
so no file descriptor leak occurs. Confirm:

```bash
# Check no leaked Dremio connections in neksur-server:
ls -la /proc/$(pgrep neksur-server)/fd | grep -c dremio
# Expected: 0
```

If > 0, the probe leak mitigation regressed — file a bug against `internal/compiler/probe.go`.

---

## 7. Escalation

| Condition | Action |
|-----------|--------|
| `compile_failed` > 15m on policies covering high-value tables | Escalate to security team — fail-open state active |
| `probe_failed` on all engines for newly pushed policy | Policy author bug — notify policy author; use `neksur-cli policy rollback` |
| GC cron down > 24h | Ops ticket; run manual GC (§4); not urgent |
| Engine recompile after version bump still failing > 30m | Engine team + policy compiler team war room |

---

## References

- **D-2.04** — CompiledPolicy graph node shape + compiler architecture.
- **D-2.05** — V0030 LISTEN/NOTIFY trigger for per-policy recompile.
- **Pitfall 3** — draft/active distinction (CONTEXT §3 Phase 2 Pitfalls).
- **Pitfall 4** — CompiledPolicy retention N=3 (CONTEXT §3 Phase 2 Pitfalls).
- **Pitfall 8** — ABAC null-safety (CONTEXT §3 Phase 2 Pitfalls + runbooks/policy-author.md §Phase 2).
- **Pitfall 11** — probe leak mitigation (CONTEXT §3 Phase 2 Pitfalls).
- **Open Question 3** — engine version bump trigger (Phase 2 CONTEXT §4).
- **observability/rules/phase2-policy-compile.yml** — PolicyCompileFailureRate alert definition.
- **runbooks/policy-author.md** — CEL policy authoring + Pitfall 8 null-safety pattern.

---

*Phase 2 policy compile debug runbook — Phase 2 Plan 02-08 Task 2.*
