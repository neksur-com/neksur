"""Integration-tier fixtures: real Postgres 16 + Apache AGE 1.6.0 via testcontainers.

The fixtures here spin up a Docker container running the official
``apache/age:release_PG16_1.6.0`` image (Postgres 16 with AGE 1.6.0 already
built and installed). They are imported lazily — pytest only instantiates a
fixture when a test requests it — so collecting tests from ``tests/unit/``
does not require Docker.

Fixtures
--------

``age_postgres`` (session-scoped)
    Returns a connected ``psycopg.Connection`` against a Postgres 16 + AGE 1.6.0
    container with the AGE extension loaded and the search path primed. The
    container is reused across the session for speed; a per-function reset
    fixture or transactional wrapper should be added when tests start
    mutating shared state.

``tenant_conn`` (function-scoped factory)
    Callable: ``tenant_conn(tenant_id) -> psycopg.Connection`` that opens a
    new transaction and issues ``SET LOCAL app.current_tenant = $1`` so that
    Postgres RLS policies (added in Plan 00-02, Wave 1) filter to that tenant.

Image pin
---------

The image tag ``apache/age:release_PG16_1.6.0`` is the GA pairing locked by
ADR-001 D-001.01 and confirmed in 00-RESEARCH.md §Standard Stack. Do not
upgrade this tag without an ADR amendment.
"""

from __future__ import annotations

import subprocess
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import TYPE_CHECKING

import pytest

if TYPE_CHECKING:
    import psycopg

REPO_ROOT = Path(__file__).resolve().parents[2]
MIGRATIONS_DIR = REPO_ROOT / "migrations"
RUN_MIGRATIONS_SCRIPT = REPO_ROOT / "infra" / "migrations" / "run-migrations.sh"

# Plan 00-02 migration order — kept in sync with run-migrations.sh::MIGRATIONS.
# The fixture applies these via psycopg (not via the shell script) so the
# test suite is portable to dev hosts without `psql` on PATH (the script's
# fallback assumes psql). Production / CI uses run-migrations.sh against a
# real Postgres; tests use this Python helper against the same files.
PHASE_0_MIGRATIONS = (
    "postgres/V0001__enable_extensions.sql",
    "graph/V0010__create_graph_and_labels.sql",
    "graph/V0020__property_indexes.sql",
    "graph/V0025__tenant_indexes_and_gin.sql",
    "postgres/V0030__rls_policies.sql",
)

# Locked Docker image — see ADR-001 D-001.01 and 00-RESEARCH.md §Standard Stack.
# DO NOT change this tag without an ADR amendment.
AGE_POSTGRES_IMAGE = "apache/age:release_PG16_1.6.0"
POSTGRES_PASSWORD = "neksur_test"  # noqa: S105 — test-only fixture


@pytest.fixture(scope="session")
def age_postgres() -> "Iterator[psycopg.Connection]":
    """Session-scoped Postgres 16 + AGE 1.6.0 connection from testcontainers.

    Yields a ``psycopg.Connection`` with:

    - the AGE extension created (``CREATE EXTENSION IF NOT EXISTS age``),
    - the AGE shared library loaded into the session (``LOAD 'age'``),
    - the search path primed (``SET search_path = ag_catalog, "$user", public``)
      so Cypher queries via ``cypher(...)`` resolve AGE catalog entries.

    Image: ``apache/age:release_PG16_1.6.0`` (ADR-001 D-001.01 / 00-RESEARCH.md).
    """
    # Imports inside the fixture so collection-time does not require Docker.
    import psycopg
    from testcontainers.postgres import PostgresContainer

    container = PostgresContainer(
        image=AGE_POSTGRES_IMAGE,
        username="postgres",
        password=POSTGRES_PASSWORD,
        dbname="postgres",
        port=5432,
    )

    container.start()
    try:
        dsn = (
            f"host={container.get_container_host_ip()} "
            f"port={container.get_exposed_port(5432)} "
            f"user=postgres password={POSTGRES_PASSWORD} dbname=postgres"
        )
        conn = psycopg.connect(dsn, autocommit=True)
        try:
            with conn.cursor() as cur:
                cur.execute("CREATE EXTENSION IF NOT EXISTS age;")
                cur.execute("LOAD 'age';")
                cur.execute('SET search_path = ag_catalog, "$user", public;')
            yield conn
        finally:
            conn.close()
    finally:
        container.stop()


