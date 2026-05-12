"""Security tier conftest — re-export integration fixtures.

Security tests (RLS isolation, FORCE-RLS bypass, Cypher injection,
parameter passthrough) need the same migrated-AGE container that the
integration tests use. Rather than duplicate the fixture, we re-export
it from the integration conftest. The depth-cap and label-whitelist
security tests don't need the DB and so don't request these fixtures —
they run as pure Python.

The re-export preserves the per-tier `conftest.py` ownership model
(``tests/conftest.py`` deliberately defines no fixtures so
``pytest tests/unit`` does not import testcontainers).
"""

from __future__ import annotations

# Re-export the session-scoped Postgres+AGE fixtures from the
# integration tier so the security tests can request them by name.
# The fixtures themselves do lazy imports of testcontainers /
# psycopg so collection-time stays cheap.
from tests.integration.conftest import (  # noqa: F401
    age_postgres,
    migrated_age_postgres,
    run_migrations_script_path,
    tenant_conn,
    tenant_conn_factory,
)
