-- =====================================================================
-- V0061 — Per-tenant policy_cache table (applied to every tenant_<uuid> schema).
--
-- Phase 1 CEL policy engine (Plan 01-05) caches compiled-AST hashes + raw
-- policy text by graph node id so repeated commit-path evaluations can skip
-- the cel-go Compile() step. `last_used_at` drives an LRU eviction sweep
-- (the worker is added in Plan 01-05; this migration only lands the storage).
--
-- Cache table is application-internal — no RLS attachment (V0066 leaves this
-- table alone). Schema-level GRANT to the tenant role (Layer 2) restricts
-- access; missing-GUC failure mode is irrelevant because tenant role can
-- only see its own schema regardless of `app.current_tenant`.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS policy_cache (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_node_id  text         NOT NULL UNIQUE,
    definition_cel  text         NOT NULL,
    ast_hash        bytea        NOT NULL,
    cached_at       timestamptz  NOT NULL DEFAULT now(),
    last_used_at    timestamptz
);

-- LRU eviction sweep scans in this order — newest-touched last, NULLs first
-- (rows never used yet). Plan 01-05 LRU worker reads via this index.
CREATE INDEX IF NOT EXISTS idx_policy_cache_last_used_at
    ON policy_cache (last_used_at DESC NULLS LAST);

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    tbl_ok      boolean;
    idx_ok      boolean;
    schema_name text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = schema_name
          AND tablename  = 'policy_cache'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0061 verify: policy_cache not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'policy_cache'
          AND indexname  = 'idx_policy_cache_last_used_at'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0061 verify: idx_policy_cache_last_used_at not created in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0061 OK — policy_cache + LRU index ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