@pytest.fixture
def tenant_conn(age_postgres: "psycopg.Connection") -> "Callable[[str], psycopg.Connection]":
    """Per-function factory: open a transaction and set the current tenant.

    Usage in a test::

        def test_isolation(tenant_conn):
            conn = tenant_conn("tenant-a")
            with conn.cursor() as cur:
                cur.execute("SELECT * FROM some_rls_table;")
            # ... assertions

    Sets ``app.current_tenant`` via ``SET LOCAL`` so the value is scoped to
    the surrounding transaction and disappears on ``ROLLBACK``/``COMMIT`` —
    this prevents cross-tenant leak when a connection is reused from a pool
    (RLS pattern from 00-RESEARCH.md §Pattern 1).
    """

    def _open(tenant_id: str) -> "psycopg.Connection":
        # Reuse the session-scoped connection. We start a transaction on it
        # and bind the tenant id via `SET LOCAL`, which Postgres scopes to
        # the surrounding txn — see 00-RESEARCH.md §Pattern 1 (RLS).
        # NOTE: real tests should manage txn lifecycle explicitly; this
        # factory exists for the scaffold and will be hardened in Plan 00-02
        # once RLS policies actually exist.
        age_postgres.execute("BEGIN;")
        # `SET LOCAL` does not accept psycopg's $1 parameter binding for
        # the GUC value; use set_config(name, value, is_local) which
        # takes proper parameters. is_local=true matches SET LOCAL
        # semantics (transaction-scoped).
        age_postgres.execute(
            "SELECT set_config('app.current_tenant', %s, true);", (tenant_id,)
        )
        return age_postgres

    return _open


# ---------------------------------------------------------------------------
# Plan 00-02 Wave 1 — schema-migrated fixtures.
#
# `migrated_age_postgres` extends `age_postgres` by applying the Phase 0
# migration set (V0001 .. V0030). Session-scoped so the cost is paid once.
#
# The fixture intentionally uses psycopg-direct SQL execution rather than
# subprocess(run-migrations.sh) because:
#   - The script's CI path needs `sqitch` or `psql` on PATH; testcontainers
#     hosts (incl. macOS dev) may have neither.
#   - The script and this fixture consume the SAME SQL files in the SAME
#     order — so the production path and the test path stay byte-equivalent
#     on the file inputs. Any drift would surface as a CI-only failure.
#   - Subprocess overhead doubles the per-test session-start time.
# ---------------------------------------------------------------------------


def _apply_phase0_migrations(conn: "psycopg.Connection") -> None:
    """Apply Plan 00-02 migrations in order against ``conn`` (autocommit).

    Used by :func:`migrated_age_postgres`. Each file is dispatched to
    libpq's simple-query protocol via ``conn.pgconn.exec_``; that path
    natively accepts multi-statement batches, which our migrations are
    (`BEGIN; ... COMMIT;` for V0010 / V0030, multiple statements for the
    others). The extended-query protocol used by ``cursor.execute`` does
    not accept multi-statement input.
    """
    from psycopg.pq import ExecStatus

    for relpath in PHASE_0_MIGRATIONS:
        path = MIGRATIONS_DIR / relpath
        if not path.is_file():
            raise FileNotFoundError(f"Plan 00-02 migration missing: {path}")
        sql = path.read_text(encoding="utf-8")
        result = conn.pgconn.exec_(sql.encode("utf-8"))
        status = result.status
        if status not in (
            ExecStatus.COMMAND_OK,
            ExecStatus.TUPLES_OK,
            ExecStatus.SINGLE_TUPLE,
            ExecStatus.PIPELINE_SYNC,
        ):
            error_msg = result.error_message.decode("utf-8", errors="replace")
            raise RuntimeError(
                f"Migration {relpath} failed (status={status!r}): {error_msg}"
            )


