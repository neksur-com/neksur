# Runbook: Pool B Provisioning (Per-Enterprise Customer)

**Owner:** Phase 0.5 SRE / first-on-call
**Scope:** Operator provisions a dedicated Pool B Postgres-on-EC2
instance for an Enterprise customer (M3 trigger: first Enterprise
candidate signs).
**Contract:** **D-0.5.02** (LOCKED) — Pool B = dedicated Postgres + AGE
per Enterprise tenant, sized r6g.large–r6g.4xlarge.
**Validation tests:** `tests/integration/pool_b_dry_run_test.go` (AWS-
sandbox-gated; nightly cron via `scripts/dry-run-pool-b.sh`).
**Closes:** ROADMAP §Phase 0.5 success criteria 5 ("Pool B provisioning
workflow is documented and exercised via dry-run").

---

## Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **Customer's AWS account ID** | 12-digit decimal | Customer-supplied; record in commissioning ticket |
| **Customer's VPC CIDR** | RFC 1918 block; non-overlapping with Neksur 10.0.0.0/16 | `aws ec2 describe-vpcs --vpc-ids <id>` (against customer's account) |
| **Backup retention preference** | Days; default 35 PITR + 7 full; max 365 each | Customer-supplied contract clause |
| **KMS key strategy** | Neksur-managed CMK OR customer-managed CMK (BYOK) | Phase 0.5: Neksur-managed only. BYOK = Phase 1 Enterprise add-on. |
| **Pool B instance class** | r6g.large / r6g.xlarge / r6g.2xlarge / r6g.4xlarge | Customer SLA tier; default r6g.large |
| **Operator AWS profile** | scoped to Phase 0.5 pilot account | `aws sts get-caller-identity` reports account 964775859511 |
| **`terraform`** | ≥ 1.7 | `terraform version` |
| **`./neksur-cli`** | freshly built from `main` | `go build -o neksur-cli ./cmd/neksur-cli` from `/Users/evgeny/neksur-core/` |

If any prereq fails, **HALT** and either re-resolve or file a follow-up
ticket with the customer.

---

## 1. Generate the customer UUID

The Pool B Terraform module accepts a UUID v4 as `customer_uuid`. Generate
one and record it in the customer-onboarding ticket:

```bash
CUSTOMER_UUID=$(uuidgen | tr 'A-Z' 'a-z')
echo "Customer UUID: $CUSTOMER_UUID"
# Validate format
echo "$CUSTOMER_UUID" | grep -E '^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$' || { echo "FAIL: not a v4 UUID"; exit 1; }
```

The UUID is the customer's stable identifier through the Pool B lifecycle
(provisioning → migration → eventual offboarding). Do NOT regenerate.

---

## 2. Add the customer to `pool_b_customers`

Edit (or supplement) the environment's tfvars to declare the new Pool B
customer:

```bash
cd /Users/evgeny/neksur-infra/environments/phase0-pilot
$EDITOR terraform.tfvars  # add an entry under pool_b_customers
```

Example tfvars block (the keyed-by-UUID shape mirrors `var.tenant_peerings`):

```hcl
pool_b_customers = {
  "a1b2c3d4-e5f6-4789-8abc-def012345678" = {
    instance_type                    = "r6g.large"
    data_volume_size_gb              = 500
    backup_retention_full_days       = 7
    backup_retention_archive_days    = 35
    cross_region_replication_enabled = false  # Phase 1+ Enterprise add-on
  }
}
```

---

## 3. `terraform apply` Pool B

```bash
cd /Users/evgeny/neksur-infra/environments/phase0-pilot
terraform plan -target="module.rds_pool_b[\"$CUSTOMER_UUID\"]"
# Review the plan — should show ~15 resources (primary + replica EC2 +
# EBS + S3 bucket + IAM role + SG + cloud-init user-data attach).
terraform apply -target="module.rds_pool_b[\"$CUSTOMER_UUID\"]"
```

Expected wall-clock: ~8–12 minutes (EC2 + EBS + cloud-init bringing up
Postgres + AGE + Patroni + pgBackRest).

---

## 4. Wait for the Pool B endpoint to be available

