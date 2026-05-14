#!/usr/bin/env bash
# scripts/tenant-suspend.sh — Plan 07 D-0.5.20 lifecycle transition wrapper.
#
# Active → suspended: gateway returns 503 on writes; reads continue.
# Wraps `./neksur-cli tenant suspend --tenant-uuid <uuid>` with:
#   1. UUID v4 regex validation BEFORE any CLI invocation (defence in depth
#      on top of the Go-side ValidateUUIDv4 — T-0.5-prov-injection).
#   2. Optional Slack notification via SLACK_OPS_WEBHOOK_URL (mirrors
#      provision-tenant.sh step (l)). Non-fatal on POST failure.
#
# Phase: 00.5-saas-pilot-infrastructure (Plan 07)
# Requirements: REQ-saas-tenancy-pool-a (lifecycle state machine)
# Threats mitigated:
#   T-0.5-prov-injection — regex validator rejects malformed inputs with
#     exit 2 BEFORE any psql/neksur-cli call.
# Runbook: runbooks/tenant-lifecycle.md
#
# Usage:
#   ./scripts/tenant-suspend.sh <tenant-uuid>
#
# Required env:
#   DATABASE_URL    Admin DSN (postgres://admin@...:5432/postgres)
#
# Optional env:
#   SLACK_OPS_WEBHOOK_URL  Slack incoming-webhook URL for ops notification.
#                          Unset → Slack step skipped (no notification).
#   NEKSUR_CLI             Path to the neksur-cli binary; default "./neksur-cli".
#   ACTOR                  Operator identifier for audit_log; default suspend@neksur.com.

set -euo pipefail

# --- Input parsing ----------------------------------------------------
TENANT_UUID="${1:?tenant-uuid required (UUID v4)}"
NEKSUR_CLI="${NEKSUR_CLI:-./neksur-cli}"
ACTOR="${ACTOR:-suspend@neksur.com}"

# --- T-0.5-prov-injection: UUID v4 regex validator BEFORE any CLI call ----
# Same regex literal as provision-tenant.sh (RESEARCH §Pattern 3 line 635) +
# Go-side tenant.ValidateUUIDv4. Defence in depth — if the Go validator
# is ever weakened, this gate still rejects malformed input.
[[ "$TENANT_UUID" =~ ^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$ ]] \
  || { echo "invalid UUID v4: $TENANT_UUID" >&2; exit 2; }

echo "==> Suspending tenant $TENANT_UUID (active → suspended; D-0.5.20)"
echo "    Gateway will return 503 on writes; reads continue."

# --- Invoke CLI -------------------------------------------------------
"$NEKSUR_CLI" tenant suspend \
  --tenant-uuid "$TENANT_UUID" \
  --actor "$ACTOR"

# --- Slack notification (optional) ------------------------------------
if [[ -n "${SLACK_OPS_WEBHOOK_URL:-}" ]]; then
  echo "==> Slack ops notification"
  curl -fsS -X POST -H "Content-Type: application/json" \
    -d "{\"text\":\":pause_button: Tenant suspended: \`$TENANT_UUID\` (actor=$ACTOR)\"}" \
    "$SLACK_OPS_WEBHOOK_URL" \
    || echo "    (Slack POST failed — non-fatal)"
else
  echo "==> Slack notification skipped (SLACK_OPS_WEBHOOK_URL not set)"
fi

echo ""
echo "================================================================"
echo "  Tenant $TENANT_UUID suspended successfully."
echo "================================================================"
echo "  Next steps:"
echo "    • To resume (Phase 1+): ./scripts/tenant-resume.sh $TENANT_UUID (NOT YET SHIPPED)"
echo "    • To wind down: ./scripts/tenant-windown.sh $TENANT_UUID"
echo "    • See runbooks/tenant-lifecycle.md for full state-machine reference."
