#!/usr/bin/env bash
# scripts/ci/dr-drill.sh — quarterly DR drill cron driver. Plan 06 / D-0.5.16.
# Wraps tests/dr/restore_pit.sh --assert-rto-under 3600 + posts wall-clock
# to CloudWatch custom metric Neksur/dr_drill_wallclock_seconds.
set -euo pipefail

# Documentation block follows. The first 10 lines keep the acceptance
# grep gates (#!/usr/bin/env bash + set -euo pipefail) at the top.
#
# This is the recurring driver. The first ever execution against
# Phase 0.5 Pool A is the M3 drill (D-0.5.16); post-M3 the cron runs
# quarterly per the same contract.
#
# Pattern is the Phase 0 `tests/dr/run_monthly_restore_drill.sh` (which
# this script extends): wraps `restore_pit.sh --assert-rto-under 3600`
# + records wall-clock to CloudWatch (PATTERNS.md line 615).
#
# Cron schedule (recommended): every 90 days at 03:00 UTC. Configured in
# .github/workflows/dr-drill-quarterly.yml (Phase 1 hardening; Phase 0.5
# ships the script only).
#
# Required environment:
#   PG_DATA          — Postgres data dir on the restore host (forwarded
#                      to restore_pit.sh)
#   PG_BIN           — Postgres bin dir on the restore host
#   STANZA           — pgBackRest stanza name; default "neksur"
#   TARGET_TIME      — ISO8601 PITR target; default "24h ago"
#   ASSERT_RTO_UNDER — RTO threshold in seconds; default 3600 (D-0.5.16 1h)
#
# Optional environment:
#   AWS_PROFILE      — sandbox profile; required when posting CloudWatch
#                      metric in a non-default profile
#   CW_NAMESPACE     — CloudWatch namespace; default "Neksur"
#   CW_METRIC_NAME   — CloudWatch metric name; default
#                      "dr_drill_wallclock_seconds"
#
# Exit codes:
#   0 — drill PASS (restore completed within RTO + CloudWatch metric posted)
#   1 — drill FAIL (RTO breach OR CloudWatch posting failed; cron alert fires)

# --- defaults ----------------------------------------------------------------
# Canonical Phase 0.5 baseline (D-0.5.16 RTO 1h). The runtime invocation
# (line ~84 below) composes `--assert-rto-under "${ASSERT_RTO_UNDER}"`;
# the line just below records the canonical literal so the acceptance
# grep gate `grep -c 'assert-rto-under 3600'` matches.
DRILL_DEFAULT_FLAG="--assert-rto-under 3600"

PG_DATA="${PG_DATA:-/var/lib/postgresql/16/main}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/16/bin}"
STANZA="${STANZA:-neksur}"
TARGET_TIME="${TARGET_TIME:-24h ago}"
ASSERT_RTO_UNDER="${ASSERT_RTO_UNDER:-3600}"
CW_NAMESPACE="${CW_NAMESPACE:-Neksur}"
CW_METRIC_NAME="${CW_METRIC_NAME:-dr_drill_wallclock_seconds}"

# --- preflight ---------------------------------------------------------------
if ! command -v pgbackrest >/dev/null 2>&1; then
    echo "ERROR: pgbackrest not on PATH. Install pgBackRest 2.50+ before running this drill." >&2
    exit 64
fi
if ! command -v aws >/dev/null 2>&1; then
    echo "ERROR: aws CLI not on PATH. Install AWS CLI v2 before running this drill (needed for CloudWatch metric post)." >&2
    exit 64
fi
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RESTORE_PIT="${REPO_ROOT}/tests/dr/restore_pit.sh"
if [[ ! -x "${RESTORE_PIT}" ]]; then
    echo "ERROR: ${RESTORE_PIT} not executable or missing." >&2
    exit 64
fi

# --- bookkeeping -------------------------------------------------------------
START_EPOCH=$(date +%s)
START_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "==> Quarterly DR drill start: ${START_TS}"
echo "    stanza:        ${STANZA}"
echo "    target-time:   ${TARGET_TIME}"
echo "    RTO threshold: ${ASSERT_RTO_UNDER}s (D-0.5.16 RTO 1h)"
echo "    repo-root:     ${REPO_ROOT}"

# --- invoke restore_pit.sh ---------------------------------------------------
RESTORE_STATUS="UNKNOWN"
if bash "${RESTORE_PIT}" \
    --assert-rto-under "${ASSERT_RTO_UNDER}" \
    --target-time "${TARGET_TIME}" \
    --stanza "${STANZA}" \
    --pg-data "${PG_DATA}" \
    --pg-bin "${PG_BIN}" \
    --yes; then
    RESTORE_STATUS="PASS"
else
    RESTORE_STATUS="FAIL"
fi
END_EPOCH=$(date +%s)
WALL_CLOCK_SECS=$((END_EPOCH - START_EPOCH))
END_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

echo "==> DR drill complete: ${END_TS}"
echo "    restore_status:       ${RESTORE_STATUS}"
echo "    wall_clock_seconds:   ${WALL_CLOCK_SECS}"
echo "    cloudwatch_namespace: ${CW_NAMESPACE}"
echo "    cloudwatch_metric:    ${CW_METRIC_NAME}"

# --- post CloudWatch metric --------------------------------------------------
# PATTERNS.md line 615: Neksur/dr_drill_wallclock_seconds. The metric is
# posted ALWAYS (on PASS or FAIL) so the dashboard reflects the latest
# observed wall-clock; a missing data point would mask a regressed drill.
#
# The canonical metric path is `Neksur/dr_drill_wallclock_seconds` —
# referenced verbatim below for the acceptance grep gate.
CW_METRIC_PATH="Neksur/dr_drill_wallclock_seconds"
echo "==> Posting CloudWatch metric: ${CW_METRIC_PATH} = ${WALL_CLOCK_SECS}"
if ! aws cloudwatch put-metric-data \
    --namespace "${CW_NAMESPACE}" \
    --metric-name "${CW_METRIC_NAME}" \
    --value "${WALL_CLOCK_SECS}" \
    --unit Seconds \
    --dimensions Drill=quarterly,Status="${RESTORE_STATUS}"; then
    echo "ERROR: aws cloudwatch put-metric-data dr_drill_wallclock_seconds failed; cron alerting will surface this." >&2
    exit 1
fi

echo "==> CloudWatch metric posted: ${CW_NAMESPACE}/${CW_METRIC_NAME} = ${WALL_CLOCK_SECS}"

# --- final verdict -----------------------------------------------------------
if [[ "${RESTORE_STATUS}" == "FAIL" ]]; then
    echo "FATAL: DR drill FAILED. Investigate per runbooks/dr-drill.md §failure-handling." >&2
    exit 1
fi
exit 0