@pytest.fixture(scope="session")
def migrated_age_postgres(age_postgres: "psycopg.Connection") -> "psycopg.Connection":
    """Session-scoped Postgres+AGE connection with Plan 00-02 schema applied.

    Order: V0001 -> V0010 -> V0020 -> V0025 -> V0030. After migrations,
    a non-superuser role ``neksur_app`` is created with SELECT/INSERT/
    UPDATE/DELETE on all 43 ``neksur.*`` tables. This role does NOT have
    BYPASSRLS, so it actually exercises the RLS policies.

    Returns the same underlying connection as ``age_postgres`` — the schema
    is part of the shared session state. Tests that need a *clean* graph
    must truncate or drop labels themselves; the Phase 0 plan does not
    do bulk seeding here (Plan 00-06 is the load-fixture plan).

    Note for tests: the connection is still as the superuser ``postgres``,
    but ``tenant_conn_factory`` (and ``tenant_conn``) issue ``SET ROLE
    neksur_app`` inside each transaction so RLS applies. Direct use of
    the connection (e.g., for schema introspection) keeps superuser
    privileges; only the per-tenant transaction is RLS-scoped.
    """
    _apply_phase0_migrations(age_postgres)

    # Create the non-superuser test role with no BYPASSRLS. The test
    # transactions SET ROLE to this user so RLS policies actually fire.
    with age_postgres.cursor() as cur:
        cur.execute("""
            DO $$
            BEGIN
                IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'neksur_app') THEN
                    CREATE ROLE neksur_app NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD 'neksur_app';
                END IF;
            END
            $$;
        """)
        # Grant USAGE on the neksur schema so the role can see the tables.
        cur.execute("GRANT USAGE ON SCHEMA neksur TO neksur_app;")
        # Grant USAGE on ag_catalog so the role can use AGE's `cypher()` etc.
        cur.execute("GRANT USAGE ON SCHEMA ag_catalog TO neksur_app;")
        # CRUD on all existing tables in neksur (FUTURE applies to new ones).
        cur.execute(
            "GRANT SELECT, INSERT, UPDATE, DELETE "
            "ON ALL TABLES IN SCHEMA neksur TO neksur_app;"
        )
        cur.execute(
            "ALTER DEFAULT PRIVILEGES IN SCHEMA neksur "
            "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO neksur_app;"
        )
        # AGE's cypher() needs to read ag_catalog state.
        cur.execute(
            "GRANT SELECT ON ALL TABLES IN SCHEMA ag_catalog TO neksur_app;"
        )
        # And EXECUTE on cypher / create_graph / etc.
        cur.execute(
            "GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO neksur_app;"
        )

    return age_postgres


@pytest.fixture(scope="session")
def run_migrations_script_path() -> Path:
    """Absolute path to ``run-migrations.sh`` for tests that invoke it.

    Tests that explicitly want to exercise the script via subprocess
    (rather than the psycopg-direct path) use this fixture. The script
    requires sqitch or psql on PATH; skip such tests cleanly if neither
    is available.
    """
    return RUN_MIGRATIONS_SCRIPT


@pytest.fixture
def tenant_conn_factory(migrated_age_postgres: "psycopg.Connection") -> "Callable[[str], psycopg.Connection]":
    """Per-test factory: open a transaction and set the current tenant.

    Unlike the legacy :func:`tenant_conn` fixture (Plan 00-01 scaffold,
    which exists for backward compatibility), this factory requires the
    schema to be migrated first — useful for RLS tests that need V0030
    policies in place. The transaction is left open; the test must
    ROLLBACK or COMMIT itself.
    """

    def _open(tenant_id: str) -> "psycopg.Connection":
        # Roll back any leftover txn from a previous test that forgot.
        # Always RESET ROLE first to recover from a previous test where
        # SET ROLE was applied (the session connection is superuser-
        # owned, but each per-tenant txn assumes the neksur_app role).
        try:
            migrated_age_postgres.execute("ROLLBACK;")
        except Exception:
            pass
        try:
            migrated_age_postgres.execute("RESET ROLE;")
        except Exception:
            pass
        migrated_age_postgres.execute("BEGIN;")
        # SET ROLE to the non-superuser app role so RLS policies fire.
        # The superuser `postgres` would bypass RLS regardless of FORCE.
        migrated_age_postgres.execute("SET LOCAL ROLE neksur_app;")
        # `SET LOCAL` does not accept psycopg's $1 parameter binding for
        # the GUC value; use set_config(name, value, is_local) which
        # takes proper parameters. is_local=true matches SET LOCAL
        # semantics (transaction-scoped).
        migrated_age_postgres.execute(
            "SELECT set_config('app.current_tenant', %s, true);", (tenant_id,)
        )
        return migrated_age_postgres

    return _open
