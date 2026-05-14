-- =====================================================================
-- V0051 — Per-tenant query_history table (applied to every tenant_<uuid> schema).
--
-- Phase 1 telemetry middleware INSERTs one row per gateway-served query
-- recording timing + outcome. Read paths are the Phase 1 admin UI's
-- "slow query log" + per-tenant operator diagnostics.
--
-- ADR-001 §3.4 — keyset pagination on (started_at DESC, id DESC).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS query_history (
    id           bigserial    PRIMARY KEY,
    started_at   timestamptz  NOT NULL DEFAULT now(),
    duration_ms  int          NOT NULL,
    cypher_text  text,
    error_code   text
);

-- Keyset pagination index per ADR-001 §3.4 — same shape as audit_log.
CREATE INDEX IF NOT EXISTS idx_query_history_started_at
    ON query_history (started_at DESC, id DESC);

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    tbl_ok  boolean;
    idx_ok  boolean;
    schema_name text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = schema_name
          AND tablename  = 'query_history'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0051 verify: query_history not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'query_history'
          AND indexname  = 'idx_query_history_started_at'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0051 verify: idx_query_history_started_at not created in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0051 OK — query_history + keyset index ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
