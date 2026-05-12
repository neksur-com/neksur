"""src.graph.tenant — tenant-context helpers for the Postgres + AGE pool.

Two surfaces live here:

* :func:`set_tenant_context` — issues ``SET LOCAL app.current_tenant = $1``
  on an *already-open* transaction. The value is scoped to the surrounding
  transaction by ``SET LOCAL`` semantics; ROLLBACK / COMMIT clears it. This
  is the only function that may modify ``app.current_tenant``.

* :func:`reset_session` — issues ``DISCARD ALL`` and is meant to be wired
  as the connection-pool reset hook (e.g., ``server_reset_query`` in
  PgBouncer, or the asyncpg / psycopg pool ``reset`` callback). It defends
  against Pitfall 5 (00-RESEARCH.md): session vars must not leak across
  connection reuse.

The functions are deliberately tiny and free-standing so they can be
called from any pool implementation. :class:`src.graph.client.GraphClient`
exposes :meth:`~src.graph.client.GraphClient.execute_in_tenant` as the
sugar context manager built on top of these primitives.

Pre-conditions / contract:
    * ``conn`` is a :class:`psycopg.AsyncConnection` (psycopg 3.x).
    * For :func:`set_tenant_context`, the caller has already started a
      transaction (``await conn.execute('BEGIN')`` or via the
      ``conn.transaction()`` context manager). ``SET LOCAL`` outside a
      txn is a no-op — Postgres silently drops it — and that would create
      a tenant-isolation hole. Production callers must ensure the txn.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from psycopg import AsyncConnection


async def set_tenant_context(conn: "AsyncConnection", tenant_id: str) -> None:
    """Set ``app.current_tenant`` for the surrounding transaction.

    Uses ``set_config(name, value, true)`` — equivalent to ``SET LOCAL``
    but parameter-bindable (Postgres rejects ``$1`` substitution for the
    value of a bare ``SET LOCAL`` statement; the function form accepts
    proper bind parameters). The ``is_local=true`` argument scopes the
    value to the active transaction; ``ROLLBACK``/``COMMIT`` clears it.

    The tenant id is bound as a parameter (``%s``), NEVER concatenated
    into the SQL string, to eliminate the injection surface even for
    trusted callers (T-0-INJ floor; Phase 5 ADR-004 hardens further).

    Args:
        conn: open async psycopg connection inside an active transaction.
        tenant_id: opaque tenant identifier; Postgres treats it as
            ``text`` and the RLS policy in V0030 compares it via
            ``(properties->>'tenant_id'::text) = current_setting(...)``.

    Raises:
        psycopg.Error: any Postgres-level failure.
    """
    await conn.execute(
        "SELECT set_config('app.current_tenant', %s, true)",
        (tenant_id,),
    )


async def reset_session(conn: "AsyncConnection") -> None:
    """Issue ``DISCARD ALL`` — wipes all session-level state.

    Wired as the connection-pool reset hook. ``DISCARD ALL`` clears
    prepared statements, temp tables, cursors, advisory locks, AND
    custom session variables like ``app.current_tenant``. Without this,
    Pitfall 5 says a returned-then-reacquired connection can leak the
    previous holder's tenant context into the new transaction.

    The integration test
    ``tests/security/test_rls_isolation.py::test_session_var_bleed``
    verifies this wiring end-to-end.
    """
    await conn.execute("DISCARD ALL")
