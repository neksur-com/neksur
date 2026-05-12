"""src.graph — Neksur metadata-graph client (Postgres + Apache AGE 1.6.0).

Public surface:
    - :class:`GraphClient`              — parameterised Cypher with depth-cap
                                          pre-parser, label whitelist, and
                                          per-transaction tenant context.
    - :func:`execute_in_tenant`         — async context manager wrapping
                                          ``SET LOCAL app.current_tenant``.
    - :exc:`UnboundedTraversalError`    — raised by the depth-cap pre-parser
                                          for ``*``, ``*N..``, ``*..``.
    - :func:`set_tenant_context`        — low-level helper used by the
                                          context manager.
    - :func:`reset_session`             — issues ``DISCARD ALL``; used as
                                          connection-pool reset hook.

Phase 0 floor only — Phase 5 ADR-004 layers MCP-aware hardening on top.
"""

from src.graph.client import (
    GraphClient,
    LABEL_WHITELIST,
    UnboundedTraversalError,
    execute_in_tenant,
)
from src.graph.tenant import reset_session, set_tenant_context

__all__ = [
    "GraphClient",
    "LABEL_WHITELIST",
    "UnboundedTraversalError",
    "execute_in_tenant",
    "reset_session",
    "set_tenant_context",
]
