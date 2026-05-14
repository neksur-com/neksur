#!/usr/bin/env bash
# scripts/provision-tenant.sh — Phase 0.5 manual tenant onboarding.
#
# D-0.5.19 12-step idempotent flow. Each step is a single ./neksur-cli
# tenant <verb> call. Re-run safe: every CLI subcommand is itself
# idempotent (IF NOT EXISTS / ON CONFLICT DO NOTHING / regex-checked-
# then-create), so re-running on a partially-onboarded tenant resumes
# from wherever the previous run stopped.
#
# Phase: 00.5-saas-pilot-infrastructure (Plan 04)
# Requirements: REQ-saas-tenancy-pool-a, REQ-saas-onboarding
# Threats mitigated:
#   T-0.5-prov-injection — regex validators here (defence in depth on top
#     of the Go-side ValidateUUIDv4 / ValidateWorkOSOrgID etc.) reject
#     malformed inputs with exit code 2 BEFORE any psql/terraform call.
#   T-0.5-peering-async-fail — step (j) prints "Re-run when customer
#     applies their accepter module" and exits 0 (graceful async) when
#     the peering is not yet ACTIVE.
# Runbook: runbooks/saas-onboarding.md (Plan 07)
#
# Usage:
#   ./scripts/provision-tenant.sh <tenant-uuid> <workos-org-id> <customer-vpc-id> <customer-vpc-region>
#
# Required env:
#   DATABASE_URL          Admin DSN (postgres://admin@...:5432/postgres)
#   TF_DIR                Absolute path to neksur-infra/environments/phase0-pilot
#   PRIVATE_CA_ARN        AWS Private CA ARN (Plan 01 modules/private-ca output)
#
# Optional env:
#   SLACK_OPS_WEBHOOK_URL Slack incoming-webhook URL for step (l) notification.
#                         If unset, the Slack step is skipped (no notification).
#   NEKSUR_CLI            Path to the neksur-cli binary; default "./neksur-cli".

set -euo pipefail

# --- Input parsing ----------------------------------------------------
TENANT_UUID="${1:?tenant-uuid required (UUID v4)}"
WORKOS_ORG_ID="${2:?workos-org-id required (org_...)}"
CUST_VPC_ID="${3:?customer-vpc-id required (vpc-...)}"
CUST_VPC_REGION="${4:?customer-vpc-region required}"

NEKSUR_CLI="${NEKSUR_CLI:-./neksur-cli}"

# --- D-0.5.21 T-0.5-prov-injection: regex validators FIRST ------------
# Bash-side gate. The Go CLI also re-validates; this is defence in
# depth — if a future code change weakens the Go validators, this gate
# still rejects the malformed input before any psql interpolation.
[[ "$TENANT_UUID" =~ ^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$ ]] \
  || { echo "invalid UUID v4: $TENANT_UUID" >&2; exit 2; }
[[ "$WORKOS_ORG_ID" =~ ^org_[A-Z0-9]+$ ]] \
  || { echo "invalid workos org id: $WORKOS_ORG_ID" >&2; exit 2; }
[[ "$CUST_VPC_ID" =~ ^vpc-[0-9a-f]{17}$ ]] \
  || { echo "invalid VPC id: $CUST_VPC_ID" >&2; exit 2; }
[[ "$CUST_VPC_REGION" =~ ^[a-z]{2}-[a-z]+-[0-9]$ ]] \
  || { echo "invalid region: $CUST_VPC_REGION" >&2; exit 2; }

echo "==> Provisioning tenant $TENANT_UUID"
echo "    workos_org   = $WORKOS_ORG_ID"
echo "    customer_vpc = $CUST_VPC_ID ($CUST_VPC_REGION)"

# --- Step (a)+(b) — WorkOS org + public.tenants row -------------------
echo "==> Step (a)+(b) tenant create (public.tenants INSERT + create_graph + role)"
"$NEKSUR_CLI" tenant create \
  --tenant-uuid "$TENANT_UUID" \
  --workos-org "$WORKOS_ORG_ID"

