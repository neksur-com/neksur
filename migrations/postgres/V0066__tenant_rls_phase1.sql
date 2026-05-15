-- =====================================================================
-- V0066 — Per-tenant RLS for Phase 1 customer-facing tables.
--
-- Three of the V0060-V0064 tables expose tenant-scoped data through the
-- admin UI and operator paths and therefore get FORCE RLS as defence-in-depth
-- against the Phase 0.5 T-0.5-rls-bypass-missing-guc failure mode (a query
-- run without `app.current_tenant` GUC set must return 0 rows, not all rows):
--
--   - catalog_credentials  (V0060) — admin/UI list, tenant role SELECT
--   - detection_runs        (V0062) — admin UI paginator + alert UI
--   - lineage_inbox         (V0063) — observability / debug surface for the
--                                     OpenLineage consumer
--
-- The per-tenant search_path + schema-level GRANT scope (Layers 1+2 from
-- Phase 0.5 Plan 04) already isolate every read; RLS predicate is layer 3
-- defence-in-depth. Predicate is "GUC present" rather than "GUC matches a
-- column" because per-tenant schemas already segregate rows; RLS only needs
-- to ensure no caller reads without an explicit tenant context.
--
-- Skipped (intentional, see PLAN.md threat model):
--   - policy_cache       (V0061) — application-internal cache, no
--                                  cross-tenant exposure surface
--   - staging.iceberg_*  (V0064) — internal COPY buffers, application-only
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: ENABLE ROW LEVEL SECURITY + FORCE ROW LEVEL SECURITY are
-- no-ops on a table that already has them; CREATE POLICY uses IF NOT EXISTS
-- via the DO-block guard below (CREATE POLICY itself lacks IF NOT EXISTS in
-- Postgres 16, so we wrap in pg_policies-existence checks).
-- =====================================================================

ALTER TABLE catalog_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE catalog_credentials FORCE ROW LEVEL SECURITY;

ALTER TABLE detection_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE detection_runs FORCE ROW LEVEL SECURITY;

ALTER TABLE lineage_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_inbox FORCE ROW LEVEL SECURITY;

DO $$
DECLARE
    schema_name text := current_schema();
    tbl text;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['catalog_credentials','detection_runs','lineage_inbox'] LOOP
        -- SELECT policy
        IF NOT EXISTS (
            SELECT 1 FROM pg_policies
            WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select'
        ) THEN
            EXECUTE format(
                'CREATE POLICY %I ON %I FOR SELECT USING (current_setting(''app.current_tenant'', true) IS NOT NULL)',
                tbl || '_select', tbl
            );
        END IF;

        -- INSERT policy
        IF NOT EXISTS (
            SELECT 1 FROM pg_policies
            WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_modify'
        ) THEN
            EXECUTE format(
                'CREATE POLICY %I ON %I FOR INSERT WITH CHECK (current_setting(''app.current_tenant'', true) IS NOT NULL)',
                tbl || '_modify', tbl
            );
        END IF;
    END LOOP;
END
$$ LANGUAGE plpgsql;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    forced_count int;
    policy_count int;
    schema_name text := current_schema();
BEGIN
    SELECT count(*)::int INTO forced_count
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = schema_name
      AND c.relname IN ('catalog_credentials','detection_runs','lineage_inbox')
      AND c.relrowsecurity = true
      AND c.relforcerowsecurity = true;

    IF forced_count <> 3 THEN
        RAISE EXCEPTION 'V0066 verify: expected 3 FORCE-RLS tables, found % (schema %)', forced_count, schema_name;
    END IF;

    SELECT count(*)::int INTO policy_count
    FROM pg_policies
    WHERE schemaname = schema_name
      AND tablename  IN ('catalog_credentials','detection_runs','lineage_inbox');

    IF policy_count < 6 THEN
        RAISE EXCEPTION 'V0066 verify: expected >=6 policies (2 per table x 3 tables), found % (schema %)', policy_count, schema_name;
    END IF;

    RAISE NOTICE 'V0066 OK — FORCE RLS + select/modify policies attached to 3 tables in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
