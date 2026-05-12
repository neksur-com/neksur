"""Neksur — cross-engine Iceberg policy enforcement with metadata graph foundation.

This is the top-level package marker. Subpackages:
    - ``src.graph``: Phase 0 metadata-graph Postgres + AGE client (this plan).

Phase 0 ships the application gateway floor: parameterised Cypher, label
whitelist, tenant context via ``SET LOCAL``, ``DISCARD ALL`` pool reset, and
the D-001.08 depth-cap pre-parser. Phase 5 (ADR-004) layers MCP-aware
hardening on top.
"""
