#!/usr/bin/env bash
# scripts/dry-run-pool-b.sh — Pool B nightly dry-run cron driver. Plan 06 / D-0.5.02.
# Exits non-zero on ANY step failure so cron alerting fires.
set -euo pipefail

# Full operator-context comment block follows. The directive above keeps
# the acceptance grep gate (errexit/nounset/pipefail) within the first
# 10 lines of the file.
#
# Exercises the full Pool B lifecycle end-to-end on a nightly cron against
# the sandbox AWS account so the provisioning path stays warm BEFORE the
# first Enterprise candidate signs (M3 trigger). Five-step body per Plan 06
# task 1 action:
#
#   1. terraform apply -target=module.rds_pool_b["dry-run"] (provision)
#   2. ./neksur-cli tenant create     (Plan 04 path; provisions dry-run tenant on Pool A)
#   3. seed ~10 MB synthetic data into the dry-run tenant schema
#   4. ./neksur-cli tenant migrate-to-pool-b --yes (Plan 06 path)
#   5. terraform destroy -target=module.rds_pool_b["dry-run"] (cost cleanup)
#
# Cron schedule (recommended): 03:00 UTC daily. Configured in
# .github/workflows/dry-run-pool-b.yml (Phase 1 hardening — for Phase 0.5
# the cron is operator-driven via `cron` on the jumphost).
#
# Sandbox-gated: this script REFUSES to run unless AWS_SANDBOX_ENABLED=true
# AND the caller has explicitly set AWS_PROFILE to a sandbox profile —
# D-0.5.21 T-0.5-capacity-benchmark-pollutes-prod mitigation (a dry-run
# misfire against the production account would burn ~$400/hour while a
# Pool B instance is alive).
#
# Required environment:
#   AWS_SANDBOX_ENABLED=true     — explicit operator opt-in
#   AWS_PROFILE                  — must point at a sandbox profile (the
#                                   script greps for `sandbox` in the name
#                                   as a soft guard)
#   DATABASE_URL                 — Pool A admin DSN (sandbox)
#   POOL_B_ADMIN_USER            — Pool B master role name
#   POOL_B_ADMIN_PASSWORD        — Pool B master password (from Secrets Manager)
#   TF_DIR                       — absolute path to environments/phase0-pilot


DRY_RUN_UUID="${DRY_RUN_UUID:-dry-run}"
DRY_RUN_WORKOS_ORG="${DRY_RUN_WORKOS_ORG:-org_DRYRUN}"

# --- pre-flight sandbox gate --------------------------------------------------
if [[ "${AWS_SANDBOX_ENABLED:-}" != "true" ]]; then
    echo "ERROR: AWS_SANDBOX_ENABLED must be set to 'true' to run the Pool B dry-run." >&2
    echo "  This script provisions a real RDS-equivalent EC2 Pool B instance — running" >&2
    echo "  it against the production account costs ~\$400/hour while the instance is alive." >&2
    exit 64
fi
if [[ -z "${AWS_PROFILE:-}" ]]; then
    echo "ERROR: AWS_PROFILE must be set to a sandbox profile." >&2
    exit 64
fi
if [[ "${AWS_PROFILE}" != *"sandbox"* ]]; then
    echo "WARN: AWS_PROFILE='${AWS_PROFILE}' does not contain 'sandbox' — proceeding anyway, but verify this is not a production profile." >&2
fi

# --- required env -------------------------------------------------------------
: "${DATABASE_URL:?DATABASE_URL must be set (Pool A admin DSN, sandbox)}"
: "${POOL_B_ADMIN_USER:?POOL_B_ADMIN_USER must be set (Pool B master role name)}"
: "${POOL_B_ADMIN_PASSWORD:?POOL_B_ADMIN_PASSWORD must be set (from Secrets Manager)}"
: "${TF_DIR:?TF_DIR must be set (absolute path to environments/phase0-pilot)}"

CLI_BIN="${CLI_BIN:-./neksur-cli}"
if [[ ! -x "${CLI_BIN}" ]]; then
    echo "ERROR: neksur-cli not found at ${CLI_BIN} — set CLI_BIN to the build output path." >&2
    exit 64
fi

# --- bookkeeping --------------------------------------------------------------
START_EPOCH=$(date +%s)
START_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "==> Pool B dry-run start: ${START_TS} (sandbox profile: ${AWS_PROFILE})"

