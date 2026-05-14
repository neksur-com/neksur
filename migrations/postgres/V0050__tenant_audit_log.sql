-- =====================================================================
-- V0050 — Per-tenant audit_log table (applied to every tenant_<uuid> schema).
--
-- This is the FIRST Atlas-managed migration that runs against a tenant
-- schema (not against public). The Atlas tenant-loop wrapper composes a
-- DSN of the form `<baseDSN>?search_path=tenant_<uuid>,public` so the
-- unqualified `audit_log` identifier in this file resolves to the tenant
-- schema. The tenant role's INSERT-only GRANT on this table is applied
-- by `internal/tenant/provision.go::CreateRole` AFTER Atlas creates the
-- table — D-0.5.21 T-0.5-audit-tamper. The migration body itself is
-- role-agnostic so it can be re-applied identically across every tenant.
--
-- D-0.5.21 + ADR-001 §3.4 — keyset pagination on (occurred_at DESC, id DESC)
-- is the Phase 1+ admin UI's audit-log paginator contract.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here (Phase 0.5
-- Plan 02 deviation #4 — Atlas + BEGIN inside BEGIN = "unexpected
-- transaction status idle").
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- Re-running on a partially-onboarded tenant is a no-op.
-- =====================================================================

CREATE TABLE IF NOT EXISTS audit_log (
    id              bigserial    PRIMARY KEY,
    occurred_at     timestamptz  NOT NULL DEFAULT now(),
    actor_user_id   text,
    event_type      text         NOT NULL,
    payload         jsonb        NOT NULL DEFAULT '{}'::jsonb
);

-- Keyset pagination index per ADR-001 §3.4 + PATTERNS.md line 814. The
-- admin UI's audit-log paginator scans in this order so a fixed-size
-- keyset query is O(rows-per-page), not O(log N).
CREATE INDEX IF NOT EXISTS idx_audit_log_occurred_at
    ON audit_log (occurred_at DESC, id DESC);

-- ----- Verify block --------------------------------------------------
-- Atlas applies this file against a per-tenant `search_path=tenant_<uuid>,public`
-- so the catalog query has to be schema-qualified by `current_schema()`
-- (the first entry of the search_path is the tenant schema during Atlas
-- apply). PATTERNS.md Group E line 463 — every migration ends with a
-- DO-block verify that fails loudly if the DDL did not land as expected.
DO $$
DECLARE
    tbl_ok  boolean;
    idx_ok  boolean;
    schema_name text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = schema_name
          AND tablename  = 'audit_log'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0050 verify: audit_log not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'audit_log'
          AND indexname  = 'idx_audit_log_occurred_at'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0050 verify: idx_audit_log_occurred_at not created in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0050 OK — audit_log + keyset index ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
