#!/usr/bin/env bash
# tests/dr/restore_pit.sh — pgBackRest point-in-time restore drill driver.
#
# STATUS: STUB. Real implementation lands in Plan 00-04 (Wave 3 — pgBackRest).
#
# Once implemented, this script will:
#   1. Take a fresh snapshot of the test Postgres+AGE cluster.
#   2. Note the LSN / wall-clock target.
#   3. Apply additional writes after the target.
#   4. Trigger pgBackRest `restore --target-time=...` to a fresh data dir.
#   5. Bring up the restored cluster and verify (a) restore time-to-ready
#      <RTO and (b) row counts match the target.
#
# CLI surface (from .planning/phases/00-metadata-graph-foundation/00-VALIDATION.md):
#   --assert-rto-under <seconds>   Fail if restore took longer than <seconds>.
#
# Verifies: REQ-NFR-dr (RTO 1h), threat T-0-DR.

set -euo pipefail

ASSERT_RTO_UNDER=""

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
    *)
      echo "Unknown argument: $1" >&2
      echo "Usage: $0 --assert-rto-under <seconds>" >&2
      exit 64
      ;;
  esac
done

if [[ -z "${ASSERT_RTO_UNDER}" ]]; then
  echo "ERROR: --assert-rto-under <seconds> is required." >&2
  exit 64
fi

echo "Not yet implemented — Plan 04 — Wave 3 pgBackRest"
echo "(would have asserted restore RTO < ${ASSERT_RTO_UNDER}s)"
exit 2
