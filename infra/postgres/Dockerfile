# Neksur Postgres + AGE image
#
# Base: apache/age:release_PG16_1.6.0
#   - Postgres 16 (the only AGE-stable major; see 00-RESEARCH.md §Standard Stack;
#     PG17 has open AGE issue #2111, PG18 only has AGE 1.7.0-rc).
#   - Apache AGE 1.6.0 (most recent GA release for PG16).
#
# Adds (layered on top of the upstream AGE image):
#   - pgaudit — Postgres-level audit-log of admin DDL (stepping stone to the
#     full Phase 6 hash-chain audit log).
#   - pg_stat_statements — query-level statistics; consumed by the observability
#     middleware in Plan 00-05 alongside the OTel Cypher wrapper.
#
# The shared_preload_libraries directive for these extensions lives in
# infra/postgres/postgresql.base.conf (mounted into the running container).
#
# This Dockerfile is for the Wave 0 scaffold — Plan 00-05 may add more layers
# (e.g., chrony for in-container time sync). Do NOT add production secrets or
# credentials here; that's Plan 00-04+ via env / mounts.

FROM apache/age:release_PG16_1.6.0

# Install pgaudit and pg_stat_statements for Postgres 16, plus python3 +
# patroni for HA orchestration (Plan 00-03 Wave 2). The apache/age image
# is Debian-based with Postgres 16 from the PGDG apt repo, so we install
# the matching extension packages from the same source.
USER root

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        postgresql-16-pgaudit \
        postgresql-contrib-16 \
        python3 \
        python3-pip \
        python3-venv; \
    rm -rf /var/lib/apt/lists/*

# Bake Patroni into the image so the HA compose can `exec patroni` directly
# without a runtime install (live verify 2026-05-13 surfaced exit-127 from
# the runtime pip3 pattern; this commit moves the install up-front).
# Use a venv to avoid PEP 668 "externally-managed-environment" rejection
# from the apt-managed Python on Debian trixie.
RUN set -eux; \
    python3 -m venv /opt/patroni-venv; \
    /opt/patroni-venv/bin/pip install --no-cache-dir \
        'patroni[etcd3]>=4.1' \
        'psycopg[binary]>=3.1'; \
    ln -s /opt/patroni-venv/bin/patroni /usr/local/bin/patroni; \
    ln -s /opt/patroni-venv/bin/patronictl /usr/local/bin/patronictl

# Drop back to the postgres user (the upstream image expects this).
USER postgres

# postgresql.base.conf is mounted in by the deployer (Plan 00-04 will produce
# the canonical compose/Helm chart that mounts it). The image itself does
# not bake config — staying as close to the upstream apache/age image as
# possible to keep the threat-model surface small (T-0-W0-SUPPLY).
