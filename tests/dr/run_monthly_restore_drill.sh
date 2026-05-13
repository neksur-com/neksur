#!/usr/bin/env bash
# tests/dr/run_monthly_restore_drill.sh — monthly DR restore drill driver.
#
# Phase 0 Wave 3 (Plan 00-04). Implements the Manual-Only Verification
# row in 00-VALIDATION.md: an operator-runnable end-to-end drill that
# (a) restores from cold storage, (b) validates schema integrity per
# the D-003.06 amendment (19 vlabels + 24 elabels), (c) emits a
# timestamped report under runbooks/drill-reports/.
#
# Why a separate script (vs. just running restore_pit.sh in cron):
#   - The drill must SUCCEED EVEN IF restore_pit.sh fails — the report
#     captures the failure mode for postmortem. Cron-invoked
#     restore_pit.sh failure would surface only as a non-zero exit.
#   - The drill must validate SCHEMA INTEGRITY post-restore (vlabel /
#     elabel counts), which is outside restore_pit.sh's contract.
#   - The drill emits a structured Markdown report (timestamp,
#     observed RTO, schema-integrity PASS/FAIL, deviations) that the
#     on-call operator references during the monthly review.
#
# CLI surface:
#   --target-time <ISO8601>     OPTIONAL. PITR target; default = "24h ago".
#   --rto-threshold <seconds>   OPTIONAL. Default 3600 (D-OQ.04 RTO 1h).
#   --report-dir <path>         OPTIONAL. Default runbooks/drill-reports.
#   --check-sql <path>          OPTIONAL. Schema-integrity SQL; default
#                               migrations/check.sql.
#   --psql <cmd>                OPTIONAL. psql invocation prefix; default
#                               "psql postgres://postgres@localhost:5432/postgres".
#   --pg-data <path>            OPTIONAL. Forwarded to restore_pit.sh.
#   --pg-bin <path>             OPTIONAL. Forwarded to restore_pit.sh.
#   --stanza <name>             OPTIONAL. Forwarded to restore_pit.sh; default neksur.
#   --repo <path>               OPTIONAL. Forwarded to restore_pit.sh.
#   --yes                       OPTIONAL. Forwarded to restore_pit.sh
#                               (REQUIRED for unattended invocation).
#
# Verifies: REQ-NFR-dr (restore-tested-monthly contract).
#
# Wrapped by: tests/dr/dr_targets_test.go::TestMonthlyRestoreDrill.
# Cross-ref:  runbooks/dr-drill.md.

set -euo pipefail

TARGET_TIME=""    # default = "24 hours ago" (computed below)
RTO_THRESHOLD=3600
REPORT_DIR="runbooks/drill-reports"
CHECK_SQL="migrations/check.sql"
PSQL_CMD="psql postgres://postgres@localhost:5432/postgres"
PG_DATA=""
PG_BIN=""
STANZA="neksur"
REPO_PATH=""
ASSUME_YES="false"

usage() {
    cat <<EOF >&2
Usage: $0 [--target-time <ISO8601>] [--rto-threshold <s>]
       [--report-dir <path>] [--check-sql <path>] [--psql <cmd>]
       [--pg-data <path>] [--pg-bin <path>]
       [--stanza <name>] [--repo <path>] [--yes]

Monthly DR drill: restore + schema-integrity check + drill report.

Verifies REQ-NFR-dr (D-OQ.04 RTO 1h + restore-tested-monthly).
EOF
    exit 64
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --target-time) TARGET_TIME="${2:-}"; shift 2 ;;
        --target-time=*) TARGET_TIME="${1#*=}"; shift ;;
        --rto-threshold) RTO_THRESHOLD="${2:-}"; shift 2 ;;
        --rto-threshold=*) RTO_THRESHOLD="${1#*=}"; shift ;;
        --report-dir) REPORT_DIR="${2:-}"; shift 2 ;;
        --report-dir=*) REPORT_DIR="${1#*=}"; shift ;;
        --check-sql) CHECK_SQL="${2:-}"; shift 2 ;;
        --check-sql=*) CHECK_SQL="${1#*=}"; shift ;;
        --psql) PSQL_CMD="${2:-}"; shift 2 ;;
        --psql=*) PSQL_CMD="${1#*=}"; shift ;;
        --pg-data) PG_DATA="${2:-}"; shift 2 ;;
        --pg-data=*) PG_DATA="${1#*=}"; shift ;;
        --pg-bin) PG_BIN="${2:-}"; shift 2 ;;
        --pg-bin=*) PG_BIN="${1#*=}"; shift ;;
        --stanza) STANZA="${2:-}"; shift 2 ;;
        --stanza=*) STANZA="${1#*=}"; shift ;;
        --repo) REPO_PATH="${2:-}"; shift 2 ;;
        --repo=*) REPO_PATH="${1#*=}"; shift ;;
        --yes) ASSUME_YES="true"; shift ;;
        -h|--help) usage ;;
        *) echo "ERROR: unknown argument: $1" >&2; usage ;;
    esac
