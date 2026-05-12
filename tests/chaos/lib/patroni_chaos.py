"""Patroni chaos-engineering driver — STUB.

Function signatures locked in Wave 0 (Plan 00-01) so downstream plans can
import them without circular failure. Real implementation lands in
Plan 00-03 (Wave 2 — Patroni HA), which fills these in against a running
3-node Patroni + etcd cluster spun up via testcontainers / docker-compose.

Public API
----------

``kill_primary(cluster_name)``
    SIGKILL the current Patroni primary; used to provoke failover.

``wait_for_new_leader(cluster_name, timeout_s=60)``
    Poll the Patroni REST API until a new node holds the leader lock.
    Returns the new leader's name.

``time_failover(cluster_name)``
    End-to-end: kill the primary, wait for a new leader, return the wall-
    clock elapsed seconds. Used by Plan 00-03's sub-30s failover assertion
    (D-001.15 contract).
"""

from __future__ import annotations


def kill_primary(cluster_name: str) -> None:
    """Kill the current Patroni primary in ``cluster_name``.

    Implementation: Plan 00-03 (Wave 2 — Patroni HA) will resolve the leader
    via the Patroni REST endpoint ``GET /cluster``, then issue a ``docker
    kill --signal=SIGKILL`` to that container.
    """
    raise NotImplementedError("Filled by Plan 03 — Wave 2 Patroni")


def wait_for_new_leader(cluster_name: str, timeout_s: int = 60) -> str:
    """Poll Patroni REST API for the new leader; return the leader's name.

    Implementation: Plan 00-03 (Wave 2 — Patroni HA) — polls
    ``GET /cluster`` every 500ms until ``role == "primary"`` appears on
    a new member, then returns that member's name. Raises ``TimeoutError``
    if ``timeout_s`` elapses with no new leader.
    """
    raise NotImplementedError("Filled by Plan 03 — Wave 2 Patroni")


def time_failover(cluster_name: str) -> float:
    """End-to-end failover timer; returns wall-clock seconds.

    Implementation: Plan 00-03 (Wave 2 — Patroni HA) — composed of
    ``kill_primary`` + ``wait_for_new_leader`` with monotonic wall-clock
    measurement. Phase 0 acceptance requires P95 of repeated runs to be
    <30s per D-001.15.
    """
    raise NotImplementedError("Filled by Plan 03 — Wave 2 Patroni")
