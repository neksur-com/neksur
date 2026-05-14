#!/usr/bin/env bash
# scripts/tenant-windown.sh — Plan 07 D-0.5.20 lifecycle transition wrapper.
#
# Active|suspended → wind_down: 30-day post-cancellation read-only window
# starts now. Customer can still download audit_log + policies; no new
# writes accepted. After 30 days (Phase 1+ cron OR manual), wind_down
# transitions to deleted via ./scripts/tenant-delete.sh.
#
# Phase: 00.5-saas-pilot-infrastructure (Plan 07)
# Requirements: REQ-saas-tenancy-pool-a (lifecycle state machine)
# Threats mitigated:
#   T-0.5-prov-injection — regex validator BEFORE any CLI invocation.
# Runbook: runbooks/tenant-lifecycle.md
#
# Usage:
#   ./scripts/tenant-windown.sh <tenant-uuid>
#
# Required env:
#   DATABASE_URL    Admin DSN.
#
# Optional env:
#   SLACK_OPS_WEBHOOK_URL  Slack ops webhook; unset → no notification.
#   NEKSUR_CLI             Path to neksur-cli binary; default "./neksur-cli".
#   ACTOR                  Operator identifier for audit_log; default winddown@neksur.com.

set -euo pipefail

TENANT_UUID="${1:?tenant-uuid required (UUID v4)}"
NEKSUR_CLI="${NEKSUR_CLI:-./neksur-cli}"
ACTOR="${ACTOR:-winddown@neksur.com}"

# T-0.5-prov-injection: UUID v4 regex validator BEFORE any CLI call.
[[ "$TENANT_UUID" =~ ^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$ ]] \
  || { echo "invalid UUID v4: $TENANT_UUID" >&2; exit 2; }

echo "==> Winding down tenant $TENANT_UUID (active|suspended → wind_down; D-0.5.20)"
echo "    30-day post-cancellation read-only window starts now."
echo "    Customer may download audit_log + policies; no new writes accepted."

"$NEKSUR_CLI" tenant wind-down \
  --tenant-uuid "$TENANT_UUID" \
  --actor "$ACTOR"

if [[ -n "${SLACK_OPS_WEBHOOK_URL:-}" ]]; then
  echo "==> Slack ops notification"
  curl -fsS -X POST -H "Content-Type: application/json" \
    -d "{\"text\":\":wastebasket: Tenant wind-down started (30-day clock): \`$TENANT_UUID\` (actor=$ACTOR)\"}" \
    "$SLACK_OPS_WEBHOOK_URL" \
    || echo "    (Slack POST failed — non-fatal)"
else
  echo "==> Slack notification skipped (SLACK_OPS_WEBHOOK_URL not set)"
fi

DELETE_AFTER=$(date -u -v+30d +%Y-%m-%d 2>/dev/null || date -u -d '+30 days' +%Y-%m-%d 2>/dev/null || echo "+30 days from now")

echo ""
echo "================================================================"
echo "  Tenant $TENANT_UUID wind-down started successfully."
echo "================================================================"
echo "  Auto-delete eligible on or after: $DELETE_AFTER"
echo "  Final delete: ./scripts/tenant-delete.sh $TENANT_UUID --yes"
echo "  See runbooks/tenant-lifecycle.md for full state-machine reference."
