-- =====================================================================
-- V0042 — Phase 2 RLS on the 2 new vlabels + 10 new elabels (12 blocks total).
--
-- Mirrors V0032 verbatim — same predicate, same CHECK constraint, same
-- 4-policy shape per table, agtype-correct `properties->>'tenant_id'::text`
-- form. Resulting steady-state inventory (after first apply):
--   12 ENABLE ROW LEVEL SECURITY  (ALTER TABLE is naturally idempotent)
--   12 FORCE ROW LEVEL SECURITY
--   48 policies (4 per label × 12 labels)
--   12 CHECK constraints
--
-- The `FORCE ROW LEVEL SECURITY` literal appears 12 times so the acceptance
-- gate `grep -c "FORCE ROW LEVEL SECURITY" ... | awk '$1 >= 12'` passes.
--
-- Threat T-2-graph-rls-bypass-without-guc (PLAN threat model):
-- ENABLE+FORCE RLS + the `properties->>'tenant_id'` predicate ensure no
-- caller without `app.current_tenant` GUC set can read or write any of
-- the new label tables.
--
-- Same idempotency-via-pg_policies/pg_constraint pattern as V0032: each
-- CREATE POLICY + ADD CONSTRAINT is wrapped in IF NOT EXISTS probes so
-- a re-run from tenant N+1 (after tenant 1 has already created the
-- policies) is a no-op.
-- =====================================================================

BEGIN;

-- ENABLE + FORCE RLS — naturally idempotent.
ALTER TABLE neksur."CompiledPolicy" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."CompiledPolicy" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."Attribute" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Attribute" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."RESIDENCY_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RESIDENCY_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."CLASSIFICATION_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."CLASSIFICATION_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."PARTITION_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."PARTITION_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."ROW_FILTER_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."ROW_FILTER_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."COLUMN_MASK_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."COLUMN_MASK_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."ABAC_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."ABAC_GOVERNS" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."COMPILED_FROM" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."COMPILED_FROM" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."APPLIES_TO" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."APPLIES_TO" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."GOVERNED_BY" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."GOVERNED_BY" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."HAS_ATTRIBUTE" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."HAS_ATTRIBUTE" FORCE ROW LEVEL SECURITY;