done

# Default target-time = 24 hours ago in ISO8601 UTC.
if [[ -z "${TARGET_TIME}" ]]; then
    if date -u -d "24 hours ago" '+%Y-%m-%d %H:%M:%S+00' >/dev/null 2>&1; then
        TARGET_TIME=$(date -u -d "24 hours ago" '+%Y-%m-%d %H:%M:%S+00')
    else
        # macOS / BSD date — uses -v.
        TARGET_TIME=$(date -u -v-24H '+%Y-%m-%d %H:%M:%S+00')
    fi
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# YYYYMM tag for the report filename.
YEARMONTH=$(date -u '+%Y%m')
START_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
START_EPOCH=$(date +%s)

mkdir -p "${REPO_ROOT}/${REPORT_DIR}"
REPORT_PATH="${REPO_ROOT}/${REPORT_DIR}/drill-report-${YEARMONTH}.md"

echo "==> Monthly DR drill"
echo "    started:        ${START_TS}"
echo "    target-time:    ${TARGET_TIME}"
echo "    rto-threshold:  ${RTO_THRESHOLD}s"
echo "    report:         ${REPORT_PATH}"

# --- Phase 1: restore from backup -------------------------------------
RESTORE_STATUS="SKIPPED"
RESTORE_ELAPSED=0
RESTORE_LOG="$(mktemp -t monthly-drill.XXXXXX.log)"

RESTORE_ARGS=(--assert-rto-under "${RTO_THRESHOLD}" --target-time "${TARGET_TIME}" --stanza "${STANZA}")
[[ -n "${PG_DATA}" ]] && RESTORE_ARGS+=(--pg-data "${PG_DATA}")
[[ -n "${PG_BIN}" ]] && RESTORE_ARGS+=(--pg-bin "${PG_BIN}")
[[ -n "${REPO_PATH}" ]] && RESTORE_ARGS+=(--repo "${REPO_PATH}")
[[ "${ASSUME_YES}" == "true" ]] && RESTORE_ARGS+=(--yes)

echo "==> Invoking restore_pit.sh ${RESTORE_ARGS[*]}..."
RESTORE_T0=$(date +%s)
if bash "${SCRIPT_DIR}/restore_pit.sh" "${RESTORE_ARGS[@]}" 2>&1 | tee "${RESTORE_LOG}"; then
    RESTORE_STATUS="PASS"
else
    RESTORE_STATUS="FAIL"
fi
RESTORE_T1=$(date +%s)
RESTORE_ELAPSED=$(( RESTORE_T1 - RESTORE_T0 ))

# --- Phase 2: schema integrity check ----------------------------------
INTEGRITY_STATUS="SKIPPED"
INTEGRITY_LOG="$(mktemp -t monthly-drill-check.XXXXXX.log)"
VLABEL_COUNT="?"
ELABEL_COUNT="?"

if [[ "${RESTORE_STATUS}" == "PASS" ]]; then
    if [[ -f "${REPO_ROOT}/${CHECK_SQL}" ]]; then
        echo "==> Running migrations/check.sql..."
        if ${PSQL_CMD} -v ON_ERROR_STOP=1 -f "${REPO_ROOT}/${CHECK_SQL}" > "${INTEGRITY_LOG}" 2>&1; then
            # check.sql emits "PASS" / "FAIL" rows. Any FAIL fails the gate.
            if grep -q "FAIL" "${INTEGRITY_LOG}"; then
                INTEGRITY_STATUS="FAIL"
            else
                INTEGRITY_STATUS="PASS"
            fi
            # Extract the actual vlabel/elabel counts from check.sql output
            # for the report. The output is the standard psql table format;
            # we grep for the check_name and extract column 3 (the actual).
            VLABEL_COUNT=$(grep "vlabel_count" "${INTEGRITY_LOG}" | awk '{print $5}' | head -1)
            ELABEL_COUNT=$(grep "elabel_count" "${INTEGRITY_LOG}" | awk '{print $5}' | head -1)
            VLABEL_COUNT=${VLABEL_COUNT:-?}
            ELABEL_COUNT=${ELABEL_COUNT:-?}
        else
            INTEGRITY_STATUS="FAIL"
            echo "WARN: psql -f ${CHECK_SQL} returned non-zero." >&2
        fi
    else
        echo "WARN: ${CHECK_SQL} not found at ${REPO_ROOT}/${CHECK_SQL} — skipping schema check." >&2
        INTEGRITY_STATUS="SKIPPED"
    fi
