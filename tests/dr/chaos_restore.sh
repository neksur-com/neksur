#!/usr/bin/env bash
# tests/dr/chaos_restore.sh — kill-primary-mid-load + restore + RPO assertion.
#
# Phase 0 Wave 3 (Plan 00-04). Implements the D-OQ.04 RPO-15min gate:
#   1. Start a sustained Cypher write workload (dr-writer Go binary).
#   2. After 10 minutes of warm-up, SIGKILL the Patroni primary.
#      (Direct `docker compose kill --signal SIGKILL` against the leader
#       is the simpler path; tests/chaos/patroni_chaos.go::KillPrimary
#       is a Go wrapper around the same docker call.)
#   3. Snapshot the kill timestamp (KILL_TS).
#   4. Run pgBackRest PITR to "immediate" (latest available WAL).
#   5. Read the dr-writer's CSV (one row per successful write with
#      its timestamp) and find the most recent timestamp that is
#      ALSO present in the restored graph.
#   6. DATA_LOSS = KILL_TS - LAST_RECOVERED_TS.
#   7. Compare DATA_LOSS to --assert-rpo-under threshold.
#
# CLI surface (from .planning/phases/00-metadata-graph-foundation/00-VALIDATION.md):
#   --assert-rpo-under <seconds>     REQUIRED. Fail if RPO exceeded.
#   --warmup-minutes <minutes>       OPTIONAL. Writer warmup before kill;
#                                    default 10 (matches plan acceptance).
#   --writer-qps <int>               OPTIONAL. Writes/sec; default 1000
#                                    (Phase 0 envelope CI proxy).
#   --leader <name>                  OPTIONAL. Patroni leader container
#                                    name to kill; default auto-detected
#                                    via Patroni REST /cluster.
#   --compose-file <path>            OPTIONAL. Default infra/docker-compose.ha.yml.
#   --writer-csv <path>              OPTIONAL. Where dr-writer logs successful
#                                    inserts; default tests/dr/_writer_timestamps.csv.
#   --psql <cmd>                     OPTIONAL. psql invocation prefix used for
#                                    LAST_RECOVERED_TS lookup; default
#                                    "psql postgres://postgres@localhost:5432/neksur".
#   --skip-restore                   OPTIONAL. For dev only — kill but don't
#                                    restore (lets ops inspect cluster state).
#   --yes                            REQUIRED for unattended runs (silently
#                                    bypasses any pgBackRest wipe prompts via
#                                    restore_pit.sh).
#
# Verifies: REQ-NFR-dr (RPO 15min per D-OQ.04, =900s), threat T-0-DR.
#
# Wrapped by: tests/dr/dr_targets_test.go::TestRPO_ChaosRestore.
# Cross-ref:  tests/dr/cmd/dr-writer/main.go (the load source),
#             tests/chaos/patroni_chaos.go::KillPrimary (Go alternative).

set -euo pipefail

ASSERT_RPO_UNDER=""
WARMUP_MINUTES=10
WRITER_QPS=1000
LEADER=""
COMPOSE_FILE="infra/docker-compose.ha.yml"
WRITER_CSV="tests/dr/_writer_timestamps.csv"
PSQL_CMD="psql postgres://postgres@localhost:5432/neksur"
SKIP_RESTORE="false"
ASSUME_YES="false"

usage() {
    cat <<EOF >&2
Usage: $0 --assert-rpo-under <seconds> [--warmup-minutes <m>]
       [--writer-qps <int>] [--leader <name>]
       [--compose-file <path>] [--writer-csv <path>]
       [--psql <cmd>] [--skip-restore] [--yes]

Required:
  --assert-rpo-under <seconds>   D-OQ.04 RPO threshold (typically 900 = 15min).

Optional:
  --warmup-minutes <m>           Default: 10 (plan acceptance).
  --writer-qps <int>             Default: 1000 (Phase 0 envelope proxy).
  --leader <name>                Default: auto-detected via Patroni /cluster.
  --compose-file <path>          Default: infra/docker-compose.ha.yml.
  --writer-csv <path>            Default: tests/dr/_writer_timestamps.csv.
  --psql <cmd>                   Default: psql postgres://postgres@localhost:5432/neksur.
  --skip-restore                 Dev only — kill but don't restore.
  --yes                          Unattended mode; bypasses restore wipe prompt.

Verifies REQ-NFR-dr (D-OQ.04 RPO 15min).
EOF
    exit 64
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --assert-rpo-under) ASSERT_RPO_UNDER="${2:-}"; shift 2 ;;
        --assert-rpo-under=*) ASSERT_RPO_UNDER="${1#*=}"; shift ;;
        --warmup-minutes) WARMUP_MINUTES="${2:-}"; shift 2 ;;
        --warmup-minutes=*) WARMUP_MINUTES="${1#*=}"; shift ;;
        --writer-qps) WRITER_QPS="${2:-}"; shift 2 ;;
        --writer-qps=*) WRITER_QPS="${1#*=}"; shift ;;
        --leader) LEADER="${2:-}"; shift 2 ;;
        --leader=*) LEADER="${1#*=}"; shift ;;
        --compose-file) COMPOSE_FILE="${2:-}"; shift 2 ;;
        --compose-file=*) COMPOSE_FILE="${1#*=}"; shift ;;
        --writer-csv) WRITER_CSV="${2:-}"; shift 2 ;;
        --writer-csv=*) WRITER_CSV="${1#*=}"; shift ;;
        --psql) PSQL_CMD="${2:-}"; shift 2 ;;
        --psql=*) PSQL_CMD="${1#*=}"; shift ;;
        --skip-restore) SKIP_RESTORE="true"; shift ;;
        --yes) ASSUME_YES="true"; shift ;;
        -h|--help) usage ;;
        *) echo "ERROR: unknown argument: $1" >&2; usage ;;
    esac
