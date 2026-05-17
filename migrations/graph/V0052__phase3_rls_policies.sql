-- =====================================================================
-- V0052 — Phase 3 RLS on the 3 new vlabels + 3 new elabels (6 blocks total).
--
-- Mirrors V0042 verbatim — same predicate, same CHECK constraint, same
-- 4-policy shape per table, agtype-correct `properties->>'tenant_id'::text`
-- form. Resulting steady-state inventory (after first apply):
--   6  ENABLE ROW LEVEL SECURITY  (ALTER TABLE is naturally idempotent)
--   6  FORCE ROW LEVEL SECURITY
--   24 policies (4 per label × 6 labels)
--   6  CHECK constraints
--
-- The `FORCE ROW LEVEL SECURITY` literal appears 6 times so the acceptance
-- gate `grep -c "FORCE ROW LEVEL SECURITY" ... | awk '$1 >= 6'` passes.
--
-- Threat T-3-01-tenant-bleed (PLAN threat model):
-- ENABLE+FORCE RLS + the `properties->>'tenant_id'` predicate ensure no
-- caller without `app.current_tenant` GUC set can read or write any of
-- the new label tables. Mirrors V0042 template exactly.
--
-- Same idempotency-via-pg_policies/pg_constraint pattern as V0042: each
-- CREATE POLICY + ADD CONSTRAINT is wrapped in IF NOT EXISTS probes so
-- a re-run from tenant N+1 (after tenant 1 has already created the
-- policies) is a no-op.
-- =====================================================================

BEGIN;

-- ENABLE + FORCE RLS — naturally idempotent.
ALTER TABLE neksur."SnapshotPin"    ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."SnapshotPin"    FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."PartitionSpec"  ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."PartitionSpec"  FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."DivergenceEvent" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DivergenceEvent" FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."PINS"           ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."PINS"           FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."USES_SPEC"      ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."USES_SPEC"      FORCE ROW LEVEL SECURITY;
ALTER TABLE neksur."DIVERGED_AT"    ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DIVERGED_AT"    FORCE ROW LEVEL SECURITY;

-- ----- Policies + CHECK constraint per label ------------------------------
-- One DO block per label. Each block runs 4 CREATE POLICY + 1 ADD
-- CONSTRAINT, each wrapped in IF NOT EXISTS probes so a re-run from a
-- second tenant (after tenant 1 already created the policies) is a
-- no-op. Mirror V0042 template verbatim.

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'SnapshotPin'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "SnapshotPin_select" ON neksur."SnapshotPin" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "SnapshotPin_insert" ON neksur."SnapshotPin" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "SnapshotPin_update" ON neksur."SnapshotPin" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "SnapshotPin_delete" ON neksur."SnapshotPin" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."SnapshotPin" ADD CONSTRAINT "SnapshotPin_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'PartitionSpec'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "PartitionSpec_select" ON neksur."PartitionSpec" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "PartitionSpec_insert" ON neksur."PartitionSpec" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "PartitionSpec_update" ON neksur."PartitionSpec" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "PartitionSpec_delete" ON neksur."PartitionSpec" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."PartitionSpec" ADD CONSTRAINT "PartitionSpec_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'DivergenceEvent'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "DivergenceEvent_select" ON neksur."DivergenceEvent" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "DivergenceEvent_insert" ON neksur."DivergenceEvent" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "DivergenceEvent_update" ON neksur."DivergenceEvent" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "DivergenceEvent_delete" ON neksur."DivergenceEvent" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."DivergenceEvent" ADD CONSTRAINT "DivergenceEvent_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'PINS'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "PINS_select" ON neksur."PINS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "PINS_insert" ON neksur."PINS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "PINS_update" ON neksur."PINS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "PINS_delete" ON neksur."PINS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."PINS" ADD CONSTRAINT "PINS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'USES_SPEC'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "USES_SPEC_select" ON neksur."USES_SPEC" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "USES_SPEC_insert" ON neksur."USES_SPEC" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "USES_SPEC_update" ON neksur."USES_SPEC" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "USES_SPEC_delete" ON neksur."USES_SPEC" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."USES_SPEC" ADD CONSTRAINT "USES_SPEC_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

DO $$ DECLARE schema_name CONSTANT text := 'neksur'; tbl CONSTANT text := 'DIVERGED_AT'; BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_select') THEN
        CREATE POLICY "DIVERGED_AT_select" ON neksur."DIVERGED_AT" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_insert') THEN
        CREATE POLICY "DIVERGED_AT_insert" ON neksur."DIVERGED_AT" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_update') THEN
        CREATE POLICY "DIVERGED_AT_update" ON neksur."DIVERGED_AT" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = schema_name AND tablename = tbl AND policyname = tbl || '_delete') THEN
        CREATE POLICY "DIVERGED_AT_delete" ON neksur."DIVERGED_AT" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = schema_name AND t.relname = tbl AND c.conname = tbl || '_tenant_id_required') THEN
        ALTER TABLE neksur."DIVERGED_AT" ADD CONSTRAINT "DIVERGED_AT_tenant_id_required" CHECK (properties ? 'tenant_id'::text);
    END IF;
END $$;

COMMIT;