fi

# --- Phase 3: write the drill report ----------------------------------
END_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
END_EPOCH=$(date +%s)
TOTAL_ELAPSED=$(( END_EPOCH - START_EPOCH ))

# Determine overall drill outcome.
if [[ "${RESTORE_STATUS}" == "PASS" && "${INTEGRITY_STATUS}" == "PASS" ]]; then
    OVERALL="PASS"
elif [[ "${RESTORE_STATUS}" == "PASS" && "${INTEGRITY_STATUS}" == "SKIPPED" ]]; then
    OVERALL="PASS-WITH-CAVEAT"
else
    OVERALL="FAIL"
fi

cat > "${REPORT_PATH}" <<EOF
# DR Drill Report — ${YEARMONTH}

**Drill date (UTC):** ${START_TS}
**Drill end (UTC):**   ${END_TS}
**Total wall-clock:**  ${TOTAL_ELAPSED}s
**Outcome:**           ${OVERALL}

## Contract

D-OQ.04 (LOCKED): RTO 1h / RPO 15min unified across all metadata stores.
This drill validates the **RTO + restore-tested-monthly** half of the
contract; RPO is validated separately by tests/dr/chaos_restore.sh
(\`go test -tags dr -run TestRPO_ChaosRestore\`).

## Restore

| Field            | Value                           |
| ---------------- | ------------------------------- |
| target-time      | \`${TARGET_TIME}\`              |
| stanza           | \`${STANZA}\`                   |
| RTO threshold    | ${RTO_THRESHOLD}s               |
| Observed RTO     | ${RESTORE_ELAPSED}s             |
| Status           | **${RESTORE_STATUS}**           |
| Restore log      | (captured to ${RESTORE_LOG} on the drill host) |

## Schema integrity (D-003.06 amendment: 19 vlabels + 24 elabels)

| Field            | Value             |
| ---------------- | ----------------- |
| check.sql        | \`${CHECK_SQL}\`  |
| Observed vlabels | ${VLABEL_COUNT} (expect 19) |
| Observed elabels | ${ELABEL_COUNT} (expect 24) |
| Status           | **${INTEGRITY_STATUS}** |
| Check log        | (captured to ${INTEGRITY_LOG} on the drill host) |

## Deviations from runbook

(Operator: fill this section if anything diverged from
\`runbooks/dr-drill.md\`. Examples: pgBackRest version drift, repo
storage outage during restore, schema FAIL on a label that was
intentionally dropped this month.)

## Next actions

EOF

if [[ "${OVERALL}" == "PASS" ]]; then
    cat >> "${REPORT_PATH}" <<EOF
- [ ] None — drill passed cleanly. File this report under
      \`${REPORT_DIR}/\` and close the drill ticket.
EOF
elif [[ "${OVERALL}" == "PASS-WITH-CAVEAT" ]]; then
    cat >> "${REPORT_PATH}" <<EOF
- [ ] Investigate why \`${CHECK_SQL}\` was unavailable / skipped on the
      drill host; ensure next drill runs the schema check.
EOF
else
    cat >> "${REPORT_PATH}" <<EOF
- [ ] **File an incident** per \`runbooks/dr-drill.md\` §failure-handling.
- [ ] Add a regression test to \`tests/dr/\` covering the failure mode.
- [ ] Re-run the drill after fix to confirm restoration of the contract.
EOF
fi

cat >> "${REPORT_PATH}" <<EOF

---

Generated by \`tests/dr/run_monthly_restore_drill.sh\` —
wrapped by Go test \`TestMonthlyRestoreDrill\` in
\`tests/dr/dr_targets_test.go\` (\`//go:build dr\`).
Schema contract: 19 vlabels + 24 elabels per D-003.06.
EOF

echo "==> Drill report written: ${REPORT_PATH}"
echo "==> Outcome: ${OVERALL} (restore=${RESTORE_STATUS}, integrity=${INTEGRITY_STATUS}, elapsed=${TOTAL_ELAPSED}s)"

if [[ "${OVERALL}" == "FAIL" ]]; then
    exit 1
fi
exit 0