done

if [[ -z "${ASSERT_RPO_UNDER}" ]]; then
    echo "ERROR: --assert-rpo-under <seconds> is required." >&2
    usage
fi

if ! [[ "${ASSERT_RPO_UNDER}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: --assert-rpo-under must be a positive integer (got: ${ASSERT_RPO_UNDER})" >&2
    exit 64
fi

# Resolve script-relative paths so the script works whether invoked
# from repo root or from tests/dr/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

echo "==> Chaos+restore drill"
echo "    RPO threshold:   ${ASSERT_RPO_UNDER}s"
echo "    warmup minutes:  ${WARMUP_MINUTES}"
echo "    writer qps:      ${WRITER_QPS}"
echo "    compose file:    ${COMPOSE_FILE}"
echo "    writer CSV:      ${WRITER_CSV}"

# 1. Auto-detect leader if not provided. Patroni REST /cluster on
#    pg-node-1 (host port 8011 per docker-compose.ha.yml).
if [[ -z "${LEADER}" ]]; then
    echo "==> Auto-detecting Patroni leader via http://localhost:8011/cluster..."
    LEADER=$(curl -fsS http://localhost:8011/cluster | \
        python3 -c 'import json,sys; m=json.load(sys.stdin)["members"]; print(next(x["name"] for x in m if x["role"] in ("master","leader")))' 2>/dev/null) || {
        echo "ERROR: failed to auto-detect leader. Provide --leader <name> explicitly." >&2
        exit 1
    }
fi
echo "    leader (kill target): ${LEADER}"

# 2. Start the dr-writer Go binary in the background. The CSV is the
#    cross-process record of what was emitted — chaos_restore reads it
#    AFTER restore to find LAST_RECOVERED_TS.
WRITER_LOG="$(mktemp -t dr-writer.XXXXXX.log)"
echo "==> Starting dr-writer (qps=${WRITER_QPS}, duration=$((WARMUP_MINUTES * 2))m, csv=${WRITER_CSV}, log=${WRITER_LOG})..."
mkdir -p "$(dirname "${WRITER_CSV}")"
: > "${WRITER_CSV}"

# Run from repo root so `go run` resolves the module path.
(
    cd "${REPO_ROOT}"
    go run ./tests/dr/cmd/dr-writer \
        -duration="$((WARMUP_MINUTES * 2))m" \
        -qps="${WRITER_QPS}" \
        -outfile="${WRITER_CSV}" \
        > "${WRITER_LOG}" 2>&1 &
    echo "$!" > "/tmp/dr-writer.pid.$$"
)
WRITER_PID=$(cat "/tmp/dr-writer.pid.$$")
echo "    dr-writer PID: ${WRITER_PID}"

# Cleanup hook — kill the writer if we exit prematurely.
cleanup() {
    if kill -0 "${WRITER_PID}" 2>/dev/null; then
        echo "==> Cleanup: stopping dr-writer (PID ${WRITER_PID})..." >&2
        kill -INT "${WRITER_PID}" 2>/dev/null || true
        wait "${WRITER_PID}" 2>/dev/null || true
    fi
    rm -f "/tmp/dr-writer.pid.$$"
}
trap cleanup EXIT

# 3. Wait WARMUP_MINUTES, then SIGKILL the leader. Direct docker
#    compose kill — equivalent to tests/chaos/patroni_chaos.go::KillPrimary
#    (which simply runs `docker compose kill --signal SIGKILL <leader>`).
echo "==> Warming up for ${WARMUP_MINUTES} minutes ($((WARMUP_MINUTES * 60))s)..."
sleep "$((WARMUP_MINUTES * 60))"

KILL_TS=$(date +%s)
echo "==> KILL_TS=${KILL_TS} ($(date -u -d "@${KILL_TS}" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -r "${KILL_TS}" '+%Y-%m-%dT%H:%M:%SZ'))"
echo "==> SIGKILL leader '${LEADER}' via docker compose -f ${COMPOSE_FILE} kill..."
docker compose -f "${COMPOSE_FILE}" kill --signal SIGKILL "${LEADER}"

# Stop the writer cleanly so the CSV is fully flushed.
echo "==> Stopping dr-writer (SIGINT)..."
kill -INT "${WRITER_PID}" 2>/dev/null || true
wait "${WRITER_PID}" 2>/dev/null || true
trap - EXIT

if [[ "${SKIP_RESTORE}" == "true" ]]; then
    echo "==> --skip-restore set; exiting before restore."
    exit 0
fi

# 4. Wait for pgBackRest archive-async to flush queued WAL. The async
#    queue contains in-flight segments at the moment of kill; giving
#    it 30s to drain matches Pitfall 6's headroom assumption. (In
#    chaos test setups where pgBackRest is colocated on the killed
#    node, the spool would be lost; the test setup uses a separate
#    backup host per T-0-DR mitigation.)
echo "==> Waiting 30s for archive-async to drain residual WAL..."
sleep 30

# 5. Run PITR restore to immediate (latest available WAL).
echo "==> Running PITR restore to latest WAL via restore_pit.sh..."
RESTORE_ARGS=(--assert-rto-under 3600 --target-time now)
if [[ "${ASSUME_YES}" == "true" ]]; then
    RESTORE_ARGS+=(--yes)
fi
bash "${SCRIPT_DIR}/restore_pit.sh" "${RESTORE_ARGS[@]}"

# 6. Find LAST_RECOVERED_TS — the most recent dr-writer timestamp
#    whose corresponding node is present in the restored graph.
#    The CSV is `<unix_ts>,<node_id>` per line; we walk it from the
#    end backwards and check each node_id via psql.
if [[ ! -s "${WRITER_CSV}" ]]; then
    echo "ERROR: writer CSV ${WRITER_CSV} is empty — dr-writer never recorded a successful insert." >&2
    cat "${WRITER_LOG}" >&2 || true
    exit 1
fi

echo "==> Determining LAST_RECOVERED_TS by walking ${WRITER_CSV} backwards..."
LAST_RECOVERED_TS=""
# tac is GNU; on macOS use `tail -r`. Try both.
if command -v tac >/dev/null 2>&1; then
    REVERSE_CMD="tac"
else
    REVERSE_CMD="tail -r"
fi
while IFS=',' read -r ts node_id; do
    # Skip blank lines.
    [[ -z "${ts}" || -z "${node_id}" ]] && continue
    # Query the restored graph for this node_id.
    FOUND=$(${PSQL_CMD} -At -c \
        "SELECT 1 FROM cypher('neksur', \$\$ MATCH (n:wal_probe {writer_id: '${node_id}'}) RETURN n LIMIT 1 \$\$) AS (n agtype) LIMIT 1;" \
        2>/dev/null || echo "")
    if [[ "${FOUND}" == "1" ]]; then
        LAST_RECOVERED_TS="${ts}"
        echo "    found node ${node_id} at ts=${ts} ($(date -u -d "@${ts}" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -r "${ts}" '+%Y-%m-%dT%H:%M:%SZ'))"
        break
    fi
done < <(${REVERSE_CMD} "${WRITER_CSV}")

if [[ -z "${LAST_RECOVERED_TS}" ]]; then
    echo "ERROR: no writer-emitted node was found in the restored graph." >&2
    echo "       This is either a complete restore failure (catastrophic data loss)" >&2
    echo "       or a wal_probe label / writer_id mismatch — investigate." >&2
    exit 1
fi

# 7. Compute DATA_LOSS = KILL_TS - LAST_RECOVERED_TS and compare.
DATA_LOSS=$(( KILL_TS - LAST_RECOVERED_TS ))
echo "==> KILL_TS=${KILL_TS} LAST_RECOVERED_TS=${LAST_RECOVERED_TS} DATA_LOSS=${DATA_LOSS}s (threshold=${ASSERT_RPO_UNDER}s)"

if (( DATA_LOSS < 0 )); then
    echo "WARN: DATA_LOSS is negative — last recovered write is after the kill timestamp." >&2
    echo "      This is a clock-skew artifact or the writer's wall-clock differed from the host." >&2
    DATA_LOSS=0
fi

if (( DATA_LOSS > ASSERT_RPO_UNDER )); then
    echo "ERROR: RPO BREACH — data_loss=${DATA_LOSS}s exceeds threshold=${ASSERT_RPO_UNDER}s" >&2
    echo "       D-OQ.04 contract: RPO 15min (${ASSERT_RPO_UNDER}s for the drill)" >&2
    exit 1
fi

echo "==> RPO PASS — observed data loss ${DATA_LOSS}s under ${ASSERT_RPO_UNDER}s threshold"
exit 0