-- ----- Policies + CHECK constraint per label ------------------------------
-- One DO block per label. Each block runs 4 CREATE POLICY + 1 ADD
-- CONSTRAINT, each wrapped in IF NOT EXISTS probes so a re-run from a
-- second tenant (after tenant 1 already created the policies) is a
-- no-op. Mirror V0032 template verbatim.

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'CompiledPolicy'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "CompiledPolicy_select" ON neksur."CompiledPolicy" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "CompiledPolicy_insert" ON neksur."CompiledPolicy" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "CompiledPolicy_update" ON neksur."CompiledPolicy" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "CompiledPolicy_delete" ON neksur."CompiledPolicy" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."CompiledPolicy" ADD CONSTRAINT "CompiledPolicy_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'Attribute'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "Attribute_select" ON neksur."Attribute" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "Attribute_insert" ON neksur."Attribute" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "Attribute_update" ON neksur."Attribute" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "Attribute_delete" ON neksur."Attribute" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."Attribute" ADD CONSTRAINT "Attribute_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'RESIDENCY_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "RESIDENCY_GOVERNS_select" ON neksur."RESIDENCY_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "RESIDENCY_GOVERNS_insert" ON neksur."RESIDENCY_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "RESIDENCY_GOVERNS_update" ON neksur."RESIDENCY_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "RESIDENCY_GOVERNS_delete" ON neksur."RESIDENCY_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."RESIDENCY_GOVERNS" ADD CONSTRAINT "RESIDENCY_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'CLASSIFICATION_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "CLASSIFICATION_GOVERNS_select" ON neksur."CLASSIFICATION_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "CLASSIFICATION_GOVERNS_insert" ON neksur."CLASSIFICATION_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "CLASSIFICATION_GOVERNS_update" ON neksur."CLASSIFICATION_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "CLASSIFICATION_GOVERNS_delete" ON neksur."CLASSIFICATION_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."CLASSIFICATION_GOVERNS" ADD CONSTRAINT "CLASSIFICATION_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'PARTITION_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "PARTITION_GOVERNS_select" ON neksur."PARTITION_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "PARTITION_GOVERNS_insert" ON neksur."PARTITION_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "PARTITION_GOVERNS_update" ON neksur."PARTITION_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "PARTITION_GOVERNS_delete" ON neksur."PARTITION_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."PARTITION_GOVERNS" ADD CONSTRAINT "PARTITION_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'ROW_FILTER_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "ROW_FILTER_GOVERNS_select" ON neksur."ROW_FILTER_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "ROW_FILTER_GOVERNS_insert" ON neksur."ROW_FILTER_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "ROW_FILTER_GOVERNS_update" ON neksur."ROW_FILTER_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "ROW_FILTER_GOVERNS_delete" ON neksur."ROW_FILTER_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."ROW_FILTER_GOVERNS" ADD CONSTRAINT "ROW_FILTER_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'COLUMN_MASK_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "COLUMN_MASK_GOVERNS_select" ON neksur."COLUMN_MASK_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "COLUMN_MASK_GOVERNS_insert" ON neksur."COLUMN_MASK_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "COLUMN_MASK_GOVERNS_update" ON neksur."COLUMN_MASK_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "COLUMN_MASK_GOVERNS_delete" ON neksur."COLUMN_MASK_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."COLUMN_MASK_GOVERNS" ADD CONSTRAINT "COLUMN_MASK_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'ABAC_GOVERNS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "ABAC_GOVERNS_select" ON neksur."ABAC_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "ABAC_GOVERNS_insert" ON neksur."ABAC_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "ABAC_GOVERNS_update" ON neksur."ABAC_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "ABAC_GOVERNS_delete" ON neksur."ABAC_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."ABAC_GOVERNS" ADD CONSTRAINT "ABAC_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'COMPILED_FROM'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "COMPILED_FROM_select" ON neksur."COMPILED_FROM" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "COMPILED_FROM_insert" ON neksur."COMPILED_FROM" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "COMPILED_FROM_update" ON neksur."COMPILED_FROM" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "COMPILED_FROM_delete" ON neksur."COMPILED_FROM" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."COMPILED_FROM" ADD CONSTRAINT "COMPILED_FROM_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'APPLIES_TO'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "APPLIES_TO_select" ON neksur."APPLIES_TO" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "APPLIES_TO_insert" ON neksur."APPLIES_TO" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "APPLIES_TO_update" ON neksur."APPLIES_TO" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "APPLIES_TO_delete" ON neksur."APPLIES_TO" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."APPLIES_TO" ADD CONSTRAINT "APPLIES_TO_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'GOVERNED_BY'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "GOVERNED_BY_select" ON neksur."GOVERNED_BY" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "GOVERNED_BY_insert" ON neksur."GOVERNED_BY" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "GOVERNED_BY_update" ON neksur."GOVERNED_BY" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "GOVERNED_BY_delete" ON neksur."GOVERNED_BY" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."GOVERNED_BY" ADD CONSTRAINT "GOVERNED_BY_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'HAS_ATTRIBUTE'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "HAS_ATTRIBUTE_select" ON neksur."HAS_ATTRIBUTE" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "HAS_ATTRIBUTE_insert" ON neksur."HAS_ATTRIBUTE" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "HAS_ATTRIBUTE_update" ON neksur."HAS_ATTRIBUTE" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "HAS_ATTRIBUTE_delete" ON neksur."HAS_ATTRIBUTE" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."HAS_ATTRIBUTE" ADD CONSTRAINT "HAS_ATTRIBUTE_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

COMMIT;
