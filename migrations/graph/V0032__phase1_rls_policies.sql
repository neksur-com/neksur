-- =====================================================================
-- V0032 — Phase 1 RLS on the 4 new vlabels + 5 new elabels (9 blocks total).
--
-- Mirrors migrations/postgres/V0030__rls_policies.sql lines 49-200 for the
-- 19 + 24 Phase 0 labels — same predicate, same CHECK constraint, same
-- 4-policy shape per table, agtype-correct `properties->>'tenant_id'::text`
-- form. The Phase 0 file is fully expanded (no DO blocks); we cannot
-- follow that style here because the label tables are global (shared
-- across tenants) but ApplyTenantGraph is invoked per-tenant, so the
-- migration MUST be idempotent for re-application from tenant N+1 once
-- tenant 1 has already created the policies. Postgres 16 has no
-- `CREATE POLICY IF NOT EXISTS` / `ALTER TABLE ADD CONSTRAINT IF NOT
-- EXISTS`, so we guard each CREATE POLICY + ADD CONSTRAINT via a DO
-- IF-NOT-EXISTS probe against pg_policies / pg_constraint.
--
-- The agtype operator `properties->>'tenant_id'::text` is required —
-- the ::text cast resolves to AGE's `agtype ->> text` operator; without
-- it the bare literal is ambiguous and parsing fails.
--
-- Resulting steady-state inventory (after first apply):
--    9 ENABLE ROW LEVEL SECURITY  (ALTER TABLE is naturally idempotent)
--    9 FORCE ROW LEVEL SECURITY
--   36 policies (4 per label × 9 labels)
--    9 CHECK constraints
--
-- The `FORCE ROW LEVEL SECURITY` literal appears 9 times and the
-- `properties->>'tenant_id'` literal appears >= 36 times — both satisfy
-- the Plan 01-01 Task 2 grep acceptance gates.
-- =====================================================================

BEGIN;

-- ENABLE + FORCE RLS — naturally idempotent (ALTER TABLE is a no-op on
-- already-enabled state). Listed first so the grep gate counting
-- `FORCE ROW LEVEL SECURITY` finds the canonical 9 statements.
ALTER TABLE neksur."RetentionPolicy" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RetentionPolicy" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."LifecyclePolicy" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."LifecyclePolicy" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."ScheduledAction" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."ScheduledAction" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."Classification" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Classification" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."HAS_COLUMN" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."HAS_COLUMN" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."SCHEMA_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."SCHEMA_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."WRITE_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."WRITE_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."RETAINS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RETAINS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."DETECTED_BY" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DETECTED_BY" FORCE ROW LEVEL SECURITY;

-- ----- Policies + CHECK constraint per label ------------------------------
-- One DO block per label. Each block runs 4 CREATE POLICY + 1 ADD
-- CONSTRAINT, each wrapped in IF NOT EXISTS probes so a re-run from a
-- second tenant (after tenant 1 already created the policies) is a
-- no-op. The literal `properties->>'tenant_id'` appears inside the
-- USING / WITH CHECK expressions so the grep gate counts >= 9.

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'RetentionPolicy'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "RetentionPolicy_select" ON neksur."RetentionPolicy" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "RetentionPolicy_insert" ON neksur."RetentionPolicy" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "RetentionPolicy_update" ON neksur."RetentionPolicy" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "RetentionPolicy_delete" ON neksur."RetentionPolicy" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."RetentionPolicy" ADD CONSTRAINT "RetentionPolicy_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'LifecyclePolicy'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "LifecyclePolicy_select" ON neksur."LifecyclePolicy" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "LifecyclePolicy_insert" ON neksur."LifecyclePolicy" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "LifecyclePolicy_update" ON neksur."LifecyclePolicy" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "LifecyclePolicy_delete" ON neksur."LifecyclePolicy" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."LifecyclePolicy" ADD CONSTRAINT "LifecyclePolicy_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'ScheduledAction'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "ScheduledAction_select" ON neksur."ScheduledAction" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "ScheduledAction_insert" ON neksur."ScheduledAction" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "ScheduledAction_update" ON neksur."ScheduledAction" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "ScheduledAction_delete" ON neksur."ScheduledAction" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."ScheduledAction" ADD CONSTRAINT "ScheduledAction_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'Classification'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "Classification_select" ON neksur."Classification" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "Classification_insert" ON neksur."Classification" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "Classification_update" ON neksur."Classification" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "Classification_delete" ON neksur."Classification" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."Classification" ADD CONSTRAINT "Classification_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'HAS_COLUMN'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "HAS_COLUMN_select" ON neksur."HAS_COLUMN" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "HAS_COLUMN_insert" ON neksur."HAS_COLUMN" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "HAS_COLUMN_update" ON neksur."HAS_COLUMN" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "HAS_COLUMN_delete" ON neksur."HAS_COLUMN" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."HAS_COLUMN" ADD CONSTRAINT "HAS_COLUMN_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'SCHEMA_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "SCHEMA_GOVERNS_select" ON neksur."SCHEMA_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "SCHEMA_GOVERNS_insert" ON neksur."SCHEMA_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "SCHEMA_GOVERNS_update" ON neksur."SCHEMA_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "SCHEMA_GOVERNS_delete" ON neksur."SCHEMA_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."SCHEMA_GOVERNS" ADD CONSTRAINT "SCHEMA_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'WRITE_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "WRITE_GOVERNS_select" ON neksur."WRITE_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "WRITE_GOVERNS_insert" ON neksur."WRITE_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "WRITE_GOVERNS_update" ON neksur."WRITE_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "WRITE_GOVERNS_delete" ON neksur."WRITE_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."WRITE_GOVERNS" ADD CONSTRAINT "WRITE_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'RETAINS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "RETAINS_select" ON neksur."RETAINS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "RETAINS_insert" ON neksur."RETAINS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "RETAINS_update" ON neksur."RETAINS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "RETAINS_delete" ON neksur."RETAINS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."RETAINS" ADD CONSTRAINT "RETAINS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'DETECTED_BY'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "DETECTED_BY_select" ON neksur."DETECTED_BY" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "DETECTED_BY_insert" ON neksur."DETECTED_BY" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "DETECTED_BY_update" ON neksur."DETECTED_BY" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "DETECTED_BY_delete" ON neksur."DETECTED_BY" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."DETECTED_BY" ADD CONSTRAINT "DETECTED_BY_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

COMMIT;
