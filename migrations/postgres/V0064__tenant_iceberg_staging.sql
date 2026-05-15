-- =====================================================================
-- V0064 — Per-tenant staging.iceberg_* tables (applied to every tenant_<uuid> schema).
--
-- COPY-target tables for the Plan 01-08 bulk backfill pathway. Spark / the
-- bulk ingest CLI streams Iceberg metadata as TSV/CSV into these tables via
-- pgx.Conn.CopyFrom; Plan 01-08 then transforms the staged rows into Cypher
-- MERGE batches and TRUNCATEs the staging tables.
--
-- The schema is `tenant_<uuid>.staging` — NOT a top-level "staging" namespace.
-- Atlas's tenant-mode runs with `search_path = tenant_<uuid>, public`, so the
-- bare `staging` identifier resolves to `tenant_<uuid>.staging`. This keeps
-- bulk staging fully tenant-isolated (no cross-tenant data crosses the COPY).
--
-- No indexes — these are short-lived load buffers; Plan 01-08 transforms
-- then truncates. RLS skipped (V0066 leaves staging.* alone) because the
-- schema-level GRANT to the tenant role + per-tenant search_path is the
-- isolation contract for application-internal tables (T-1-staging-iceberg-rls-skip
-- in PLAN.md threat model — accept).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE SCHEMA / CREATE TABLE IF NOT EXISTS.
-- =====================================================================

CREATE SCHEMA IF NOT EXISTS staging;

CREATE TABLE IF NOT EXISTS staging.iceberg_tables (
    uuid                 text,
    namespace            text,
    name                 text,
    current_snapshot_id  bigint,
    metadata_location    text,
    properties           jsonb
);

CREATE TABLE IF NOT EXISTS staging.iceberg_columns (
    table_uuid                  text,
    snapshot_metadata_location  text,
    column_id                   int,
    name                        text,
    data_type                   text,
    required                    boolean,
    doc                         text,
    ordinal                     int
);

CREATE TABLE IF NOT EXISTS staging.iceberg_snapshots (
    snapshot_id         bigint,
    parent_snapshot_id  bigint,
    metadata_location   text,
    committed_at_ms     bigint,
    operation           text,
    summary             jsonb
);

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    schema_ok   boolean;
    tables_ok   int;
    schema_name text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_namespace
        WHERE nspname = 'staging'
    ) INTO schema_ok;

    IF schema_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0064 verify: staging schema not created (current_schema=%)', schema_name;
    END IF;

    SELECT count(*)::int INTO tables_ok
    FROM pg_tables
    WHERE schemaname = 'staging'
      AND tablename IN ('iceberg_tables','iceberg_columns','iceberg_snapshots');

    IF tables_ok <> 3 THEN
        RAISE EXCEPTION 'V0064 verify: staging.iceberg_* expected 3 tables, found % (current_schema=%)', tables_ok, schema_name;
    END IF;

    RAISE NOTICE 'V0064 OK — staging.iceberg_{tables,columns,snapshots} ready (parent schema=%).', schema_name;
END
$$ LANGUAGE plpgsql;
