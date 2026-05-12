"""Integration: RLS positive path — self-tenant reads see own nodes.

Maps to 00-VALIDATION.md row:
    02-T3 / REQ-tenant-isolation / "Self-tenant read sees own nodes".

This is the positive counterpart to ``test_no_cross_tenant_read``: with
the correct tenant set via ``SET LOCAL app.current_tenant``, a tenant's
own rows must be visible. If this fails, RLS is over-restricting (which
breaks the application entirely, not just tenant isolation).
"""

from __future__ import annotations

import json

import pytest

pytestmark = pytest.mark.integration


def _insert_table_node(conn, vertex_id: int, properties: dict) -> None:
    # AGE id column is `graphid`, not bigint — pass as text + cast.
    with conn.cursor() as cur:
        cur.execute(
            'INSERT INTO neksur."Table" (id, properties) '
            'VALUES (%s::graphid, %s::agtype)',
            (str(vertex_id), json.dumps(properties)),
        )


def test_self_read_visible(migrated_age_postgres, tenant_conn_factory) -> None:
    """Insert as Tenant A; read within Tenant A context; row is visible."""
    tid = "tenant-self-read"
    conn = tenant_conn_factory(tid)
    base = 281474976710900

    try:
        _insert_table_node(conn, base + 1, {
            "uri": "iceberg://t/self/visible",
            "name": "visible",
            "tenant_id": tid,
        })
        conn.execute("COMMIT;")
    except Exception:
        conn.execute("ROLLBACK;")
        raise

    # Open a fresh tenant txn and read.
    conn = tenant_conn_factory(tid)
    try:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT * FROM cypher('neksur', $$
                    MATCH (t:Table {uri: 'iceberg://t/self/visible'})
                    RETURN t
                $$) AS (t agtype)
                """
            )
            rows = cur.fetchall()
    finally:
        try:
            conn.execute("ROLLBACK;")
        except Exception:
            pass

    assert len(rows) == 1, (
        f"self-tenant read returned {len(rows)} rows; expected exactly 1 — "
        "RLS may be over-restricting and dropping legitimate rows"
    )