```bash
POOL_B_ENDPOINT=$(terraform -chdir=/Users/evgeny/neksur-infra/environments/phase0-pilot output -json pool_b_endpoints | jq -r ".\"$CUSTOMER_UUID\"")
echo "Pool B endpoint: $POOL_B_ENDPOINT"

# Poll for pgwire reachability from a jumphost inside the Neksur VPC:
for i in $(seq 1 60); do
  pg_isready -h "$POOL_B_ENDPOINT" && break
  echo "  waiting for Pool B pgwire... ($i/60)"
  sleep 10
done
```

If pgwire never becomes ready within 10 min, SSH-via-SSM into the Pool B
primary and check `journalctl -u patroni -n 200` for cloud-init failures.

---

## 5. Create the tenant on Pool B

The application path normally creates tenants on Pool A. Pool B = dedicated
instance — the tenant create flow is slightly different: instead of
shelling out to the shared Pool A admin pool, the operator passes the
Pool B endpoint as `DATABASE_URL`:

```bash
TENANT_UUID="<the customer's tenant UUID (typically matches CUSTOMER_UUID for greenfield Pool B onboardings)>"
DATABASE_URL="postgres://admin:$POOL_B_ADMIN_PASSWORD@$POOL_B_ENDPOINT/neksur" \
  ./neksur-cli tenant create \
    --tenant-uuid "$TENANT_UUID" \
    --workos-org "<customer's WorkOS org id>"
```

This runs Plan 04's `create_graph` + `CREATE ROLE tenant_<uuid>_role`
against the customer's dedicated Pool B instance.

---

## 6. Apply the Atlas tenant-loop migrations

```bash
DATABASE_URL="postgres://admin:$POOL_B_ADMIN_PASSWORD@$POOL_B_ENDPOINT/neksur" \
  ./neksur-cli tenant migrate \
    --tenant-uuid "$TENANT_UUID" \
    --step apply-versioned
```

This applies V0050+V0051+V0052 (audit_log + query_history + policies)
inside `tenant_<uuid>` on Pool B AND runs the
`RevokeAuditLogWrites` step (T-0.5-audit-tamper).

---

## 7. Issue the mTLS client cert (pgwire SQL Proxy)

```bash
./neksur-cli tenant cert-issue --tenant-uuid "$TENANT_UUID"
```

The cert bundle is the customer's Spark client identity for connecting
to `sql.neksur.com` (D-0.5.08). Ship the bundle to the customer via
the secure channel agreed in their onboarding kit.

---

## 8. Smoke tests

```bash
DATABASE_URL="postgres://admin:$POOL_B_ADMIN_PASSWORD@$POOL_B_ENDPOINT/neksur" \
  ./neksur-cli tenant smoke --tenant-uuid "$TENANT_UUID"
```

The smoke flow runs:
- Gateway commit + audit edge write
- Policy fetch from `tenant_<uuid>.policies`
- Cross-tenant probe (negative test — expects permission denied)

All three must PASS. On any FAIL, do not hand the Pool B over to the
customer; investigate first.

---

## 9. Hand off to customer

Deliver the onboarding kit:
- Pool B pgwire endpoint (`$POOL_B_ENDPOINT`)
- pgwire mTLS client cert bundle (`./neksur-cli tenant cert-issue` output)
- WorkOS organization invitation link (separate flow via WorkOS admin UI)
- VPC peering customer-side Terraform module (see `runbooks/saas-onboarding.md`
  Plan 04 — Pool B onboarding includes peering identical to Pool A)

---

## 10. Cross-references

- `/Users/evgeny/neksur-infra/modules/rds-pool-b/` — Terraform module
- `/Users/evgeny/neksur-core/internal/tenant/` — Go provisioning surface
- `/Users/evgeny/neksur-core/scripts/dry-run-pool-b.sh` — nightly cron
  rehearsal of the full lifecycle
- `runbooks/pool-b-migration.md` — Pool A → Pool B migration for existing
  customers
- `runbooks/restore-pitr.md` §9 — Pool B-specific restore guidance
- `runbooks/saas-onboarding.md` — Pool A onboarding (cross-reference for
  the steps common to both pools)
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-CONTEXT.md`
  D-0.5.02 — Pool B contract
