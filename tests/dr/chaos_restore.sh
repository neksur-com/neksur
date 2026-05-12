#!/usr/bin/env bash
# tests/dr/chaos_restore.sh — kill-primary-mid-load + restore + RPO assertion.
#
# STATUS: STUB. Real implementation lands in Plan 00-04 (Wave 3 — pgBackRest).
#
# Once implemented, this script will:
#   1. Start the Phase 0 envelope seed in the background
#      (tests/load/fixtures/phase0_envelope_seed.py::seed).
#   2. Snapshot the LSN at the moment of kill.
#   3. SIGKILL the Patroni primary
#      (tests/chaos/lib/patroni_chaos.py::kill_primary).
#   4. Wait for pgBackRest to flush archive; trigger restore.
#   5. Bring up the restored cluster; measure LSN delta vs the pre-kill LSN.
#   6. Assert wall-clock RPO (data lost / write rate) is under the threshold.
#
# CLI surface (from .planning/phases/00-metadata-graph-foundation/00-VALIDATION.md):
#   --assert-rpo-under <seconds>   Fail if RPO exceeded <seconds>.
#
# Verifies: REQ-NFR-dr (RPO 15min = 900s per D-OQ.04 tightening), threat T-0-DR.

set -euo pipefail

ASSERT_RPO_UNDER=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --assert-rpo-under)
      ASSERT_RPO_UNDER="${2:-}"
      shift 2
      ;;
    --assert-rpo-under=*)
      ASSERT_RPO_UNDER="${1#*=}"
      shift
      ;;
    *)
      echo "Unknown argument: $1" >&2
      echo "Usage: $0 --assert-rpo-under <seconds>" >&2
      exit 64
      ;;
  esac
done

if [[ -z "${ASSERT_RPO_UNDER}" ]]; then
  echo "ERROR: --assert-rpo-under <seconds> is required." >&2
  exit 64
fi

echo "Not yet implemented — Plan 04 — Wave 3 pgBackRest"
echo "(would have asserted RPO < ${ASSERT_RPO_UNDER}s after primary kill)"
exit 2
