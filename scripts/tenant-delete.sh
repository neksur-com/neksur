#!/usr/bin/env bash
# scripts/tenant-delete.sh — Plan 07 D-0.5.20 IRREVERSIBLE terminal transition.
#
# * → deleted: schema drop + lifecycle_state='deleted' + audit_log row +
# terraform destroy customer_peering. 30-day RDS backup retention (Plan 01
# pgBackRest) provides recovery window.
#
# Defence in depth (T-0.5-accidental-delete):
#   • UUID v4 regex validator BEFORE any CLI invocation.
#   • Optional --yes flag for unattended/cron use; without --yes the
#     script prompts the operator to type the literal string
#     `DELETE <uuid>` to confirm. Any other input aborts.
#   • Downstream CLI also requires --yes (refuses to delete otherwise).
#
# Phase: 00.5-saas-pilot-infrastructure (Plan 07)
# Requirements: REQ-saas-tenancy-pool-a (lifecycle state machine)
# Threats mitigated:
#   T-0.5-accidental-delete — dual-gate confirmation (interactive prompt OR --yes).
#   T-0.5-prov-injection — regex validator BEFORE any CLI call.
#   T-0.5-partial-delete-state — CLI logs WARN if terraform destroy fails;
#     DB delete already committed; operator runs terraform destroy manually.
# Runbook: runbooks/tenant-lifecycle.md
#
# Usage:
#   ./scripts/tenant-delete.sh <tenant-uuid>          # interactive, prompts for DELETE <uuid>
#   ./scripts/tenant-delete.sh <tenant-uuid> --yes    # unattended (CI / cron)
#
# Required env:
#   DATABASE_URL    Admin DSN.
#
# Optional env:
#   TF_DIR                 Absolute path to neksur-infra/environments/phase0-pilot
#                          (for the terraform destroy step). If unset, CLI prints
#                          a manual cleanup command and continues.
#   SLACK_OPS_WEBHOOK_URL  Slack ops webhook; unset → no notification.
#   NEKSUR_CLI             Path to neksur-cli binary; default "./neksur-cli".
#   ACTOR                  Operator identifier for audit_log; default delete@neksur.com.
#   SKIP_TERRAFORM         If "1", pass --skip-terraform to the CLI (no peering destroy).

set -euo pipefail

TENANT_UUID="${1:?tenant-uuid required (UUID v4)}"
shift || true

YES_FLAG=""
for arg in "$@"; do
  case "$arg" in
    --yes) YES_FLAG="--yes" ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

NEKSUR_CLI="${NEKSUR_CLI:-./neksur-cli}"
ACTOR="${ACTOR:-delete@neksur.com}"

# T-0.5-prov-injection: UUID v4 regex validator BEFORE any CLI call.
[[ "$TENANT_UUID" =~ ^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$ ]] \
  || { echo "invalid UUID v4: $TENANT_UUID" >&2; exit 2; }

# --- Interactive confirmation gate (unless --yes given) -------------------
# T-0.5-accidental-delete: operator must type `DELETE <uuid>` exactly.
# Modeled on tests/dr/restore_pit.sh wipe-confirmation idiom (PATTERNS.md
# Group G line 609) — the prompt is the last line of defence against typos.
if [[ -z "$YES_FLAG" ]]; then
  echo ""
  echo "================================================================"
  echo "  IRREVERSIBLE DELETE — tenant $TENANT_UUID"
  echo "================================================================"
  echo "  This will:"
  echo "    1. UPDATE public.tenants.lifecycle_state → 'deleted'"
  echo "    2. SELECT drop_graph('tenant_${TENANT_UUID//-/_}', true) — drops schema"
  echo "    3. INSERT public.system_audit_log event_type='tenant.deleted'"
  echo "    4. terraform destroy -target=module.customer_peering[$TENANT_UUID]"
  echo ""
  echo "  Recovery is only possible from 30-day RDS backup (Plan 01 pgBackRest)."
  echo ""
  printf "  To confirm, type exactly:  DELETE %s\n  > " "$TENANT_UUID"
  read -r CONFIRM
  if [[ "$CONFIRM" != "DELETE $TENANT_UUID" ]]; then
    echo "ABORTED: confirmation phrase did not match." >&2
    exit 1
  fi
  echo ""
  echo "==> Confirmed. Proceeding with delete."
fi

# --- Build CLI args -------------------------------------------------------
SKIP_TF_ARG=""
if [[ "${SKIP_TERRAFORM:-}" == "1" ]]; then
  SKIP_TF_ARG="--skip-terraform"
fi

# --- Invoke CLI with --yes (CLI also enforces --yes as defence in depth) --
"$NEKSUR_CLI" tenant delete \
  --tenant-uuid "$TENANT_UUID" \
  --yes \
  --actor "$ACTOR" \
  ${SKIP_TF_ARG:+$SKIP_TF_ARG}

if [[ -n "${SLACK_OPS_WEBHOOK_URL:-}" ]]; then
  echo "==> Slack ops notification"
  curl -fsS -X POST -H "Content-Type: application/json" \
    -d "{\"text\":\":skull: Tenant DELETED (irreversible): \`$TENANT_UUID\` (actor=$ACTOR)\"}" \
    "$SLACK_OPS_WEBHOOK_URL" \
    || echo "    (Slack POST failed — non-fatal)"
else
  echo "==> Slack notification skipped (SLACK_OPS_WEBHOOK_URL not set)"
fi

echo ""
echo "================================================================"
echo "  Tenant $TENANT_UUID DELETED."
echo "================================================================"
echo "  • Schema tenant_${TENANT_UUID//-/_} dropped."
echo "  • public.tenants.lifecycle_state = 'deleted'"
echo "  • 30-day RDS backup retention (pgBackRest) provides recovery window."
echo "  • See runbooks/tenant-lifecycle.md §4 for backup-recovery procedure."
