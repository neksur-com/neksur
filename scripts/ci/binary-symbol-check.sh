#!/usr/bin/env bash
# scripts/ci/binary-symbol-check.sh
#
# Pitfall 7 (build-tag drift) mitigation: verifies that the L1 (BSL Core)
# binary does NOT contain symbols from the private L2/L3 coordination packages.
#
# If any of the five private-module coordination package paths appear in the
# binary's string table, the L1 binary has accidentally been compiled with
# L2/L3 code — a build-tag drift failure.
#
# Usage:
#   ./scripts/ci/binary-symbol-check.sh <binary-path>
#
# Exit codes:
#   0 — binary is clean (no private symbols found)
#   1 — one or more private symbols found (build-tag drift detected)
#   2 — usage error (wrong number of arguments or file not found)
#
# Plan 03-13 §Pitfall 7.

set -euo pipefail

BINARY="${1:-}"
if [[ -z "$BINARY" ]]; then
    echo "ERROR: binary path required" >&2
    echo "Usage: $0 <binary-path>" >&2
    exit 2
fi

if [[ ! -f "$BINARY" ]]; then
    echo "ERROR: binary not found: $BINARY" >&2
    exit 2
fi

# Private module symbol patterns to look for.
# Each pattern corresponds to a coordination package from neksur-commercial
# or neksur-enterprise. If any of these appear in the L1 binary's strings,
# the build-tag isolation has broken down.
PRIVATE_PATTERNS=(
    "coordination/schemacache"
    "coordination/verifier"
    "coordination/writeconflict"
    "coordination/partitionspec"
    "coordination/compaction"
)

FOUND=0

for PATTERN in "${PRIVATE_PATTERNS[@]}"; do
    if strings "$BINARY" | grep -qE "$PATTERN" 2>/dev/null; then
        echo "ERROR: private symbol found in L1 binary: $PATTERN" >&2
        FOUND=1
    fi
done

if [[ "$FOUND" -eq 1 ]]; then
    echo "" >&2
    echo "FAIL: L1 binary contains private coordination module symbols." >&2
    echo "      This is a build-tag drift failure (Pitfall 7)." >&2
    echo "      Verify that //go:build tags are correct in cmd/neksur-server/main_*.go." >&2
    echo "      Binary: $BINARY" >&2
    exit 1
fi

echo "OK: L1 binary is clean — no private coordination module symbols found."
echo "    Binary: $BINARY"
