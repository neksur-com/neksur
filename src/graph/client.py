"""src.graph.client — Neksur Postgres + AGE Cypher client (Phase 0 floor).

This module is the **application gateway** for every Cypher call. It enforces
the Phase 0 hardening contract:

1. **Parameterised Cypher.** :meth:`GraphClient.cypher` binds values via
   psycopg's parameter substitution. No string concatenation of caller-
   supplied values. Verified by
   ``tests/security/test_cypher_injection.py::test_parameter_passthrough_safe``.

2. **Label whitelist.** Any code path that takes a label name as a
   parameter must run it through :meth:`GraphClient._validate_label` first.
   The whitelist is the LOCKED set of 43 D-001.05 / D-001.06 identifiers
   amended by D-003.06 (19 vlabels + 24 elabels). Verified by
   ``tests/security/test_cypher_injection.py::test_string_concat_rejected_by_whitelist``.

3. **Tenant context.** :meth:`GraphClient.execute_in_tenant` opens a
   transaction, issues ``SET LOCAL app.current_tenant`` (via
   :func:`src.graph.tenant.set_tenant_context`), runs the user callback,
   commits, and triggers :func:`src.graph.tenant.reset_session` on connection
   return — defends against Pitfall 5.

4. **D-001.08 depth cap.** :meth:`GraphClient.cypher` calls
   :func:`_validate_traversal_depth` BEFORE submitting the query, raising
   :exc:`UnboundedTraversalError` on ``*``, ``*N..``, and ``*..``
   variable-length traversals. Bounded forms (``*N``, ``*N..M``, ``*..M``)
   pass through. Verified by
   ``tests/security/test_depth_cap.py`` (4 cases).

This is the Phase 0 baseline for the MCP ``graph.cypher`` hardening contract
(D-OQ.03 — fully wired in Phase 5 via ADR-004). Future revisions tighten:

* MCP-aware schema-bound query templates with a single allowed entry-point
  per pattern.
* Edge-label whitelist plus per-edge property whitelist.
* Time-budget enforcement at the wrapper layer (parallel to depth budget).

Do not add a Phase 5 hardening behaviour here — keep this file the
**floor** so Phase 5 ADR-004 can layer on top without ambiguity.
"""

from __future__ import annotations

import re
from contextlib import asynccontextmanager
from typing import TYPE_CHECKING, Any, AsyncIterator, Awaitable, Callable

from src.graph.tenant import reset_session, set_tenant_context

if TYPE_CHECKING:
    from psycopg import AsyncConnection
    from psycopg_pool import AsyncConnectionPool


# ---------------------------------------------------------------------------
# LABEL WHITELIST — 43 identifiers per D-001.05 + D-001.06 amended by D-003.06.
# This is the Phase 0 floor for D-OQ.03 / ADR-004 (Phase 5 MCP hardening).
# Order matches V0010 / V0030 for human-grep ergonomics; the set itself is
# order-insensitive (`frozenset`).
# ---------------------------------------------------------------------------
LABEL_WHITELIST: frozenset[str] = frozenset({
    # 19 vlabels (D-001.05 + D-003.06)
    "Table", "Column", "Snapshot", "Metric", "Dimension",
    "View", "Dashboard", "Pipeline", "Query", "Person",
    "Team", "Policy", "GlossaryTerm", "Tag", "DataContract",
    "Engine", "Catalog", "WriteEvent", "DetectionRun",
    # 18 mandatory elabels (D-001.06 + D-003.06)
    "LINEAGE_OF", "OWNS", "MEMBER_OF", "DEPENDS_ON", "CLASSIFIED_AS",
    "APPLIES_TO", "DEFINED_BY", "WROTE", "READ", "PRODUCES",
    "CONSUMES", "GOVERNED_BY", "STORED_IN", "RUNS_ON", "SUPERSEDES",
    "INTENDED_WRITE", "ACTUAL_WRITE", "VIOLATION_DETECTED_BY",
    # 6 supplement elabels (D-001.06)
    "BELONGS_TO", "OF_TABLE", "USED_ENGINE", "USES_DIMENSION",
    "RAN_ON", "GOVERNS",
})

assert len(LABEL_WHITELIST) == 43, (
    f"LABEL_WHITELIST has {len(LABEL_WHITELIST)} entries; "
    "D-001.05/.06 amended by D-003.06 requires exactly 43."
)


