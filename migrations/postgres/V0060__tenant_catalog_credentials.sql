-- =====================================================================
-- V0060 — Per-tenant catalog_credentials table (applied to every tenant_<uuid> schema).
--
-- Phase 1 L1 Catalog Gateway (Plan 01-06) reads from this table to discover the
-- upstream Iceberg REST catalog endpoint and adapter-specific config for the
-- requesting tenant. Each row is one configured catalog (e.g., "prod-polaris",
-- "staging-nessie"); `catalog_kind` discriminates the adapter; `config_json`
-- carries the adapter-specific struct (polaris.Config / nessie.Config /
-- glue.Config / unity.Config) marshalled to JSON.
--
-- D-1.01/.02/.03 (PLAN 01-CONTEXT.md): per-catalog Config struct lives in
-- the adapter package; this row is the storage shape.
--
-- GRANT shape: SELECT to tenant role so catalog.Repo works via
-- tenant.WithTenantTx; INSERT/UPDATE/DELETE remain admin-only (catalog
-- onboarding is an admin-pathway operation, not a tenant self-service surface
-- in Phase 1).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS catalog_credentials (
    id               uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    catalog_kind     text          NOT NULL
                                   CHECK (catalog_kind IN ('polaris','nessie','glue','unity')),
    nickname         text          NOT NULL UNIQUE,
    endpoint         text          NOT NULL,
    config_json      jsonb         NOT NULL,
    encrypted_secret bytea,
    created_at       timestamptz   NOT NULL DEFAULT now()
);

-- INSERT/UPDATE/DELETE only admin role; SELECT permitted to tenant role
-- (inherits via Layer 2 schema GRANT — see internal/tenant/provision.go::CreateRole).
GRANT SELECT ON catalog_credentials TO PUBLIC;
REVOKE INSERT, UPDATE, DELETE ON catalog_credentials FROM PUBLIC;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    tbl_ok      boolean;
    chk_ok      boolean;
    schema_name text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = schema_name
          AND tablename  = 'catalog_credentials'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0060 verify: catalog_credentials not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = schema_name
          AND t.relname = 'catalog_credentials'
          AND c.contype = 'c'
    ) INTO chk_ok;

    IF chk_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0060 verify: catalog_kind CHECK constraint missing in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0060 OK — catalog_credentials ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