# --- Step (c) — AGE create_graph + Postgres role + GRANTs -------------
# tenant create already did (a/b/c/d). This explicit step is the runbook
# checkpoint for operators re-running from a partial state — if (a/b)
# is already done, this still runs (c/d) safely (idempotent).
echo "==> Step (c)+(d) tenant migrate bootstrap-schema"
"$NEKSUR_CLI" tenant migrate \
  --tenant-uuid "$TENANT_UUID" \
  --step bootstrap-schema

# --- Step (e) — Atlas tenant-loop apply V0050+V0051+V0052 -------------
echo "==> Step (e) tenant migrate apply-versioned (V0050+V0051+V0052)"
"$NEKSUR_CLI" tenant migrate \
  --tenant-uuid "$TENANT_UUID" \
  --step apply-versioned

# --- Step (f)+(g) — covered by Atlas above (audit_log/query_history/policies)
echo "==> Step (f)+(g) (covered by step (e) — audit_log / query_history / policies created)"

# --- Step (h) — Neksur-side VPC peering -------------------------------
echo "==> Step (h) tenant peer (terraform apply customer_peering)"
"$NEKSUR_CLI" tenant peer \
  --tenant-uuid "$TENANT_UUID" \
  --customer-vpc "$CUST_VPC_ID" \
  --customer-region "$CUST_VPC_REGION"

# --- Step (i) — print customer-side Terraform module ------------------
echo "==> Step (i) Customer-side Terraform module — paste this to the customer:"
echo "----- BEGIN CUSTOMER MODULE -----"
"$NEKSUR_CLI" tenant peer \
  --tenant-uuid "$TENANT_UUID" \
  --show-customer-module
echo "----- END CUSTOMER MODULE -----"

# --- Step (j) — async peering acceptance gate -------------------------
# RESEARCH Pattern 3 line 681 — graceful re-run pattern. If peering is
# not yet active (customer hasn't applied their accepter), exit 0 so
# the operator can re-run after the customer is done.
echo "==> Step (j) Polling VPC peering status..."
PEER_STATUS=$("$NEKSUR_CLI" tenant peer --tenant-uuid "$TENANT_UUID" --status)
echo "    peering status = $PEER_STATUS"
if [[ "$PEER_STATUS" != "active" ]]; then
  echo "==> Peering not yet active ($PEER_STATUS)."
  echo "    Re-run this script after customer applies their accepter module."
  exit 0
fi

# --- Step (k) — smoke tests -------------------------------------------
echo "==> Step (k) tenant smoke (gateway audit + policy fetch + cross-tenant probe)"
"$NEKSUR_CLI" tenant smoke --tenant-uuid "$TENANT_UUID"

# --- Step (l) — Slack notification (optional) -------------------------
if [[ -n "${SLACK_OPS_WEBHOOK_URL:-}" ]]; then
  echo "==> Step (l) Slack notification"
  curl -fsS -X POST -H "Content-Type: application/json" \
    -d "{\"text\":\":white_check_mark: Tenant onboarded: \`$TENANT_UUID\` (org=$WORKOS_ORG_ID, customer_vpc=$CUST_VPC_ID/$CUST_VPC_REGION)\"}" \
    "$SLACK_OPS_WEBHOOK_URL" \
    || echo "    (Slack POST failed — non-fatal)"
else
  echo "==> Step (l) skipped (SLACK_OPS_WEBHOOK_URL not set)"
fi

echo ""
echo "================================================================"
echo "  Tenant $TENANT_UUID onboarded successfully."
echo "================================================================"
echo "    schema       = tenant_${TENANT_UUID//-/_}"
echo "    role         = tenant_${TENANT_UUID//-/_}_role"
echo "    workos_org   = $WORKOS_ORG_ID"
echo "    customer_vpc = $CUST_VPC_ID ($CUST_VPC_REGION)"
echo ""
echo "Next steps:"
echo "  1. Hand off the mTLS client cert bundle to the customer (see step (i) output)"
echo "  2. Add the tenant to the on-call rotation per runbooks/saas-onboarding.md"
echo "  3. Verify the entry at admin.neksur.com/tenants"
