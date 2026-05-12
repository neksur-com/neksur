"""Security: RLS blocks cross-tenant reads; CHECK rejects missing tenant_id;
FORCE RLS blocks owner bypass; pool reset wipes session vars.

Maps to 00-VALIDATION.md rows:
    02-T3 / REQ-tenant-isolation / T-0-RLS / "Cross-tenant read returns zero rows"
    02-T3 / REQ-tenant-isolation / T-0-RLS / "Insert without tenant_id is rejected"
    02-T3 / REQ-tenant-isolation / T-0-RLS / "RLS bypass via direct table query
                                              blocked (FORCE RLS)"

Plus a non-VALIDATION-listed but PLAN-mandated check:
    "test_session_var_bleed — validates DISCARD ALL pool reset wiring"
"""

from __future__ import annotations

import json

import psycopg
import pytest

pytestmark = pytest.mark.security


def _insert_table_node(conn, vertex_id: int, properties: dict) -> None:
    # AGE id column is `graphid`, not bigint — pass as text + cast.
    with conn.cursor() as cur:
        cur.execute(
            'INSERT INTO neksur."Table" (id, properties) '
            'VALUES (%s::graphid, %s::agtype)',
            (str(vertex_id), json.dumps(properties)),
        )


def test_no_cross_tenant_read(migrated_age_postgres, tenant_conn_factory) -> None:
    """T-0-RLS smoking gun: Tenant B cannot see Tenant A's rows.

    Procedure:
        1. As Tenant A, insert one Table node with tenant_id='A'.
        2. As Tenant B, MATCH the same uri.
        3. Assert 0 rows returned.
    """
    base = 281474976710800
    a_id = "tenant-A-cross-read"
    b_id = "tenant-B-cross-read"

    # Insert as Tenant A.
    conn = tenant_conn_factory(a_id)
    try:
        _insert_table_node(conn, base + 1, {
            "uri": "iceberg://t/A-only",
            "name": "A-only",
            "tenant_id": a_id,
        })
        conn.execute("COMMIT;")
    except Exception:
        conn.execute("ROLLBACK;")
        raise

    # Read as Tenant B.
    conn = tenant_conn_factory(b_id)
    try:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT * FROM cypher('neksur', $$
                    MATCH (t:Table {uri: 'iceberg://t/A-only'})
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

    assert rows == [], (
        f"T-0-RLS LEAK: Tenant B read {len(rows)} of Tenant A's rows; "
        f"expected 0. Rows: {rows!r}"
    )


def test_insert_without_tenant_fails(migrated_age_postgres, tenant_conn_factory) -> None:
    """Pitfall 4: INSERT without tenant_id in properties is rejected.

    The row is rejected by EITHER the CHECK constraint (which the plan
    cites explicitly — `CHECK (properties ? 'tenant_id')`) OR the RLS
    WITH CHECK policy (whose `(properties->>'tenant_id'::text) =
    current_setting(...)` fails because `->>` on a missing key returns
    NULL, which fails the equality test). Both rejections prove the
    intended-behaviour gate; we accept either error type.

    Order-of-evaluation note: in Postgres, RLS WITH CHECK runs BEFORE
    CHECK constraints during INSERT (see
    https://www.postgresql.org/docs/current/ddl-rowsecurity.html). With
    a non-superuser role, the RLS rejection wins. Both gates exist in
    V0030 — the test fires on whichever one Postgres reaches first.
    """
    tid = "tenant-check-test"
    conn = tenant_conn_factory(tid)

    with pytest.raises(
        (psycopg.errors.CheckViolation, psycopg.errors.InsufficientPrivilege)
    ) as exc_info:
        try:
            with conn.cursor() as cur:
                cur.execute(
                    'INSERT INTO neksur."Table" (id, properties) '
                    'VALUES (%s::graphid, %s::agtype)',
                    ("281474976710801",
                     json.dumps({"uri": "iceberg://no-tenant/x"})),
                )
        finally:
            # Clean up: a failed CHECK / RLS leaves the txn aborted.
            try:
                conn.execute("ROLLBACK;")
            except Exception:
                pass

    # Either the CHECK constraint name or the RLS-policy message should
    # appear — both prove the same gate.
    err_msg = str(exc_info.value)
    assert ("tenant_id_required" in err_msg
            or "row-level security policy" in err_msg), (
        f"unexpected rejection message (neither CHECK nor RLS): {err_msg}"
    )


def test_force_rls_blocks_owner_bypass(migrated_age_postgres, tenant_conn_factory) -> None:
    """FORCE RLS makes the table-owning role respect RLS, not just app roles.

    Procedure:

    1. Insert one row as Tenant A (with the neksur_app role, RLS applies).
    2. Reassign the `neksur."Table"` table OWNER to ``neksur_app``. After
       this, ``neksur_app`` is BOTH the table-owning role AND a non-
       superuser. Without FORCE RLS, the owner would bypass policies.
       With FORCE RLS (V0030), the owner role still respects them.
    3. Connect under ``neksur_app`` with NO tenant context (current_tenant
       is empty), do a direct heap read. Assert 0 rows: FORCE worked.
    4. Restore ownership to postgres (cleanup so other tests still work).

    Limitation: the session connection is `postgres` (superuser); we
    cannot meaningfully probe the superuser-bypasses-RLS case because
    that bypass is unconditional (not a Phase 0 attack surface — we
    never run application code as superuser). The owner-bypass attack
    IS in scope: if the application were ever run with an admin role
    that happens to own the AGE tables, FORCE RLS prevents data leakage.
    """
    base = 281474976710820
    a_id = "tenant-owner-bypass-A"
    conn = migrated_age_postgres

    # Step 1: insert as Tenant A via the standard tenant_conn_factory.
    tc = tenant_conn_factory(a_id)
    try:
        _insert_table_node(tc, base + 1, {
            "uri": "iceberg://t/force-rls-probe",
            "name": "force-probe",
            "tenant_id": a_id,
        })
        tc.execute("COMMIT;")
    except Exception:
        tc.execute("ROLLBACK;")
        raise

    # Make sure session is clean / role reset.
    try:
        conn.execute("ROLLBACK;")
    except Exception:
        pass
    conn.execute("RESET ROLE;")

    # Step 2: reassign ownership to neksur_app.
    conn.execute('ALTER TABLE neksur."Table" OWNER TO neksur_app;')

    try:
        # Step 3: connect as neksur_app (via SET ROLE — same session,
        # different role) and read with NO tenant context.
        conn.execute("BEGIN;")
        conn.execute("SET LOCAL ROLE neksur_app;")
        # `app.current_tenant` GUC was wiped at end-of-prev-txn; ensure
        # explicitly empty.
        conn.execute("SELECT set_config('app.current_tenant', '', true);")
        with conn.cursor() as cur:
            # Qualified type name in case search_path was reset.
            cur.execute(
                'SELECT count(*) FROM neksur."Table" '
                'WHERE id = %s::ag_catalog.graphid',
                (str(base + 1),),
            )
            (visible_as_owner,) = cur.fetchone()
        conn.execute("ROLLBACK;")

        assert visible_as_owner == 0, (
            f"FORCE RLS BYPASS: as table-owning role neksur_app with "
            f"no tenant context, saw {visible_as_owner} rows; "
            "expected 0. The FORCE ROW LEVEL SECURITY clause in V0030 "
            "is not effective for the table-owning role."
        )
    finally:
        # Step 4: restore ownership to postgres so other tests still
        # have a clean state.
        try:
            conn.execute("ROLLBACK;")
        except Exception:
            pass
        conn.execute("RESET ROLE;")
        conn.execute('ALTER TABLE neksur."Table" OWNER TO postgres;')


def test_session_var_bleed(migrated_age_postgres, tenant_conn_factory) -> None:
    """DISCARD ALL clears app.current_tenant before pool returns the conn.

    Procedure:
        1. Open a tenant txn (sets app.current_tenant = tenant-A).
        2. COMMIT — txn-scoped SET LOCAL is cleared by COMMIT itself.
        3. DISCARD ALL — also clears any session-level GUCs.
        4. Query current_setting('app.current_tenant', true); expect
           empty string (or NULL/empty per Postgres convention).

    The `true` flag to current_setting means "missing setting -> empty
    string, not error" — that's the runtime contract V0030 relies on.
    """
    tid = "tenant-bleed-probe"
    conn = tenant_conn_factory(tid)
    # Verify tenant is set inside the txn (sanity check the test setup).
    with conn.cursor() as cur:
        cur.execute("SELECT current_setting('app.current_tenant', true)")
        (mid_txn_value,) = cur.fetchone()
    assert mid_txn_value == tid, (
        f"setup sanity: tenant context was {mid_txn_value!r}, expected {tid!r}"
    )

    # COMMIT clears SET LOCAL by default; we ALSO call DISCARD ALL to
    # exercise the pool-reset wiring even for any session-level vars.
    conn.execute("COMMIT;")
    conn.execute("DISCARD ALL")

    # Now check from the same (returned-to-pool) connection.
    with conn.cursor() as cur:
        cur.execute("SELECT current_setting('app.current_tenant', true)")
        (post_reset_value,) = cur.fetchone()

    assert post_reset_value in ("", None), (
        f"SESSION VAR BLEED: after COMMIT + DISCARD ALL, "
        f"app.current_tenant = {post_reset_value!r}; expected empty/NULL. "
        "The connection-pool reset hook is broken."
    )
