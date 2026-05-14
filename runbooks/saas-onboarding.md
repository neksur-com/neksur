# Runbook: SaaS Tenant Onboarding (Phase 0.5)

**Owner:** Founder / first-on-call SRE (Phase 0.5); Customer Success (Phase 1+)
**Scope:** Drive `./scripts/provision-tenant.sh` end-to-end for a new
design-partner tenant — from "customer signed" to "tenant fully active
on the SaaS pilot infrastructure".
**Contract:** **D-0.5.19** (12-step idempotent provisioning).
**Validation tests:** `tests/integration/provisioning_test.go::TestProvisioningIdempotent`
(per-step idempotency) + `TestProvisioningRegex` (input validators) +
`tests/integration/_phase05_p04_blocking_attestation.md` (end-to-end attestation).
**Closes:** `00.5-VALIDATION.md` Manual-Only Verifications row
"Customer-facing onboarding kick-off"; covers REQ-saas-onboarding and
contributes evidence to ROADMAP §Phase 0.5 success criterion 3
("Tenant onboarded end-to-end via script in <2h").

> **Note on the contract.** The provisioning script is idempotent at every
> step (RESEARCH §Pattern 3): re-running on a partially-onboarded tenant
> resumes from wherever the previous run stopped. Step (j) is an async
> gate — the customer must apply their accepter Terraform module before
> the script proceeds past peering acceptance. The script exits 0 at
> step (j) when peering is not yet active so the operator can re-run.

---

## 1. Prerequisites

Before kicking off, confirm with the customer (in writing):

| Item | Required | How to obtain |
|------|----------|---------------|
| **Tenant UUID** | UUID v4 (mint via `uuidgen` and record in customer-facing onboarding doc) | Generate locally; e.g., `uuidgen \| tr 'A-Z' 'a-z'`. Validate matches `^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$` |
| **WorkOS organization** | Created in WorkOS Dashboard → Organizations | WorkOS Dashboard → Organizations → "Create"; copy the `org_...` ID |
| **Customer AWS account ID** | 12 digits | Customer's AWS console → top-right account dropdown |
| **Customer VPC ID** | `vpc-` + 17 hex chars | Customer-supplied (from their VPC console); validate matches `^vpc-[0-9a-f]{17}$` |
| **Customer VPC CIDR** | Non-overlapping with Neksur's 10.0.0.0/16 | Customer-supplied; confirm no overlap before continuing |
| **Customer AWS region** | e.g., `us-east-1` | Customer-supplied; must match the region of the VPC |
| **Customer-side SRE contact** | Email + Slack/Teams handle | Customer-signed onboarding kit |
| **Stripe Customer ID** (optional) | `cus_...` | Stripe Dashboard → Customers (set later when billing flips on at M7+; not required for Phase 0.5) |

**HALT criteria:** if customer VPC CIDR overlaps Neksur's `10.0.0.0/16`,
the peering will be silently broken. Ask the customer to pre-allocate a
non-overlapping CIDR or peer via a transit gateway (Phase 2+).

---

## 2. Verify customer's AWS VPC info

Sanity-check the customer-supplied VPC info BEFORE running the provisioning
script — bad input late in the script triggers a rollback that costs
~15 minutes per attempt.

```bash
# From the operator workstation, with the customer's read-only AWS profile.
aws --profile customer-readonly --region us-east-1 \
    ec2 describe-vpcs --vpc-ids vpc-0123456789abcdef0 \
    --query 'Vpcs[].{CidrBlock:CidrBlock,DnsHostnames:EnableDnsHostnames,DnsResolution:EnableDnsSupport}' \
    --output table
```

Confirm:

- `DnsHostnames = True` AND `DnsResolution = True` — required for the
  RDS endpoint to resolve to the private IP via peering (RESEARCH
  Pitfall 6; documented in `runbooks/vpc-peering.md` §4).
- `CidrBlock` does NOT overlap `10.0.0.0/16`.

