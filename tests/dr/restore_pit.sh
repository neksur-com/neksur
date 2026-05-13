#!/usr/bin/env bash
# tests/dr/restore_pit.sh — pgBackRest point-in-time restore drill driver.
#
# Phase 0 Wave 3 (Plan 00-04). Implements the D-OQ.04 RTO-1h gate:
#   - Stop Postgres on the restore host.
#   - Wipe the data dir.
#   - pgbackrest --stanza=neksur --type=time --target=<ts> restore.
#   - Start Postgres; poll pg_isready until ready (or timeout).
#   - Compare elapsed wall-clock to --assert-rto-under threshold.
#
# CLI surface (from .planning/phases/00-metadata-graph-foundation/00-VALIDATION.md):
#   --assert-rto-under <seconds>     REQUIRED. Fail if restore took longer.
#   --target-time <ISO8601>          OPTIONAL. PITR target; default = "now".
#   --repo <path>                    OPTIONAL. pgBackRest repo path; default
#                                    /var/lib/pgbackrest.
#   --stanza <name>                  OPTIONAL. Stanza name; default "neksur"
#                                    (matches patroni.yml.template).
#   --pg-data <path>                 OPTIONAL. Postgres data dir to restore
#                                    INTO; default /var/lib/postgresql/16/main.
#   --pg-bin <path>                  OPTIONAL. Postgres bin dir for pg_ctl /
#                                    pg_isready; default /usr/lib/postgresql/16/bin.
#   --yes                            OPTIONAL. Bypass the "this will wipe
#                                    pg-data" confirmation prompt — REQUIRED
#                                    for unattended invocation (CI / wrapped
#                                    by Go test harness).
#
# Verifies: REQ-NFR-dr (RTO 1h per D-OQ.04), threat T-0-DR.
#
# Wrapped by: tests/dr/dr_targets_test.go::TestRTO_RestorePIT.
# Cross-ref:  runbooks/restore-pitr.md.

set -euo pipefail

# Defaults — overridable via flags.
ASSERT_RTO_UNDER=""
TARGET_TIME="now"
REPO_PATH="/var/lib/pgbackrest"
STANZA="neksur"
PG_DATA="${PGDATA:-/var/lib/postgresql/16/main}"
PG_BIN="/usr/lib/postgresql/16/bin"
ASSUME_YES="false"

usage() {
    cat <<EOF >&2
Usage: $0 --assert-rto-under <seconds> [--target-time <ISO8601>]
       [--repo <path>] [--stanza <name>] [--pg-data <path>]
       [--pg-bin <path>] [--yes]

Required:
  --assert-rto-under <seconds>   Fail if restore wall-clock exceeds.

Optional:
  --target-time <ISO8601>        Default: "now" (replays all available WAL).
  --repo <path>                  Default: /var/lib/pgbackrest.
  --stanza <name>                Default: neksur.
  --pg-data <path>               Default: /var/lib/postgresql/16/main.
  --pg-bin <path>                Default: /usr/lib/postgresql/16/bin.
  --yes                          Skip the wipe-confirmation prompt.

Verifies REQ-NFR-dr (D-OQ.04 RTO 1h).
EOF
    exit 64
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --assert-rto-under)
            ASSERT_RTO_UNDER="${2:-}"
            shift 2
            ;;
        --assert-rto-under=*)
            ASSERT_RTO_UNDER="${1#*=}"
            shift
            ;;
        --target-time)
            TARGET_TIME="${2:-}"
            shift 2
            ;;
        --target-time=*)
            TARGET_TIME="${1#*=}"
            shift
            ;;
        --repo)
            REPO_PATH="${2:-}"
            shift 2
            ;;
        --repo=*)
            REPO_PATH="${1#*=}"
            shift
            ;;
        --stanza)
            STANZA="${2:-}"
            shift 2
            ;;
        --stanza=*)
            STANZA="${1#*=}"
            shift
            ;;
        --pg-data)
            PG_DATA="${2:-}"
            shift 2
            ;;
        --pg-data=*)
            PG_DATA="${1#*=}"
            shift
            ;;
        --pg-bin)
            PG_BIN="${2:-}"
            shift 2
            ;;
        --pg-bin=*)
            PG_BIN="${1#*=}"
            shift
            ;;
        --yes)
            ASSUME_YES="true"
            shift
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            usage
            ;;
    esac
done

if [[ -z "${ASSERT_RTO_UNDER}" ]]; then
    echo "ERROR: --assert-rto-under <seconds> is required." >&2
    usage
fi

