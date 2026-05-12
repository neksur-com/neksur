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

from collections.abc import Callable, Iterator
from typing import TYPE_CHECKING

import pytest

if TYPE_CHECKING:
    import psycopg

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
        # psycopg parameterizes by `%s`; SET LOCAL takes a literal so we
        # quote-escape via psycopg's literal binding.
        age_postgres.execute(
            "SET LOCAL app.current_tenant = %s;", (tenant_id,)
        )
        return age_postgres

    return _open
