-- =====================================================================
-- V0001 — Enable required Postgres extensions for the Neksur stack.
--
-- Order: this migration MUST run before V0010 (which `LOAD 'age'` and
-- creates the graph). Each extension is its own statement; per Pitfall 7
-- (00-RESEARCH.md), AGE catalog mutations are not always fully transactional,
-- so we isolate the extension installs with explicit transaction comment
-- headers — if one fails, the operator inspects exactly that statement.
--
-- pgaudit + pg_stat_statements also require entries in
-- shared_preload_libraries (set in infra/postgres/postgresql.base.conf:
-- shared_preload_libraries = 'age,pgaudit,pg_stat_statements'). The
-- CREATE EXTENSION calls here register the SQL objects; the loader
-- itself is configured at Postgres-server startup.
--
-- Pitfall 1 (00-RESEARCH.md): "AGE graph name" and "Postgres schema name"
-- are distinct. The AGE graph created in V0010 is named `neksur`; do NOT
-- confuse it with future application schemas (prefix `neksur_app_*`).
-- See infra/postgres/age-naming.md.
-- =====================================================================

-- Transaction header: pg_stat_statements (cheap; harmless if already loaded)
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Transaction header: pgaudit (DDL audit trail — Phase 6 hardens further).
--
-- pgaudit is REQUIRED in production (Phase 0 production image bundles it
-- via infra/postgres/Dockerfile). In testcontainer-only dev/test runs the
-- base apache/age image does NOT include it, so we conditionally install:
-- if the extension files are present, create it; otherwise emit a NOTICE
-- and continue. The Phase 0 test gates don't depend on pgaudit (it's the
-- stepping stone to the Phase 6 hash-chain audit log); production gates
-- WILL fail on the image-builder step if the package is missing.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_available_extensions WHERE name = 'pgaudit'
    ) THEN
        CREATE EXTENSION IF NOT EXISTS pgaudit;
    ELSE
        RAISE NOTICE 'pgaudit not available (testcontainer image); '
                     'skipped. Production image must include it — see '
                     'infra/postgres/Dockerfile.';
    END IF;
END
$$ LANGUAGE plpgsql;

-- Transaction header: age (graph extension — must come last so any
-- failure leaves the lighter extensions installed and inspectable)
CREATE EXTENSION IF NOT EXISTS age;