# Sanity check: ASSERT_RTO_UNDER must be a positive integer.
if ! [[ "${ASSERT_RTO_UNDER}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: --assert-rto-under must be a positive integer (got: ${ASSERT_RTO_UNDER})" >&2
    exit 64
fi

echo "==> pgBackRest PITR drill"
echo "    stanza:       ${STANZA}"
echo "    repo:         ${REPO_PATH}"
echo "    pg-data:      ${PG_DATA}"
echo "    target-time:  ${TARGET_TIME}"
echo "    RTO threshold: ${ASSERT_RTO_UNDER}s"

# Wipe-confirmation gate — production safety. The --yes flag bypasses
# this for CI / unattended invocation (the Go test harness sets it).
if [[ "${ASSUME_YES}" != "true" ]]; then
    cat <<EOF >&2
================================================================
WARNING: this drill will:
  1. Stop Postgres on this host (pg_ctl stop -m fast)
  2. WIPE the data directory at ${PG_DATA}
  3. Restore from pgBackRest repo at ${REPO_PATH}
================================================================
Type "WIPE" (uppercase) to proceed, anything else to abort:
EOF
    read -r CONFIRM
    if [[ "${CONFIRM}" != "WIPE" ]]; then
        echo "Aborted by user." >&2
        exit 1
    fi
fi

# Record T0 for the RTO measurement.
T0=$(date +%s)
echo "==> T0=${T0} ($(date -u -d "@${T0}" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -r "${T0}" '+%Y-%m-%dT%H:%M:%SZ'))"

# 1. Stop Postgres (fast mode — terminates connections, replays WAL on
#    next startup; cleaner than immediate which skips checkpoint).
echo "==> Stopping Postgres (pg_ctl stop -m fast)..."
if "${PG_BIN}/pg_ctl" -D "${PG_DATA}" status >/dev/null 2>&1; then
    "${PG_BIN}/pg_ctl" -D "${PG_DATA}" -m fast -w stop || {
        echo "WARN: pg_ctl stop failed (Postgres may already be down) — continuing." >&2
    }
else
    echo "    Postgres not running — nothing to stop."
fi

# 2. Wipe the data dir. Belt-and-braces: refuse to operate on `/`
#    or empty PG_DATA.
if [[ -z "${PG_DATA}" || "${PG_DATA}" == "/" ]]; then
    echo "ERROR: refusing to wipe PG_DATA=${PG_DATA}" >&2
    exit 1
fi
echo "==> Wiping ${PG_DATA}..."
rm -rf "${PG_DATA:?}"/*

# 3. pgBackRest restore.
echo "==> Restoring from pgBackRest --stanza=${STANZA} --type=time --target='${TARGET_TIME}'..."
RESTORE_ARGS=(--stanza="${STANZA}" --pg1-path="${PG_DATA}" --repo1-path="${REPO_PATH}")
if [[ "${TARGET_TIME}" == "now" ]]; then
    # No --target — default replays all available WAL.
    pgbackrest "${RESTORE_ARGS[@]}" --type=immediate restore
else
    pgbackrest "${RESTORE_ARGS[@]}" --type=time --target="${TARGET_TIME}" restore
fi

# 4. Start Postgres and poll pg_isready until ready (or RTO timeout).
echo "==> Starting Postgres (pg_ctl start -w)..."
"${PG_BIN}/pg_ctl" -D "${PG_DATA}" -w -t "${ASSERT_RTO_UNDER}" start

# pg_ctl -w already waits for accepting-connections; double-check with
# pg_isready (defensive — pg_ctl -w can succeed before recovery completes).
echo "==> Polling pg_isready (every 2s, up to ${ASSERT_RTO_UNDER}s total)..."
DEADLINE=$(( T0 + ASSERT_RTO_UNDER ))
while true; do
    if "${PG_BIN}/pg_isready" -d postgres -h /var/run/postgresql >/dev/null 2>&1; then
        break
    fi
    NOW=$(date +%s)
    if (( NOW > DEADLINE )); then
        echo "ERROR: pg_isready never became ready within ${ASSERT_RTO_UNDER}s" >&2
        exit 1
    fi
    sleep 2
done

# 5. Record T1 and compute elapsed.
T1=$(date +%s)
ELAPSED=$(( T1 - T0 ))
echo "==> T1=${T1} ELAPSED=${ELAPSED}s (threshold=${ASSERT_RTO_UNDER}s)"

if (( ELAPSED > ASSERT_RTO_UNDER )); then
    echo "ERROR: RTO BREACH — elapsed=${ELAPSED}s exceeds threshold=${ASSERT_RTO_UNDER}s" >&2
    echo "       D-OQ.04 contract: RTO 1h (${ASSERT_RTO_UNDER}s for the drill)" >&2
    exit 1
fi

echo "==> RTO PASS — restored to ${TARGET_TIME} in ${ELAPSED}s (under ${ASSERT_RTO_UNDER}s threshold)"
exit 0