# ---------------------------------------------------------------------------
# Depth-cap pre-parser regex (D-001.08).
#
# Catches the three forbidden unbounded variable-length-traversal shapes:
#
#   *        (bare)        — flagged by `bare` group
#   *N..     (open upper)  — flagged by `lower_only` group
#   *..      (no bounds)   — flagged by `no_bounds` group
#
# Bounded shapes pass through:
#
#   *N         exact length
#   *N..M      both bounds
#   *..M       upper-only (lower defaults to 1 per D-001.08)
#
# A terminator after the unbounded form (`]`, whitespace, `)`, `|`, `,`,
# end-of-input) is what distinguishes "bounded" from "unbounded" — for
# example `*1..5` is matched against `lower_only` but the `(?=...)` lookahead
# requires a terminator immediately after the `..` and fails because `5` is
# next. That makes `*1..5` accepted.
# ---------------------------------------------------------------------------
_TERMINATOR = r"[\s\]\),\|]|$"
_UNBOUNDED_VLP_RE = re.compile(
    rf"""
    \*                              # the asterisk that opens a VLP
    (?:
        (?P<bare>(?={_TERMINATOR}))             # *  followed by terminator
      | (?P<lower_only>\d+\.\.(?={_TERMINATOR})) # *N..   (open upper)
      | (?P<no_bounds>\.\.(?={_TERMINATOR}))    # *..    (no bounds)
    )
    """,
    re.VERBOSE | re.MULTILINE,
)


class UnboundedTraversalError(ValueError):
    """Raised when a Cypher query contains a forbidden unbounded VLP.

    D-001.08 forbids ``*``, ``*N..``, and ``*..`` variable-length traversal
    patterns. Default depth is 3; max depth is 5. Bounded forms
    (``*N``, ``*N..M``, ``*..M``) are allowed and bounded-checked at the
    plan layer; we accept them here at the gateway and rely on the query
    planner / downstream tests for the upper-bound clamp.

    Phase 5 (ADR-004) replaces this with MCP-aware hardening; this is the
    Phase 0 floor. Tests:
    ``tests/security/test_depth_cap.py::test_unbounded_star_rejected``,
    ``test_open_upper_bound_rejected``, ``test_bounded_traversal_accepted``,
    ``test_default_depth_three_accepted``.
    """


def _validate_traversal_depth(query: str) -> None:
    """Raise :class:`UnboundedTraversalError` if ``query`` has an unbounded VLP.

    Scans the entire query text for the three forbidden shapes. Bounded
    forms — including ``*N``, ``*N..M``, ``*..M`` — pass through.

    This is a *pre-parser* — it runs before the query reaches Postgres /
    AGE. Defensive depth: the gateway never lets an unbounded traversal
    out of the application boundary, so even a planner regression in AGE
    cannot turn a missed depth cap into a tenant-wide DoS (T-0-DOS).

    Args:
        query: full Cypher statement text as it would be submitted.

    Raises:
        UnboundedTraversalError: when the first forbidden match is found.
            The message includes the offending substring for debuggability.
    """
    match = _UNBOUNDED_VLP_RE.search(query)
    if match is None:
        return

    if match.group("bare") is not None:
        offending = "*"
    elif match.group("lower_only") is not None:
        offending = match.group(0)  # e.g., "*1.."
    else:
        offending = "*.."

    raise UnboundedTraversalError(
        f"Unbounded traversal forbidden per D-001.08: {offending!r}; "
        "use *N..M with both bounds (default 1..3, max 1..5)."
    )


