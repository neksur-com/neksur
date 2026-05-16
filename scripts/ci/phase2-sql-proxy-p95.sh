#!/usr/bin/env bash
# scripts/ci/phase2-sql-proxy-p95.sh — Phase 2 nightly cron driver.
# Runs the SQL proxy P95/P99 latency baseline measurement and asserts
# the REQ-NFR-latency-sql-proxy contract (P95 ≤ 50ms / P99 ≤ 150ms) on
# every nightly invocation. Wraps `go run ./tests/load/cmd/sql_proxy_overhead`
# + posts wall-clock to CloudWatch (mirrors scripts/ci/phase1-gateway-overhead.sh).
set -euo pipefail

# Documentation block follows. The first 10 lines keep the acceptance
# grep gates (#!/usr/bin/env bash + set -euo pipefail) at the top.
#
# Runs nightly via .github/workflows/phase2-sql-proxy-nightly.yml
# (Phase 2 hardening; this script is the single source of truth so the
# workflow is a thin wrapper). The CloudWatch metric posted from this
# driver is Neksur/Phase2/SqlProxyP95DrillDurationSec — dashboards key
# on it.
#
# Required environment:
#   TRINO_DSN              — direct Trino DSN for the baseline phase
#                            (e.g. http://test@trino:8080?catalog=tpch&schema=tiny).
#   PROXY_URL              — Neksur SQL proxy endpoint URL (Plan 02-05
#                            will populate this; until then the runner
#                            writes status=PENDING_PROXY_LANDED).
#   ICEBERG_REST_BEARER    — OAuth bearer token (applied to both phases).
#
# Optional environment:
#   SAMPLE_COUNT           — queries per phase; default 1000.
#   WARMUP                 — warmup queries (discarded); default 100.
#   ASSERT_P95_UNDER       — REQ-NFR-latency-sql-proxy P95; default 50ms.
#   ASSERT_P99_UNDER       — REQ-NFR-latency-sql-proxy P99; default 150ms.
#   AWS_REGION             — required when posting CloudWatch metric.
#
# Exit codes:
#   0 — P95 + P99 gates pass OR proxy not-yet-shipped (PENDING_PROXY_LANDED).
#   1 — gate miss OR runtime error (cron alert fires on this).

# --- preflight ---------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

ASSERT_P95_UNDER="${ASSERT_P95_UNDER:-50ms}"
ASSERT_P99_UNDER="${ASSERT_P99_UNDER:-150ms}"
SAMPLE_COUNT="${SAMPLE_COUNT:-1000}"
WARMUP="${WARMUP:-100}"

if [[ -z "${TRINO_DSN:-}" ]]; then
    echo "ERROR: TRINO_DSN env required (direct Trino DSN for baseline phase)." >&2
    exit 64
fi
if [[ -z "${PROXY_URL:-}" ]]; then
    echo "ERROR: PROXY_URL env required (Neksur SQL proxy endpoint)." >&2
    exit 64
fi

# --- bookkeeping -------------------------------------------------------------
START=$(date +%s)
START_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "==> Phase 2 sql-proxy-p95 drill start: ${START_TS}"
echo "    trino_dsn:              ${TRINO_DSN}"
echo "    proxy_url:              ${PROXY_URL}"
echo "    assert_p95_under:       ${ASSERT_P95_UNDER} (REQ-NFR-latency-sql-proxy)"
echo "    assert_p99_under:       ${ASSERT_P99_UNDER} (REQ-NFR-latency-sql-proxy)"
echo "    sample_count:           ${SAMPLE_COUNT}"
echo "    warmup:                 ${WARMUP}"

# --- run measurement ---------------------------------------------------------
# Run the SQL proxy P95/P99 baseline against the supplied endpoints.
# Wall-clock recorded; CloudWatch metric posted via aws cli (Phase 0.5
# dr-drill.sh pattern, carried forward from Phase 1 gateway-overhead.sh).
EXIT=0
go run ./tests/load/cmd/sql_proxy_overhead \
    -assert-p95-under="${ASSERT_P95_UNDER}" \
    -assert-p99-under="${ASSERT_P99_UNDER}" \
    -sample-count="${SAMPLE_COUNT}" \
    -warmup="${WARMUP}" \
    -trino-dsn="${TRINO_DSN}" \
    -proxy-url="${PROXY_URL}" \
    -bearer="${ICEBERG_REST_BEARER:-}" \
    -baseline-out=tests/load/sql-proxy-baseline.json \
    || EXIT=$?

END=$(date +%s)
DURATION=$((END - START))
END_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "==> Phase 2 sql-proxy-p95 drill complete: ${END_TS}"
echo "    duration_seconds:     ${DURATION}"
echo "    exit_status:          ${EXIT}"

# --- post CloudWatch metric --------------------------------------------------
# Post wall-clock duration to CloudWatch (Phase 0.5 pattern — DR drill
# wraps the same way; see scripts/ci/dr-drill.sh + Phase 1 sibling).
# Posted ALWAYS (on PASS / FAIL / PENDING_PROXY_LANDED) so the dashboard
# reflects the latest observed wall-clock; a missing data point would
# mask a regressed drill.
if command -v aws &>/dev/null && [[ -n "${AWS_REGION:-}" ]]; then
    aws cloudwatch put-metric-data \
        --namespace Neksur/Phase2 \
        --metric-name SqlProxyP95DrillDurationSec \
        --value "${DURATION}" \
        --region "${AWS_REGION}" || true
fi

exit ${EXIT}