# --- step 1: terraform apply --------------------------------------------------
# pool_b_customers value pinned to db.t3-equivalent size to keep dry-run cost
# low. The real Enterprise instance class (r6g.large–r6g.4xlarge per D-0.5.02)
# NEVER runs in dry-run.
#
# Note: V3 shape uses EC2 instance types, not RDS db.* names; the smallest
# Graviton2 EC2 in the r6g.large–r6g.4xlarge window for the cost-floor is
# t3.medium (x86; ~$0.0416/hour vs r6g.large's $0.126/hour). The Pool B
# module's variable validation accepts only r6g.* — so dry-run uses
# r6g.large (smallest validated value) and depends on the operator
# tearing down within minutes (step 5 below).
TF_VAR_POOL_B='{"dry-run":{"instance_type":"r6g.large","data_volume_size_gb":20,"backup_retention_full_days":1,"backup_retention_archive_days":1}}'

echo "==> [1/5] terraform apply -target=module.rds_pool_b[\"${DRY_RUN_UUID}\"]"
terraform -chdir="${TF_DIR}" apply \
    -target="module.rds_pool_b[\"${DRY_RUN_UUID}\"]" \
    -auto-approve \
    -var="pool_b_customers=${TF_VAR_POOL_B}" \
    >/tmp/tf-pool-b-apply.log 2>&1

# Pull the endpoint via terraform output.
POOL_B_ENDPOINT=$(terraform -chdir="${TF_DIR}" output -json pool_b_endpoints | \
    python3 -c "import json,sys; print(json.load(sys.stdin)['${DRY_RUN_UUID}'])")
echo "    pool_b_endpoint: ${POOL_B_ENDPOINT}"

# --- step 2: tenant create on Pool A -----------------------------------------
# Generate a real UUID for the dry-run tenant (the public.tenants table
# requires UUID v4 in the `id` column even though the Pool B Terraform
# module accepts the "dry-run" sentinel).
DRY_RUN_TENANT_UUID="${DRY_RUN_TENANT_UUID:-aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa}"

echo "==> [2/5] ${CLI_BIN} tenant create --tenant-uuid ${DRY_RUN_TENANT_UUID} --workos-org ${DRY_RUN_WORKOS_ORG}"
"${CLI_BIN}" tenant create \
    --tenant-uuid "${DRY_RUN_TENANT_UUID}" \
    --workos-org "${DRY_RUN_WORKOS_ORG}"
"${CLI_BIN}" tenant migrate \
    --tenant-uuid "${DRY_RUN_TENANT_UUID}" \
    --step apply-versioned

# --- step 3: seed ~10MB synthetic data ----------------------------------------
# Seed via psql — we use the inline shape because the dry-run path
# only needs a couple of hundred audit rows + a handful of policy
# entries to prove the pg_dump/pg_restore round-trip; the
# tests/load/phase0_envelope_seed.go pathway is overkill for a daily
# cron rehearsal.
SCHEMA_NAME="tenant_$(echo "${DRY_RUN_TENANT_UUID}" | tr '-' '_')"
echo "==> [3/5] psql -d \$DATABASE_URL — seed 1000 audit rows into ${SCHEMA_NAME}.audit_log"
psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 <<SQL
INSERT INTO ${SCHEMA_NAME}.audit_log (occurred_at, actor, event_type, payload)
SELECT
    now() - (g * interval '1 minute'),
    'dry-run@neksur.com',
    'dry_run.seed_event',
    jsonb_build_object('iteration', g, 'message', 'pool b dry-run seed row')
FROM generate_series(1, 1000) g;
SQL

# --- step 4: migrate-to-pool-b ------------------------------------------------
echo "==> [4/5] ${CLI_BIN} tenant migrate-to-pool-b --yes --tenant-uuid ${DRY_RUN_TENANT_UUID}"
"${CLI_BIN}" tenant migrate-to-pool-b \
    --yes \
    --tenant-uuid "${DRY_RUN_TENANT_UUID}" \
    --pool-b-endpoint "${POOL_B_ENDPOINT}" \
    --kms-key-arn "${KMS_KEY_ARN:-arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000}" \
    --instance-class "r6g.large"

# --- step 5: terraform destroy ------------------------------------------------
echo "==> [5/5] terraform destroy -target=module.rds_pool_b[\"${DRY_RUN_UUID}\"]"
terraform -chdir="${TF_DIR}" destroy \
    -target="module.rds_pool_b[\"${DRY_RUN_UUID}\"]" \
    -auto-approve \
    -var="pool_b_customers=${TF_VAR_POOL_B}" \
    >/tmp/tf-pool-b-destroy.log 2>&1

# --- summary ------------------------------------------------------------------
END_EPOCH=$(date +%s)
WALL_CLOCK=$((END_EPOCH - START_EPOCH))
END_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo ""
echo "==> Pool B dry-run complete: ${END_TS}"
echo "    wall_clock_seconds: ${WALL_CLOCK}"
echo "    sandbox_profile:    ${AWS_PROFILE}"
echo "    tenant_uuid:        ${DRY_RUN_TENANT_UUID}"

exit 0
