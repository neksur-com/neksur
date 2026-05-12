"""Top-level pytest conftest for the Neksur test suite.

Test tiers (one subdirectory per tier):

- ``tests/unit/`` — pure-Python unit tests, no external resources. Runs per
  commit in ``.github/workflows/unit.yml``.
- ``tests/integration/`` — spins up Postgres 16 + Apache AGE 1.6.0 via
  testcontainers. Fixtures live in ``tests/integration/conftest.py``. Runs
  per pull request in ``.github/workflows/integration.yml``.
- ``tests/load/`` — load-generated envelope tests at the Phase 0 scale
  (10M nodes / 50M edges). Fixtures under ``tests/load/fixtures/``. Heavy —
  runs nightly in ``.github/workflows/load-chaos-restore.yml``.
- ``tests/chaos/`` — chaos-engineering tests (kill primary, network
  partitions, replication-lag injection). Drivers under ``tests/chaos/lib/``.
- ``tests/security/`` — tenant-isolation / RLS / audit-log assertions.
- ``tests/dr/`` — disaster-recovery drill scripts (Bash, not Python — exec'd
  from CI via ``subprocess``).

Pytest markers (declared in ``pyproject.toml``):
``integration``, ``load``, ``chaos``, ``security``, ``dr``.

This top-level conftest deliberately defines no fixtures — each tier owns its
fixture set in its own ``conftest.py`` so that ``pytest tests/unit`` does not
import (and therefore does not need) testcontainers / psycopg.
"""
