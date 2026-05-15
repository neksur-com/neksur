# Runbook: CEL Policy Authoring (SecOps)

**Owner:** SecOps + Phase 1 platform engineer
**Scope:** How to author CEL policies (P1 schema / P2 write-ACL /
P3 retention) for the Neksur L1 Catalog Gateway — including the
mandatory `neksur-cli policy compile` dogfood step (Pitfall 7
mitigation per CONTEXT line 84) and the example expressions from
01-CONTEXT line 172.
**Closes:** Phase 1 acceptance criterion §5
(REQ-write-l1-schema-policy + REQ-write-l1-write-acl +
REQ-write-l1-retention). Plan 01-05 ships the engine + bindings;
Plan 01-06 ships the gateway wiring; this runbook + the
`neksur-cli policy compile` subcommand close the operator-facing
authoring loop.

---

## 1. Purpose

CEL (Common Expression Language) policies are the per-table gating
predicates the L1 gateway evaluates on every Iceberg commit. Three
families are in scope for Phase 1:

| Family | Edge label (V0030) | Stored on vlabel | Example intent |
|--------|-------------------|------------------|----------------|
| **P1 — Schema** | `[:SCHEMA_GOVERNS]` | generic `Policy` | "No PII column in this table" |
| **P2 — Write ACL** | `[:WRITE_GOVERNS]` | generic `Policy` | "Only principals X+Y may write this table" |
| **P3 — Retention** | `[:RETAINS]` | `RetentionPolicy` (ADR-010) | "No snapshot expiration below 30 days" |

The gateway evaluates every applicable policy on every commit;
**the FIRST `ActionDeny` rejects the commit** with HTTP 403 + a
`WriteEvent {REJECTED}` audit emission + the rejection reason in the
response body. Compile/eval failures are fail-closed → HTTP 503
(D-1.09); see `runbooks/commit-rejected-503-rate.md` for the
incident response.

**Policy text contract** — every CEL expression MUST evaluate to a
`bool`:

- `true` → allow the commit (the gateway continues evaluating
  remaining policies).
- `false` → deny the commit (the gateway STOPS evaluation, audits
  REJECTED, returns 403).

Non-bool returns are wrapped in `cel.ErrPolicyReturnedNonBool` and
treated as fail-closed (503) — `42` and `"yes"` will NOT bypass.

---

## 2. Workflow

The dogfood workflow has three steps. Each step is mandatory:

1. **Author** — Write the CEL expression in a `.cel` file in your
   local clone of `neksur-com/policy-bank` (or operator scratch dir).
2. **Compile-test** — Run `neksur-cli policy compile <file>` (this
   plan, Plan 01-09 Task 3). This dogfoods the EXACT same
   `cel.Compiler` the runtime gateway uses (Plan 01-05); operators
   see the same error messages a customer's request would trigger.
3. **Push to graph** — Insert/update the `Policy` (P1/P2) or
   `RetentionPolicy` (P3) graph node via the standard tenant-scoped
   pattern; the gateway picks it up on the next commit (no restart
   needed — `cel.Compiler.CompileOrGet` cache-key is hashed on text
   so an update auto-invalidates).

### 2.1 Compile-test (mandatory per Pitfall 7 — CONTEXT line 84)

```bash
$ neksur-cli policy compile /path/to/my-schema-policy.cel
Policy my-schema-policy.cel compiles cleanly.
$ echo $?
0
```

On compile failure:

```bash
$ neksur-cli policy compile /path/to/bad.cel
policy compile: cel: compile policy cli-compile-/path/to/bad.cel:
  cel: compile failed
  ERROR: <input>:1:18: undeclared reference to 'manifest.hasColumn'
   | manifest.hasColumn(table, "ssn")
   | .................^
$ echo $?
1
```

The subcommand exits **0 on valid CEL** and **non-zero on syntax or
binding errors**, with the wrapped `cel.ErrCompileFailed` sentinel
preserved in the message so operators can `grep -E 'compile failed'`
in CI logs.

Exit codes:

| Exit | Meaning |
|------|---------|
| `0` | Policy compiles cleanly. |
| `1` | CEL syntax error OR undeclared binding (wrapped in `cel.ErrCompileFailed`). |
| `2` | Wrong usage (missing arg) OR file does not exist / unreadable. |