If `DnsHostnames = False`, ask the customer to enable it BEFORE peering
apply — toggling it post-apply requires destroying the peering connection.

---

## 3. Run the provisioning script

```bash
cd /Users/evgeny/neksur-core

export DATABASE_URL='postgres://admin@pool-a-primary.private:5432/postgres'
export TF_DIR='/Users/evgeny/neksur-infra/environments/phase0-pilot'
export PRIVATE_CA_ARN='arn:aws:acm-pca:us-east-1:964775859511:certificate-authority/<uuid>'
export SLACK_OPS_WEBHOOK_URL='<from AWS Secrets Manager — neksur/slack/ops_webhook>'

./scripts/provision-tenant.sh \
    <tenant-uuid> \
    <workos-org-id> \
    <customer-vpc-id> \
    <customer-vpc-region>
```

The script orchestrates the **D-0.5.19 12-step flow** (mirrors
`internal/tenant/provision.go`):

| Step | Action | Idempotent? |
|------|--------|-------------|
| (a)+(b) | `neksur-cli tenant create` — INSERT public.tenants row + AGE create_graph + per-tenant role | yes (ON CONFLICT DO NOTHING / IF NOT EXISTS) |
| (c)+(d) | `neksur-cli tenant migrate --step bootstrap-schema` — search_path-safe role provisioning + GRANTs | yes (REVOKE-of-non-existing is no-op) |
| (e) | `neksur-cli tenant migrate --step apply-versioned` — Atlas tenant-loop V0050 + V0051 + V0052 | yes (Atlas tracks revisions) |
| (f)+(g) | Covered by step (e) — audit_log + query_history + policies tables created | yes |
| (h) | `neksur-cli tenant peer` — terraform apply Neksur-side `module.customer_peering[<uuid>]` | yes (Terraform tracks state) |
| (i) | `neksur-cli tenant peer --show-customer-module` — prints customer-side Terraform module | yes |
| (j) | `neksur-cli tenant peer --status` — polls peering status; exits 0 if not yet active | **GATE** — re-run after (k) |
| (k) | `neksur-cli tenant smoke` — gateway audit + policy fetch + cross-tenant probe | yes |
| (l) | Slack ops notification (curl POST) | yes (notification can be re-sent) |

**Expected wall-clock** (Phase 0.5 baseline against sandbox AWS):
~15 minutes for steps (a)–(i) on first attempt; **async wait at step (j)**
typically 15–60 minutes (customer SRE response time); ~5 minutes for
steps (k)+(l) on resume.

---

## 4. Async peering wait

When step (j) prints:

```
==> Peering not yet active (pending-acceptance).
    Re-run this script after customer applies their accepter module.
```

The script has exited 0. Hand the customer-side Terraform module
(captured at step (i)) to the customer SRE via the Slack DM channel or
secure email. Sample handoff text:

> Hi {customer-SRE},
>
> Neksur side of the peering is up and waiting for your accepter.
> Apply this module on your AWS account in the {region} region:
>
> ```hcl
> { paste the BEGIN..END CUSTOMER MODULE block from step (i) }
> ```
>
> You'll need to supply `peering_connection_id = "{pcx-...}"` (printed
> above) and the route-table IDs in your VPC that should route Neksur
> traffic via the peering. After your apply succeeds, ping me and I'll
> finish onboarding.

**Confirm with customer:** apply succeeded; `aws ec2 describe-vpc-peering-connections`
returns `Status.Code = "active"`. Optionally pre-flight with
`runbooks/vpc-peering.md` §3.

---

## 5. Re-run the provisioning script

After the customer confirms peering is active, re-run the SAME command:

```bash
./scripts/provision-tenant.sh \
    <tenant-uuid> \
    <workos-org-id> \
    <customer-vpc-id> \
    <customer-vpc-region>
```

The script resumes from step (j) — step (j) now passes (peering is
active), step (k) runs smoke tests, step (l) posts Slack notification.

