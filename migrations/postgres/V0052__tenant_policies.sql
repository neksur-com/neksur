-- =====================================================================
-- V0052 — Per-tenant policies table (applied to every tenant_<uuid> schema).
--
-- Phase 1 policy enforcement consumes this table. Each row is one
-- policy definition (row filter / column mask / RBAC / ABAC stub).
--
-- The `kind` CHECK constraint locks the policy taxonomy at Phase 0.5
-- so Phase 1 code can switch on a known enum-equivalent. ABAC is in
-- the constraint but FF-gated in code per ADR-004 §4 (RBAC only in
-- Phase 0; ABAC is a Phase 1+ feature).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS policies (
    id          uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text          NOT NULL UNIQUE,
    kind        text          NOT NULL
                              CHECK (kind IN ('row_filter','column_mask','rbac','abac')),
    body        jsonb         NOT NULL,
    created_at  timestamptz   NOT NULL DEFAULT now(),
    updated_at  timestamptz   NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE INDEX IF NOT EXISTS idx_policies_kind
    ON policies (kind);

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
          AND tablename  = 'policies'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0052 verify: policies not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'policies'
          AND indexname  = 'idx_policies_kind'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0052 verify: idx_policies_kind not created in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0052 OK — policies + kind index ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