### 2.2 Push to graph (P1 + P2 generic Policy shape)

The Phase 1 push is an `INSERT … RETURNING` into the per-tenant graph
inside `tenant.WithTenantTx` (Phase 0.5). A `neksur-cli policy push`
subcommand is planned but not yet shipped — until it lands, use the
following Cypher pattern executed via `gc.ExecuteInTenant`:

```cypher
-- P1 — generic Policy node + [:SCHEMA_GOVERNS]->(Table)
MERGE (p:Policy { id: 'no-pii-orders' })
  ON CREATE SET p.tenant_id = $tenant_id,
                p.kind = 'schema',
                p.definition_cel = $cel_text,
                p.created_at = now()
WITH p
MATCH (t:Table { name: 'orders', namespace: 'prod' })
MERGE (p)-[:SCHEMA_GOVERNS]->(t)
RETURN p.id
```

For P2, replace `kind: 'schema'` with `kind: 'write_acl'` and the
edge with `[:WRITE_GOVERNS]`.

> **AGE 1.6 quirk:** `ON CREATE SET` and `ON MATCH SET` are rejected
> by AGE 1.6 (Plan 01-04 deviation #1). Use the COALESCE-on-WITH-SET
> emulation pattern from `internal/policy/store/age.go` — see Plan
> 01-04 SUMMARY + the `internal/ingest/snapshot.go` `pitfall5SemanticTag`
> audit anchor for the canonical shape.

### 2.3 Push to graph (P3 RetentionPolicy ADR-010 shape)

P3 is **NOT a generic Policy** — it uses the ADR-010 `RetentionPolicy`
vlabel + `[:RETAINS]` edge so the Phase 2 lifecycle scheduler can
consume the same shape without a migration:

```cypher
MERGE (rp:RetentionPolicy { id: 'no-young-expire-orders' })
  ON CREATE SET rp.tenant_id = $tenant_id,
                rp.kind = 'retention',
                rp.definition_cel = $cel_text,
                rp.created_at = now()
WITH rp
MATCH (t:Table { name: 'orders', namespace: 'prod' })
MERGE (rp)-[:RETAINS]->(t)
RETURN rp.id
```

The gateway's `store.AGEStore.LoadPoliciesForTable` (Plan 01-05) runs
two MATCH queries: one for `(:Policy)-[:SCHEMA_GOVERNS|WRITE_GOVERNS]->`
edges and one for `(:RetentionPolicy)-[:RETAINS]->`. Both are merged
into a single `[]cel.Policy` slice before evaluation. P3's `kind`
field MUST be `"retention"` for the gateway to apply it to expire-snapshot
commits.

---

## 3. Example CEL expressions

These are the CONTEXT line 172 reference examples — operators should
adapt them to their specific schemas. All three expressions return
`bool`: `true` = allow, `false` = deny.

### 3.1 P1 — no PII column in this table

```cel
// P1 — no PII column in this table.
// CONTEXT line 172 example #1.
// Pitfall 7: CEL has no JSONPath — use the manifest.has_column()
// custom binding (Plan 01-05 functions.go) to inspect table schema.
//
// Returns TRUE (allow) when NONE of the banned column names exist.
!manifest.has_column(table, "ssn") &&
!manifest.has_column(table, "credit_card") &&
!manifest.has_column(table, "social_security_number")
```

Test with:

```bash
neksur-cli policy compile p1-no-pii-orders.cel
# Expect: "Policy p1-no-pii-orders.cel compiles cleanly."
```

### 3.2 P2 — only principal X writes this table

```cel
// P2 — only allowlisted principals may commit.
// CONTEXT line 172 example #2.
// Phase 1: principal.sub comes from the Pitfall 8 chain
// (mTLS SAN > Authorization Bearer > WorkOS session — see
// internal/gateway/iceberg/principal.go for the precedence rules).
//
// Returns TRUE iff principal.sub is in the allowlist.
principal.sub in ["alice@neksur.com", "bob@neksur.com"]
```

For role-based allowlists, use the `principal.role` custom binding
(also Plan 01-05):

```cel
principal.role(principal, "writer:prod") ||
principal.role(principal, "admin")
```

### 3.3 P3 — no snapshot expiration below N days (ADR-010 RetentionPolicy)

```cel
// P3 — block expire-snapshots when target snapshot age < 30 days.
// CONTEXT line 172 example #3 + ADR-010 RetentionPolicy shape
// (CONTEXT line 86 override — P3 uses RetentionPolicy + [:RETAINS],
// NOT generic Policy + [:RETENTION_GOVERNS]).
//
// Inspects commit.updates for any "remove-snapshot" operation against
// a snapshot whose committed_at_ms is within 30 days of now.
//
// NOTE: cel-go has no `now()` stdlib; the gateway exposes the
// commit-time millis via `commit.now_ms` when Plan 01-06's gateway
// wires it (today the test fixtures pre-compute the cutoff and
// splice into the policy text — see Plan 01-05 P3 test).
//
// Returns TRUE iff NO update is a too-young snapshot expiration.
!commit.updates.exists(u,
    u.action == "remove-snapshot" &&
    (commit.now_ms - u.snapshot.committed_at_ms) < 30 * 86400 * 1000
)
```

For tests, see `tests/integration/policy_cel_p3_retention_test.go`
which pre-computes the cutoff to avoid the missing-`now()` issue.
Phase 2 ADR-010 scheduler will register a `now()` binding so P3
policies can be authored without time-of-write substitution.

---

## 4. CEL function reference

Plan 01-05 `internal/policy/cel/functions.go` registers three custom
binary bindings. The L1 gateway env declares three variables — `table`
+ `commit` + `principal` — all `MapType(StringType, DynType)`.

| Function | Arity | Returns | Behavior |
|----------|-------|---------|----------|
| `manifest.has_column(table, name)` | binary (table, string) | bool | `true` iff `table.schema.fields[].name == name` for any field. Case-sensitive. |
| `manifest.has_partition(table, spec_name)` | binary (table, string) | bool | `true` iff `table.partition_spec.fields[].name == spec_name`. |
| `principal.role(principal, role)` | binary (principal, string) | bool | `true` iff `role in principal.roles`. |

Variable map shapes (full projection at
`internal/gateway/iceberg/handler.go::tableMetadataToMap`):

```
table:
  uuid: string
  schema:
    fields: [{ id: int, name: string, type: string, required: bool, doc: string }, ...]
  partition_spec:
    spec_id: int
    fields: [{ source_column_id: int, transform: string, name: string }, ...]
  current_snapshot_id: int
  metadata_location: string
  snapshots: [{ snapshot_id, parent_snapshot_id, timestamp_ms, operation, metadata_location }, ...]
  properties: map<string, string>

commit:
  requirements: [ map<string, any>, ... ]
  updates:      [ map<string, any>, ... ]
  # Phase 1+ may add: commit.now_ms (current millis at gateway), commit.principal_source

principal:
  sub:   string   # OIDC subject — Pitfall 8 chain
  email: string   # may be empty if mTLS path is taken
  roles: [string, ...]
```

**Phase 2 will add** the following bindings (REQ-write-l2-residency
+ REQ-write-l2-classification + REQ-write-l2-partition-spec); they
are NOT available in Phase 1 — `neksur-cli policy compile` rejects
expressions that reference them:

- `manifest.classification(column, tag)` — bool
- `manifest.residency(table)` — string (region code)
- `manifest.partition_in(spec, name, allowed_values)` — bool

---

## 5. Debugging

Common compile errors and how to read them:

### 5.1 Typo in binding name

```
ERROR: <input>:1:1: undeclared reference to 'manifest.hasColumn'
 | manifest.hasColumn(table, "ssn")
 | ^
```

The function is `manifest.has_column` (snake_case), not
`manifest.hasColumn` (camelCase). The cel-go convention is
snake_case for custom bindings; the error message is verbatim from
cel-go and the `^` points at the unresolved reference.

### 5.2 Wrong arity

```
ERROR: <input>:1:1: found no matching overload for 'manifest.has_column'
  applied to '(map(string, dyn), string, string)'
 | manifest.has_column(table, "ssn", "extra")
 | ^
```

`manifest.has_column` is **binary** — takes exactly `(table, name)`.
Three args produce the "no matching overload" error. The same error
shape applies to all three custom bindings.

### 5.3 Non-bool return

The gateway will fail-closed (503) on a non-bool return; the
compile step does NOT catch this (cel-go compiles `42` cleanly — it's
a valid CEL expression that happens to return int). The runtime
catches it as `cel.ErrPolicyReturnedNonBool`. To dogfood non-bool
returns, evaluate against a test input:

```bash
# (Future) neksur-cli policy eval <file> --table=test-fixture.json
# Not shipped in Phase 1; the compile-test is the minimum gate.
```

### 5.4 Missing required field on a function call

If a CEL expression accesses `table.no_such_field`, cel-go does NOT
fail at compile time — `table` is declared as `MapType(StringType,
DynType)` so any key is acceptable at compile. At RUNTIME the
expression evaluates to a typed-Value error that the gateway maps to
ErrPolicyEvalFailed → 503 + `commit_rejected_total{reason=
"policy_engine_unavailable"}` increment (see Plan 01-05's
`TestEvaluatorFailClosedOnCELPanic` and Plan 01-06's
`TestGateway503OnCELPanic`).

**Mitigation:** Test new policies against staging traffic for at
least 24h before promoting to production. The PolicyEngineUnavailableAlert
will fire at severity=page if a malformed policy hits production —
follow `runbooks/commit-rejected-503-rate.md`.

---

## 6. Pitfall 7 note — CEL has no JSONPath

**This is the single most common authoring trap.** CEL does NOT
support JSONPath-style queries:

```cel
// THIS DOES NOT PARSE — cel-go has no JSONPath operator:
commit.updates[0].schema.fields[name=='ssn']
```

To inspect manifest contents, use the `manifest.*` function bindings:

```cel
// CORRECT — the custom binding handles the lookup internally:
manifest.has_column(table, "ssn")
```

To inspect commit-request fields, CEL DOES support standard list +
map indexing:

```cel
// OK — standard CEL list access + dot-notation:
commit.updates.exists(u, u.action == "remove-snapshot")

// OK — standard CEL list slicing:
size(commit.updates) > 0

// NOT OK — predicate-in-square-brackets is JSONPath, not CEL:
// commit.updates[action=='remove-snapshot']
```

The Phase 1 binding inventory (`manifest.has_column`,
`manifest.has_partition`, `principal.role`) covers the P1/P2/P3
predicates we need. Phase 2 will add residency / classification /
partition-spec bindings (see §4); until they ship, route any
predicate that would require JSONPath through a custom binding
request to the Phase 1 platform engineer.

---

## References

- **ADR-005** — Cypher hardening contract (Phase 5 ratification gate
  for any policy authoring UI per `runbooks/gateway-deploy.md` §7).
- **ADR-007** — Classification graph shape — relevant to the L3
  detection emission, NOT the L1 P1/P2 policy emission. Phase 6 ML
  classifier inherits Classification but does not author P1 policies.
- **ADR-010** — RetentionPolicy + [:RETAINS] shape for P3 (CONTEXT
  line 86 override). Phase 2 lifecycle scheduler consumes the same
  shape — adopting it today avoids a migration later.
- **CONTEXT line 84** — Pitfall 7 mitigation (CEL has no JSONPath +
  `neksur-cli policy compile` dogfood requirement).
- **CONTEXT line 172** — example CEL expressions for P1/P2/P3
  (this runbook §3).
- **Plan 01-05 SUMMARY** — `internal/policy/cel/` package + 12 unit
  tests + 10 BLOCKING integration tests covering every fail-closed
  branch + Pitfall 7 mitigations.
- **Plan 01-06 SUMMARY** — gateway 10-step pipeline + audit emission +
  Pitfall 8 principal chain.
- **`runbooks/commit-rejected-503-rate.md`** — incident response when
  a malformed policy reaches production.
- **`runbooks/gateway-deploy.md`** — gateway HA topology +
  scaling triggers.

---

*Phase 1 policy authoring runbook — closes Pitfall 7 mitigation
contract per CONTEXT line 84 + REQ-write-l1-{schema,write-acl,
retention}. Updated 2026-05-15 by Plan 01-09 Task 1.*
