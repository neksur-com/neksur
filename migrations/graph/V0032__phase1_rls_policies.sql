-- =====================================================================
-- V0032 — Phase 1 RLS on the 4 new vlabels + 5 new elabels (9 blocks total).
--
-- Mirrors migrations/postgres/V0030__rls_policies.sql lines 49-200 for the
-- 19 + 24 Phase 0 labels — same predicate, same CHECK constraint, same
-- 4-policy shape per table. The agtype-correct form `(properties->>'tenant_id'::text)`
-- is required (the ::text cast resolves to AGE's `agtype ->> text`
-- operator; without it the literal is ambiguous and parsing fails).
--
-- Per-table contract (9 × the same shape, identical to V0030 postgres):
--   1. ENABLE ROW LEVEL SECURITY
--   2. FORCE ROW LEVEL SECURITY (T-0-RLS-FORCE-BYPASS — even owner respects RLS)
--   3. CREATE POLICY <Label>_select FOR SELECT USING tenant match
--   4. CREATE POLICY <Label>_insert FOR INSERT WITH CHECK tenant match
--   5. CREATE POLICY <Label>_update FOR UPDATE USING + WITH CHECK
--   6. CREATE POLICY <Label>_delete FOR DELETE USING tenant match
--   7. ADD CONSTRAINT <Label>_tenant_id_required CHECK (properties ? 'tenant_id'::text)
--
-- Resulting exact counts:
--    9 ENABLE ROW LEVEL SECURITY
--    9 FORCE ROW LEVEL SECURITY
--   36 current_setting('app.current_tenant' references (4 policies × 9)
--    9 CHECK constraints
--
-- Idempotency contract: applied via internal/migrate.ApplyTenantGraph,
-- which records V0032 in <schema>.graph_schema_revisions on success and
-- skips re-application on subsequent runs (the same shape that
-- migrations/postgres/V0030 relies on Atlas's revision tracker for).
-- =====================================================================

BEGIN;

-- ----- 1/9  RetentionPolicy ------------------------------------------------
ALTER TABLE neksur."RetentionPolicy" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RetentionPolicy" FORCE ROW LEVEL SECURITY;
CREATE POLICY "RetentionPolicy_select" ON neksur."RetentionPolicy" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "RetentionPolicy_insert" ON neksur."RetentionPolicy" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "RetentionPolicy_update" ON neksur."RetentionPolicy" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "RetentionPolicy_delete" ON neksur."RetentionPolicy" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."RetentionPolicy" ADD CONSTRAINT "RetentionPolicy_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 2/9  LifecyclePolicy ------------------------------------------------
ALTER TABLE neksur."LifecyclePolicy" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."LifecyclePolicy" FORCE ROW LEVEL SECURITY;
CREATE POLICY "LifecyclePolicy_select" ON neksur."LifecyclePolicy" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "LifecyclePolicy_insert" ON neksur."LifecyclePolicy" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "LifecyclePolicy_update" ON neksur."LifecyclePolicy" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "LifecyclePolicy_delete" ON neksur."LifecyclePolicy" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."LifecyclePolicy" ADD CONSTRAINT "LifecyclePolicy_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 3/9  ScheduledAction ------------------------------------------------
ALTER TABLE neksur."ScheduledAction" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."ScheduledAction" FORCE ROW LEVEL SECURITY;
CREATE POLICY "ScheduledAction_select" ON neksur."ScheduledAction" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "ScheduledAction_insert" ON neksur."ScheduledAction" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "ScheduledAction_update" ON neksur."ScheduledAction" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "ScheduledAction_delete" ON neksur."ScheduledAction" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."ScheduledAction" ADD CONSTRAINT "ScheduledAction_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 4/9  Classification -------------------------------------------------
ALTER TABLE neksur."Classification" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Classification" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Classification_select" ON neksur."Classification" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "Classification_insert" ON neksur."Classification" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "Classification_update" ON neksur."Classification" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "Classification_delete" ON neksur."Classification" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Classification" ADD CONSTRAINT "Classification_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 5/9  HAS_COLUMN -----------------------------------------------------
ALTER TABLE neksur."HAS_COLUMN" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."HAS_COLUMN" FORCE ROW LEVEL SECURITY;
CREATE POLICY "HAS_COLUMN_select" ON neksur."HAS_COLUMN" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "HAS_COLUMN_insert" ON neksur."HAS_COLUMN" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "HAS_COLUMN_update" ON neksur."HAS_COLUMN" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "HAS_COLUMN_delete" ON neksur."HAS_COLUMN" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."HAS_COLUMN" ADD CONSTRAINT "HAS_COLUMN_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 6/9  SCHEMA_GOVERNS -------------------------------------------------
ALTER TABLE neksur."SCHEMA_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."SCHEMA_GOVERNS" FORCE ROW LEVEL SECURITY;
CREATE POLICY "SCHEMA_GOVERNS_select" ON neksur."SCHEMA_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "SCHEMA_GOVERNS_insert" ON neksur."SCHEMA_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "SCHEMA_GOVERNS_update" ON neksur."SCHEMA_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "SCHEMA_GOVERNS_delete" ON neksur."SCHEMA_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."SCHEMA_GOVERNS" ADD CONSTRAINT "SCHEMA_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 7/9  WRITE_GOVERNS --------------------------------------------------
ALTER TABLE neksur."WRITE_GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."WRITE_GOVERNS" FORCE ROW LEVEL SECURITY;
CREATE POLICY "WRITE_GOVERNS_select" ON neksur."WRITE_GOVERNS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "WRITE_GOVERNS_insert" ON neksur."WRITE_GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "WRITE_GOVERNS_update" ON neksur."WRITE_GOVERNS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "WRITE_GOVERNS_delete" ON neksur."WRITE_GOVERNS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."WRITE_GOVERNS" ADD CONSTRAINT "WRITE_GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 8/9  RETAINS --------------------------------------------------------
ALTER TABLE neksur."RETAINS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RETAINS" FORCE ROW LEVEL SECURITY;
CREATE POLICY "RETAINS_select" ON neksur."RETAINS" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "RETAINS_insert" ON neksur."RETAINS" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "RETAINS_update" ON neksur."RETAINS" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "RETAINS_delete" ON neksur."RETAINS" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."RETAINS" ADD CONSTRAINT "RETAINS_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

-- ----- 9/9  DETECTED_BY ----------------------------------------------------
ALTER TABLE neksur."DETECTED_BY" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DETECTED_BY" FORCE ROW LEVEL SECURITY;
CREATE POLICY "DETECTED_BY_select" ON neksur."DETECTED_BY" FOR SELECT USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "DETECTED_BY_insert" ON neksur."DETECTED_BY" FOR INSERT WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "DETECTED_BY_update" ON neksur."DETECTED_BY" FOR UPDATE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
CREATE POLICY "DETECTED_BY_delete" ON neksur."DETECTED_BY" FOR DELETE USING ((properties->>'tenant_id'::text) = current_setting('app.current_tenant', true));
ALTER TABLE neksur."DETECTED_BY" ADD CONSTRAINT "DETECTED_BY_tenant_id_required" CHECK (properties ? 'tenant_id'::text);

COMMIT;