class GraphClient:
    """Thin wrapper around a psycopg async connection pool.

    Constructor takes an already-built :class:`psycopg_pool.AsyncConnectionPool`
    so the pool's lifecycle (open / close / size / reset hooks) is owned by
    the caller; tests can pass a single-connection pool and production wires
    up its own with ``server_settings={'application_name': 'neksur-graph'}``.

    The two operations that matter:

    * :meth:`cypher` — submit a parameterised Cypher statement against
      a graph. Runs :func:`_validate_traversal_depth` first; raises
      :class:`UnboundedTraversalError` before touching the database.

    * :meth:`execute_in_tenant` — context manager that opens a transaction,
      sets ``app.current_tenant``, yields a connection inside the txn, and
      on exit triggers :func:`~src.graph.tenant.reset_session` to wipe
      session state before the connection is returned to the pool.
    """

    def __init__(self, pool: "AsyncConnectionPool") -> None:
        self._pool = pool

    # ----- Validation primitives -------------------------------------------

    @staticmethod
    def _validate_label(label: str) -> None:
        """Reject any label not in :data:`LABEL_WHITELIST`.

        Labels cannot be parameterised in AGE Cypher (only values can),
        so any code path that takes a label name as input MUST whitelist.
        See Pattern 2 in 00-RESEARCH.md.

        Args:
            label: candidate label identifier.

        Raises:
            ValueError: when ``label`` is not in :data:`LABEL_WHITELIST`.
        """
        if label not in LABEL_WHITELIST:
            raise ValueError(
                f"Label {label!r} is not in the Phase 0 LABEL_WHITELIST. "
                "Add to D-001.05/.06 + this whitelist via ADR amendment, "
                "or refactor the call site to use a parameter-bound value."
            )

    # ----- Cypher execution ------------------------------------------------

    async def cypher(
        self,
        graph: str,
        query: str,
        params: dict[str, Any] | None = None,
    ) -> list[tuple]:
        """Submit a Cypher statement; return all rows as a list of tuples.

        Workflow:

            1. :func:`_validate_traversal_depth(query)` — depth cap (D-001.08).
            2. Acquire a pooled connection.
            3. ``LOAD 'age'`` + ``SET search_path`` (idempotent; cheap).
            4. ``SELECT * FROM cypher(:graph, $$ :query $$, :params)`` —
               with all caller-supplied values bound as **parameters**,
               never concatenated.
            5. Return ``rows``.

        Args:
            graph: AGE graph name (typically ``"neksur"``). Phase 0 has a
                single graph; whitelist not strictly needed yet but the
                pattern is set so Phase 5 can extend.
            query: Cypher statement text. May reference parameters via
                ``$paramname`` per AGE convention. Must NOT contain
                unbounded variable-length traversals.
            params: parameter bindings keyed by parameter name. Values
                go through psycopg's binder — safe against injection.

        Raises:
            UnboundedTraversalError: depth cap violation (pre-DB).
            psycopg.Error: Postgres-level failure.

        Notes:
            This is the Phase 0 *floor* — the implementation deliberately
            does NOT add OpenTelemetry instrumentation; that lives in
            Plan 00-05 (Wave 4) and wraps this call.
        """
        _validate_traversal_depth(query)

        # The query is parameterised; psycopg passes `params` through
        # libpq's bind interface, never concatenating. This is the
        # T-0-INJ floor.
        import json

        bound_params_agtype = json.dumps(params or {})
        sql = "SELECT * FROM cypher(%s, %s, %s::agtype) AS (r agtype)"

        async with self._pool.connection() as conn:
            async with conn.cursor() as cur:
                await cur.execute("LOAD 'age'")
                await cur.execute(
                    'SET search_path = ag_catalog, "$user", public'
                )
                await cur.execute(sql, (graph, query, bound_params_agtype))
                rows = await cur.fetchall()
        return rows

    # ----- Tenant-scoped execution -----------------------------------------

    @asynccontextmanager
    async def execute_in_tenant(
        self,
        tenant_id: str,
    ) -> AsyncIterator["AsyncConnection"]:
        """Open a transaction bound to ``tenant_id``; yield the connection.

        Workflow:

            1. Acquire connection from pool.
            2. ``BEGIN`` (via psycopg's transaction context).
            3. :func:`~src.graph.tenant.set_tenant_context` —
               ``SET LOCAL app.current_tenant = $1``.
            4. Yield the connection to the user.
            5. ``COMMIT`` on clean exit; ``ROLLBACK`` on exception.
            6. :func:`~src.graph.tenant.reset_session` — ``DISCARD ALL``
               wipes session state (Pitfall 5 mitigation).
            7. Connection returns to pool.

        The integration test
        ``tests/integration/test_rls.py::test_self_read_visible`` and
        the security test
        ``tests/security/test_rls_isolation.py::test_no_cross_tenant_read``
        both run inside this context manager.
        """
        async with self._pool.connection() as conn:
            try:
                async with conn.transaction():
                    await set_tenant_context(conn, tenant_id)
                    yield conn
            finally:
                # DISCARD ALL is the Pitfall 5 mitigation. We run it on the
                # *outer* connection scope (after the txn closes) so that
                # the pool returns a connection with a wiped session.
                await reset_session(conn)


@asynccontextmanager
async def execute_in_tenant(
    client: GraphClient,
    tenant_id: str,
) -> AsyncIterator["AsyncConnection"]:
    """Module-level convenience wrapper around :meth:`GraphClient.execute_in_tenant`.

    Lets callers write::

        from src.graph import execute_in_tenant

        async with execute_in_tenant(client, "tenant-a") as conn:
            ...

    instead of going through the method. Behaviour is identical.
    """
    async with client.execute_in_tenant(tenant_id) as conn:
        yield conn