---

## 6. Smoke tests passing → onboarding complete

Step (k) runs three smoke checks via `./neksur-cli tenant smoke`:

1. **Gateway audit row write** — INSERT into `tenant_<uuid>.audit_log`
   from the per-tenant role; assert success + visible to the role.
2. **Policy fetch** — `SELECT count(*) FROM tenant_<uuid>.policies`
   returns 0 (newly-provisioned tenants have no policies; baseline OK).
3. **Cross-tenant probe (Layer 2 RLS)** — attempt SELECT against
   another tenant's schema; assert SQLSTATE 42501 (insufficient_privilege).

If ANY smoke check fails, HALT and investigate before declaring
onboarding complete:

- Layer 2 failure → tenant role's GRANT list is broken; re-run step (c)+(d).
- Layer 3 RLS failure (cross-tenant probe returns rows) → critical
  security issue; suspend the new tenant via `./scripts/tenant-suspend.sh`
  and page the on-call.
- Pgwire reachability failure → peering route propagation incomplete;
  wait 2–5 minutes and retry.

---

## 7. Slack notification → record onboarded_at

Step (l) posts to the ops Slack channel. After the message lands:

```bash
# Mark tenant fully onboarded (sets public.tenants.onboarded_at if NULL).
psql "$DATABASE_URL" -c "
    UPDATE public.tenants
       SET onboarded_at = COALESCE(onboarded_at, now())
     WHERE id = '<tenant-uuid>';
"
```

This timestamp is what the customer-facing admin UI surfaces as "Member
since YYYY-MM-DD". It is also what `00.5-ACCEPTANCE.md` §ROADMAP success
criterion 3 ("<2h end-to-end") measures against.

---

## 8. Cross-references

- `scripts/provision-tenant.sh` — the canonical orchestrator (this
  runbook is its operator manual).
- `internal/tenant/provision.go` — the Go implementation of each step;
  read this when debugging a step failure.
- `runbooks/vpc-peering.md` — operator setup + troubleshooting for the
  peering step (h)–(j).
- `runbooks/tenant-lifecycle.md` — state-machine transitions AFTER
  onboarding (suspend / wind-down / delete).
- `runbooks/secret-rotation.md` — rotate the PRIVATE_CA_ARN / WORKOS_API_KEY /
  Slack webhook URLs the script depends on.
- `tests/integration/_phase05_p04_blocking_attestation.md` — Plan 04's
  end-to-end attestation (proves the 12-step path works against a real
  testcontainer).
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-ACCEPTANCE.md` —
  §ROADMAP success criterion 3 cross-reference.

---

## 9. Common failures + fixes

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `invalid UUID v4` exit 2 at script start | UUID is uppercase OR has version nibble ≠ 4 | Re-mint via `uuidgen \| tr 'A-Z' 'a-z'` |
| `invalid workos org id` exit 2 | Org ID is lowercase / has dashes | WorkOS org IDs are `org_` + uppercase alphanumeric only |
| Step (a/b) — `tenant_by_workos_org returned NULL` after INSERT | RLS predicate hit before `app.current_tenant` GUC is set | Confirm INSERT runs as `admin_role` (BYPASSRLS); not `neksur_app` |
| Step (e) — `revision 0050 already applied` | Re-running after partial completion | Idempotent — script proceeds; no action needed |
| Step (h) — `terraform: VPC peering quota exceeded` | AWS soft quota of 50 VPC peerings per VPC reached | Request quota increase via `./scripts/request-vpc-peering-quota-increase.sh` (Plan 05) |
| Step (j) — peering stays `pending-acceptance` >24h | Customer didn't apply accepter | Re-DM customer; for Phase 1+ consider an SLA gate |
| Step (k) — smoke test fails with `relation does not exist` | Atlas tenant-loop did NOT apply V0050+V0051+V0052 | Re-run script — step (e) is idempotent and will detect missing migrations |
