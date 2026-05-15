#!/usr/bin/env bash
# scripts/ci/phase1-gateway-overhead.sh — Phase 1 nightly cron driver.
# Runs the gateway commit-overhead baseline measurement and asserts the
# ADR-003 §3.3 contract (≤5% overhead) on every nightly invocation.
# Wraps `go run ./tests/load/cmd/gateway_overhead -assert-overhead-under=0.05`
# + posts wall-clock to CloudWatch (Phase 0.5 dr-drill.sh pattern per
# PATTERNS.md Group scripts ci line 218).
set -euo pipefail

# Documentation block follows. The first 10 lines keep the acceptance
# grep gates (#!/usr/bin/env bash + set -euo pipefail) at the top.
#
# Runs nightly via .github/workflows/phase1-overhead-nightly.yml (Phase
# 1 hardening; this script is the single source of truth so the workflow
# is a thin wrapper). The CloudWatch metric posted from this driver is
# Neksur/Phase1/GatewayOverheadDrillDurationSec — dashboards key on it.
#
# Required environment:
#   POLARIS_BASELINE_URL   — direct Polaris commit endpoint URL (the
#                            Polaris testcontainer or a pre-provisioned
#                            sandbox tenant's table).
#   GATEWAY_URL            — Neksur L1 gateway commit endpoint URL.
#   ICEBERG_REST_BEARER    — OAuth bearer token (applied to BOTH phases).
#
# Optional environment:
#   COMMIT_COUNT           — total commits per phase; default 1000.
#   WARMUP                 — warmup commits (discarded); default 100.
#   ASSERT_OVERHEAD_UNDER  — ADR-003 §3.3 contract; default 0.05.
#   AWS_REGION             — required when posting CloudWatch metric.
#
# Exit codes:
#   0 — overhead_ratio ≤ ASSERT_OVERHEAD_UNDER (default 0.05).
#   1 — assertion miss OR runtime error (cron alert fires on this).

# --- preflight ---------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

ASSERT_OVERHEAD_UNDER="${ASSERT_OVERHEAD_UNDER:-0.05}"
COMMIT_COUNT="${COMMIT_COUNT:-1000}"
WARMUP="${WARMUP:-100}"

if [[ -z "${POLARIS_BASELINE_URL:-}" ]]; then
    echo "ERROR: POLARIS_BASELINE_URL env required (direct Polaris commit endpoint)." >&2
    exit 64
fi
if [[ -z "${GATEWAY_URL:-}" ]]; then
    echo "ERROR: GATEWAY_URL env required (Neksur gateway commit endpoint)." >&2
    exit 64
fi
if [[ -z "${ICEBERG_REST_BEARER:-}" ]]; then
    echo "ERROR: ICEBERG_REST_BEARER env required (OAuth bearer for both phases)." >&2
    exit 64
fi

# --- bookkeeping -------------------------------------------------------------
START=$(date +%s)
START_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "==> Phase 1 gateway-overhead drill start: ${START_TS}"
echo "    baseline_url:           ${POLARIS_BASELINE_URL}"
echo "    gateway_url:            ${GATEWAY_URL}"
echo "    assert_overhead_under:  ${ASSERT_OVERHEAD_UNDER} (ADR-003 §3.3 contract)"
echo "    commit_count:           ${COMMIT_COUNT}"
echo "    warmup:                 ${WARMUP}"

# --- run measurement ---------------------------------------------------------
# Run the gateway-overhead baseline against the supplied endpoints.
# Wall-clock recorded; CloudWatch metric posted via aws cli (Phase 0.5
# dr-drill.sh pattern).
EXIT=0
go run ./tests/load/cmd/gateway_overhead \
    -assert-overhead-under="${ASSERT_OVERHEAD_UNDER}" \
    -commit-count="${COMMIT_COUNT}" \
    -warmup="${WARMUP}" \
    -baseline-url="${POLARIS_BASELINE_URL}" \
    -gateway-url="${GATEWAY_URL}" \
    -bearer="${ICEBERG_REST_BEARER}" \
    -baseline-out=tests/load/gateway-overhead-baseline.json \
    || EXIT=$?

END=$(date +%s)
DURATION=$((END - START))
END_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "==> Phase 1 gateway-overhead drill complete: ${END_TS}"
echo "    duration_seconds:     ${DURATION}"
echo "    exit_status:          ${EXIT}"

# --- post CloudWatch metric --------------------------------------------------
# Post wall-clock duration to CloudWatch (Phase 0.5 pattern — DR drill
# wraps the same way; see scripts/ci/dr-drill.sh). The metric is posted
# ALWAYS (on PASS or FAIL) so the dashboard reflects the latest observed
# wall-clock; a missing data point would mask a regressed drill.
if command -v aws &>/dev/null && [[ -n "${AWS_REGION:-}" ]]; then
    aws cloudwatch put-metric-data \
        --namespace Neksur/Phase1 \
        --metric-name GatewayOverheadDrillDurationSec \
        --value "${DURATION}" \
        --region "${AWS_REGION}" || true
fi

exit ${EXIT}
