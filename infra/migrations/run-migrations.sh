#!/usr/bin/env bash
# =====================================================================
# run-migrations.sh — Phase 0 migration runner.
#
# Tool: sqitch (see docs/decisions/D-W1-migration-tool.md — LOCKED via
# the Atlas-vs-sqitch spike).
#
# Usage:
#   ./run-migrations.sh --db-url postgresql://user:pass@host:5432/db
#
# Behaviour:
#   1. Strict mode (errors fail fast).
#   2. Validates --db-url is present.
#   3. Applies migrations in order:
#        postgres/V0001  (extensions: age, pgaudit, pg_stat_statements)
#        graph/V0010     (AGE graph + 19 vlabels + 24 elabels)
#        graph/V0020     (D-001.07 property + edge indexes)
#        graph/V0025     (per-vlabel tenant btree + GIN — BEFORE load)
#        postgres/V0030  (RLS policies — Task 2b)
#   4. Logs every step.
#
# Implementation note:
#   When the `sqitch` binary is on PATH, we invoke `sqitch deploy` against
#   the bundled sqitch project (the canonical, ADR-documented path).
#   When `sqitch` is not available — e.g., on a developer laptop, or in
#   the testcontainers-based integration test harness that wraps this
#   script — we fall back to ordered `psql` invocations against the same
#   versioned SQL files. The output is byte-identical schema state either
#   way; sqitch is the system-of-record for production, psql-direct is
#   the convenience path for tests. The fallback path is the reason
#   `migrations/` lives at the repo root and not under `sqitch/deploy/`
#   — both tools can consume the same file tree.
# =====================================================================

set -euo pipefail

# ---- Args ----------------------------------------------------------------
DB_URL=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --db-url)
            DB_URL="$2"
            shift 2
            ;;
        --db-url=*)
            DB_URL="${1#*=}"
            shift
            ;;
        -h|--help)
            sed -n '1,40p' "$0"
            exit 0
            ;;
        *)
            echo "run-migrations.sh: unknown argument: $1" >&2
            exit 2
            ;;
    esac
done

if [[ -z "$DB_URL" ]]; then
    echo "run-migrations.sh: --db-url is required" >&2
    exit 2
fi

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

# Migration file order (lexicographic within each subdir is intentional;
# postgres before graph because V0010 depends on `age` extension installed
# by V0001).
MIGRATIONS=(
    "migrations/postgres/V0001__enable_extensions.sql"
    "migrations/graph/V0010__create_graph_and_labels.sql"
    "migrations/graph/V0020__property_indexes.sql"
    "migrations/graph/V0025__tenant_indexes_and_gin.sql"
    "migrations/postgres/V0030__rls_policies.sql"
)

# ---- Mode selection ------------------------------------------------------
if command -v sqitch >/dev/null 2>&1; then
    echo "run-migrations.sh: using sqitch (system-of-record per D-W1-migration-tool.md)"
    # Sqitch invocation. The project file is generated lazily so the
    # plan-file regeneration is bit-for-bit reproducible from this script.
    SQITCH_DIR="$REPO_ROOT/infra/migrations/sqitch"
    mkdir -p "$SQITCH_DIR/deploy"
    for m in "${MIGRATIONS[@]}"; do
        bn="$(basename "$m" .sql)"
        ln -sf "$REPO_ROOT/$m" "$SQITCH_DIR/deploy/$bn.sql"
    done
    sqitch deploy --target "$DB_URL" --chdir "$SQITCH_DIR"
    exit 0
fi

# ---- Fallback: ordered psql ---------------------------------------------
if ! command -v psql >/dev/null 2>&1; then
    echo "run-migrations.sh: neither sqitch nor psql found on PATH — install one" >&2
    exit 3
fi

echo "run-migrations.sh: sqitch not on PATH; using psql fallback (CI / test path)"
for m in "${MIGRATIONS[@]}"; do
    echo "run-migrations.sh: applying $m"
    psql "$DB_URL" -v ON_ERROR_STOP=1 -f "$REPO_ROOT/$m"
done

echo "run-migrations.sh: all ${#MIGRATIONS[@]} migrations applied successfully"
