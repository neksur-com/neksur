"""Phase 0 envelope load-seed fixture — STUB.

The Phase 0 acceptance envelope is **10M nodes / 50M edges** (per ADR-001
and ROADMAP.md Phase 0 §Phase Details). This module's ``seed`` function
will materialize that volume of synthetic metadata-graph data via the
COPY-then-MERGE pattern from 00-RESEARCH.md §Code Examples (or AGEFreighter
with ``use_copy=True`` if Python is the runtime — D-W0-runtime-pick.md
confirms Python).

Real implementation lands in **Plan 00-06 (Wave 5 — load envelope)**.
This Wave 0 stub exists so chaos / DR drill scripts that consume the seed
output (e.g., kill-primary-mid-load tests in Plan 00-04) can import their
upstream dependency without circular import failure.

Expected return shape
---------------------

``{"nodes_created": int, "edges_created": int, "duration_s": float}``

- ``nodes_created`` ≥ ``target_nodes`` (callers may request a smaller
  fraction of the envelope for fast smoke tests).
- ``edges_created`` ≥ ``target_edges``.
- ``duration_s`` is wall-clock seconds taken; used by Plan 00-06's
  throughput acceptance gate (≥ 100k nodes/s sustained).
"""

from __future__ import annotations

from typing import Any


def seed(
    conn: Any,
    *,
    target_nodes: int = 10_000_000,
    target_edges: int = 50_000_000,
) -> dict[str, Any]:
    """Seed synthetic Phase 0 envelope data into ``conn``.

    Parameters
    ----------
    conn:
        A connected ``psycopg.Connection`` against the test Postgres+AGE
        instance (from ``tests/integration/conftest.py::age_postgres``).
    target_nodes:
        Desired node count. Default: 10M (full Phase 0 envelope).
    target_edges:
        Desired edge count. Default: 50M (full Phase 0 envelope).

    Returns
    -------
    dict
        ``{"nodes_created": int, "edges_created": int, "duration_s": float}``

    Raises
    ------
    NotImplementedError
        Always — real implementation lands in Plan 00-06 (Wave 5).
    """
    raise NotImplementedError("Filled by Plan 06 — Wave 5 envelope load")
