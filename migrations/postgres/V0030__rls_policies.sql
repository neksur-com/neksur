-- =====================================================================
-- V0030 — Row-Level Security on all 43 AGE label tables.
--
-- Coverage per ADR-001 §9.4 + D-001.04 + D-001.05/.06 amended by ADR-003
-- D-003.06: 19 vlabels + 24 elabels = 43 label tables. EVERY label table —
-- not just vertex tables — gets RLS, because edge MERGEs that cross
-- tenant boundaries must also be blocked.
--
-- Per-table contract (43 × the same shape):
--   1.  ENABLE ROW LEVEL SECURITY
--   2.  FORCE ROW LEVEL SECURITY (covers T-0-RLS-FORCE-BYPASS: even the
--                                  table-owning role must respect RLS)
--   3.  CREATE POLICY <Label>_select  FOR SELECT
--                     USING tenant match
--   4.  CREATE POLICY <Label>_insert  FOR INSERT
--                     WITH CHECK tenant match
--   5.  CREATE POLICY <Label>_update  FOR UPDATE
--                     USING tenant match WITH CHECK tenant match
--   6.  CREATE POLICY <Label>_delete  FOR DELETE
--                     USING tenant match
--   7.  ADD CONSTRAINT  <Label>_tenant_id_required
--                     CHECK (properties ? 'tenant_id')   (Pitfall 4)
--
-- Tenant match expression (constant across all 43 tables, all 4 policies):
--     (properties->>'tenant_id') = current_setting('app.current_tenant', true)
--
-- Resulting exact counts (PLAN <verify> regex is anchored; do not relax):
--   43 ENABLE ROW LEVEL SECURITY
--   43 FORCE ROW LEVEL SECURITY
--  172 current_setting('app.current_tenant' references (4 policies × 43)
--   43 CHECK (properties ? 'tenant_id') constraints
--
-- Order matches V0010 — 19 vlabels first (D-001.05 + D-003.06 additions),
-- then 24 elabels (15 mandatory + 3 D-003.06 + 6 supplement).
--
-- IMPLEMENTATION NOTE: generated as a fully-expanded SQL file (no DO-loop)
-- so the PLAN's exact-count grep verifies the text occurrences directly
-- and there is no risk of runtime/text-count drift. The 43 blocks below
-- are mechanically identical except for the label identifier.
-- =====================================================================

BEGIN;

-- ----- 1/43  Table ------------------------------------------------
ALTER TABLE neksur."Table" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Table" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Table_select" ON neksur."Table" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Table_insert" ON neksur."Table" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Table_update" ON neksur."Table" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Table_delete" ON neksur."Table" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Table" ADD CONSTRAINT "Table_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 2/43  Column ------------------------------------------------
ALTER TABLE neksur."Column" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Column" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Column_select" ON neksur."Column" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Column_insert" ON neksur."Column" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Column_update" ON neksur."Column" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Column_delete" ON neksur."Column" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Column" ADD CONSTRAINT "Column_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 3/43  Snapshot ------------------------------------------------
ALTER TABLE neksur."Snapshot" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Snapshot" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Snapshot_select" ON neksur."Snapshot" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Snapshot_insert" ON neksur."Snapshot" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Snapshot_update" ON neksur."Snapshot" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Snapshot_delete" ON neksur."Snapshot" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Snapshot" ADD CONSTRAINT "Snapshot_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 4/43  Metric ------------------------------------------------
ALTER TABLE neksur."Metric" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Metric" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Metric_select" ON neksur."Metric" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Metric_insert" ON neksur."Metric" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Metric_update" ON neksur."Metric" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Metric_delete" ON neksur."Metric" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Metric" ADD CONSTRAINT "Metric_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 5/43  Dimension ------------------------------------------------
ALTER TABLE neksur."Dimension" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Dimension" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Dimension_select" ON neksur."Dimension" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Dimension_insert" ON neksur."Dimension" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Dimension_update" ON neksur."Dimension" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Dimension_delete" ON neksur."Dimension" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Dimension" ADD CONSTRAINT "Dimension_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 6/43  View ------------------------------------------------
ALTER TABLE neksur."View" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."View" FORCE ROW LEVEL SECURITY;
CREATE POLICY "View_select" ON neksur."View" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "View_insert" ON neksur."View" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "View_update" ON neksur."View" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "View_delete" ON neksur."View" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."View" ADD CONSTRAINT "View_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 7/43  Dashboard ------------------------------------------------
ALTER TABLE neksur."Dashboard" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Dashboard" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Dashboard_select" ON neksur."Dashboard" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Dashboard_insert" ON neksur."Dashboard" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Dashboard_update" ON neksur."Dashboard" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Dashboard_delete" ON neksur."Dashboard" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Dashboard" ADD CONSTRAINT "Dashboard_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 8/43  Pipeline ------------------------------------------------
ALTER TABLE neksur."Pipeline" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Pipeline" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Pipeline_select" ON neksur."Pipeline" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Pipeline_insert" ON neksur."Pipeline" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Pipeline_update" ON neksur."Pipeline" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Pipeline_delete" ON neksur."Pipeline" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Pipeline" ADD CONSTRAINT "Pipeline_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 9/43  Query ------------------------------------------------
ALTER TABLE neksur."Query" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Query" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Query_select" ON neksur."Query" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Query_insert" ON neksur."Query" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Query_update" ON neksur."Query" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Query_delete" ON neksur."Query" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Query" ADD CONSTRAINT "Query_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 10/43  Person ------------------------------------------------
ALTER TABLE neksur."Person" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Person" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Person_select" ON neksur."Person" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Person_insert" ON neksur."Person" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Person_update" ON neksur."Person" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Person_delete" ON neksur."Person" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Person" ADD CONSTRAINT "Person_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 11/43  Team ------------------------------------------------
ALTER TABLE neksur."Team" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Team" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Team_select" ON neksur."Team" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Team_insert" ON neksur."Team" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Team_update" ON neksur."Team" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Team_delete" ON neksur."Team" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Team" ADD CONSTRAINT "Team_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 12/43  Policy ------------------------------------------------
ALTER TABLE neksur."Policy" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Policy" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Policy_select" ON neksur."Policy" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Policy_insert" ON neksur."Policy" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Policy_update" ON neksur."Policy" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Policy_delete" ON neksur."Policy" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Policy" ADD CONSTRAINT "Policy_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 13/43  GlossaryTerm ------------------------------------------------
ALTER TABLE neksur."GlossaryTerm" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."GlossaryTerm" FORCE ROW LEVEL SECURITY;
CREATE POLICY "GlossaryTerm_select" ON neksur."GlossaryTerm" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GlossaryTerm_insert" ON neksur."GlossaryTerm" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GlossaryTerm_update" ON neksur."GlossaryTerm" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GlossaryTerm_delete" ON neksur."GlossaryTerm" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."GlossaryTerm" ADD CONSTRAINT "GlossaryTerm_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 14/43  Tag ------------------------------------------------
ALTER TABLE neksur."Tag" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Tag" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Tag_select" ON neksur."Tag" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Tag_insert" ON neksur."Tag" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Tag_update" ON neksur."Tag" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Tag_delete" ON neksur."Tag" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Tag" ADD CONSTRAINT "Tag_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 15/43  DataContract ------------------------------------------------
ALTER TABLE neksur."DataContract" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DataContract" FORCE ROW LEVEL SECURITY;
CREATE POLICY "DataContract_select" ON neksur."DataContract" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DataContract_insert" ON neksur."DataContract" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DataContract_update" ON neksur."DataContract" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DataContract_delete" ON neksur."DataContract" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."DataContract" ADD CONSTRAINT "DataContract_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 16/43  Engine ------------------------------------------------
ALTER TABLE neksur."Engine" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Engine" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Engine_select" ON neksur."Engine" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Engine_insert" ON neksur."Engine" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Engine_update" ON neksur."Engine" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Engine_delete" ON neksur."Engine" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Engine" ADD CONSTRAINT "Engine_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 17/43  Catalog ------------------------------------------------
ALTER TABLE neksur."Catalog" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."Catalog" FORCE ROW LEVEL SECURITY;
CREATE POLICY "Catalog_select" ON neksur."Catalog" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Catalog_insert" ON neksur."Catalog" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Catalog_update" ON neksur."Catalog" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "Catalog_delete" ON neksur."Catalog" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."Catalog" ADD CONSTRAINT "Catalog_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 18/43  WriteEvent ------------------------------------------------
ALTER TABLE neksur."WriteEvent" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."WriteEvent" FORCE ROW LEVEL SECURITY;
CREATE POLICY "WriteEvent_select" ON neksur."WriteEvent" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "WriteEvent_insert" ON neksur."WriteEvent" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "WriteEvent_update" ON neksur."WriteEvent" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "WriteEvent_delete" ON neksur."WriteEvent" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."WriteEvent" ADD CONSTRAINT "WriteEvent_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 19/43  DetectionRun ------------------------------------------------
ALTER TABLE neksur."DetectionRun" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DetectionRun" FORCE ROW LEVEL SECURITY;
CREATE POLICY "DetectionRun_select" ON neksur."DetectionRun" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DetectionRun_insert" ON neksur."DetectionRun" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DetectionRun_update" ON neksur."DetectionRun" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DetectionRun_delete" ON neksur."DetectionRun" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."DetectionRun" ADD CONSTRAINT "DetectionRun_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 20/43  LINEAGE_OF ------------------------------------------------
ALTER TABLE neksur."LINEAGE_OF" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."LINEAGE_OF" FORCE ROW LEVEL SECURITY;
CREATE POLICY "LINEAGE_OF_select" ON neksur."LINEAGE_OF" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "LINEAGE_OF_insert" ON neksur."LINEAGE_OF" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "LINEAGE_OF_update" ON neksur."LINEAGE_OF" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "LINEAGE_OF_delete" ON neksur."LINEAGE_OF" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."LINEAGE_OF" ADD CONSTRAINT "LINEAGE_OF_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 21/43  OWNS ------------------------------------------------
ALTER TABLE neksur."OWNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."OWNS" FORCE ROW LEVEL SECURITY;
CREATE POLICY "OWNS_select" ON neksur."OWNS" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "OWNS_insert" ON neksur."OWNS" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "OWNS_update" ON neksur."OWNS" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "OWNS_delete" ON neksur."OWNS" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."OWNS" ADD CONSTRAINT "OWNS_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 22/43  MEMBER_OF ------------------------------------------------
ALTER TABLE neksur."MEMBER_OF" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."MEMBER_OF" FORCE ROW LEVEL SECURITY;
CREATE POLICY "MEMBER_OF_select" ON neksur."MEMBER_OF" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "MEMBER_OF_insert" ON neksur."MEMBER_OF" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "MEMBER_OF_update" ON neksur."MEMBER_OF" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "MEMBER_OF_delete" ON neksur."MEMBER_OF" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."MEMBER_OF" ADD CONSTRAINT "MEMBER_OF_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 23/43  DEPENDS_ON ------------------------------------------------
ALTER TABLE neksur."DEPENDS_ON" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DEPENDS_ON" FORCE ROW LEVEL SECURITY;
CREATE POLICY "DEPENDS_ON_select" ON neksur."DEPENDS_ON" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DEPENDS_ON_insert" ON neksur."DEPENDS_ON" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DEPENDS_ON_update" ON neksur."DEPENDS_ON" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DEPENDS_ON_delete" ON neksur."DEPENDS_ON" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."DEPENDS_ON" ADD CONSTRAINT "DEPENDS_ON_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 24/43  CLASSIFIED_AS ------------------------------------------------
ALTER TABLE neksur."CLASSIFIED_AS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."CLASSIFIED_AS" FORCE ROW LEVEL SECURITY;
CREATE POLICY "CLASSIFIED_AS_select" ON neksur."CLASSIFIED_AS" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "CLASSIFIED_AS_insert" ON neksur."CLASSIFIED_AS" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "CLASSIFIED_AS_update" ON neksur."CLASSIFIED_AS" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "CLASSIFIED_AS_delete" ON neksur."CLASSIFIED_AS" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."CLASSIFIED_AS" ADD CONSTRAINT "CLASSIFIED_AS_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 25/43  APPLIES_TO ------------------------------------------------
ALTER TABLE neksur."APPLIES_TO" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."APPLIES_TO" FORCE ROW LEVEL SECURITY;
CREATE POLICY "APPLIES_TO_select" ON neksur."APPLIES_TO" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "APPLIES_TO_insert" ON neksur."APPLIES_TO" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "APPLIES_TO_update" ON neksur."APPLIES_TO" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "APPLIES_TO_delete" ON neksur."APPLIES_TO" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."APPLIES_TO" ADD CONSTRAINT "APPLIES_TO_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 26/43  DEFINED_BY ------------------------------------------------
ALTER TABLE neksur."DEFINED_BY" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."DEFINED_BY" FORCE ROW LEVEL SECURITY;
CREATE POLICY "DEFINED_BY_select" ON neksur."DEFINED_BY" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DEFINED_BY_insert" ON neksur."DEFINED_BY" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DEFINED_BY_update" ON neksur."DEFINED_BY" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "DEFINED_BY_delete" ON neksur."DEFINED_BY" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."DEFINED_BY" ADD CONSTRAINT "DEFINED_BY_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 27/43  WROTE ------------------------------------------------
ALTER TABLE neksur."WROTE" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."WROTE" FORCE ROW LEVEL SECURITY;
CREATE POLICY "WROTE_select" ON neksur."WROTE" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "WROTE_insert" ON neksur."WROTE" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "WROTE_update" ON neksur."WROTE" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "WROTE_delete" ON neksur."WROTE" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."WROTE" ADD CONSTRAINT "WROTE_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 28/43  READ ------------------------------------------------
ALTER TABLE neksur."READ" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."READ" FORCE ROW LEVEL SECURITY;
CREATE POLICY "READ_select" ON neksur."READ" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "READ_insert" ON neksur."READ" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "READ_update" ON neksur."READ" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "READ_delete" ON neksur."READ" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."READ" ADD CONSTRAINT "READ_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 29/43  PRODUCES ------------------------------------------------
ALTER TABLE neksur."PRODUCES" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."PRODUCES" FORCE ROW LEVEL SECURITY;
CREATE POLICY "PRODUCES_select" ON neksur."PRODUCES" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "PRODUCES_insert" ON neksur."PRODUCES" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "PRODUCES_update" ON neksur."PRODUCES" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "PRODUCES_delete" ON neksur."PRODUCES" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."PRODUCES" ADD CONSTRAINT "PRODUCES_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 30/43  CONSUMES ------------------------------------------------
ALTER TABLE neksur."CONSUMES" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."CONSUMES" FORCE ROW LEVEL SECURITY;
CREATE POLICY "CONSUMES_select" ON neksur."CONSUMES" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "CONSUMES_insert" ON neksur."CONSUMES" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "CONSUMES_update" ON neksur."CONSUMES" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "CONSUMES_delete" ON neksur."CONSUMES" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."CONSUMES" ADD CONSTRAINT "CONSUMES_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 31/43  GOVERNED_BY ------------------------------------------------
ALTER TABLE neksur."GOVERNED_BY" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."GOVERNED_BY" FORCE ROW LEVEL SECURITY;
CREATE POLICY "GOVERNED_BY_select" ON neksur."GOVERNED_BY" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GOVERNED_BY_insert" ON neksur."GOVERNED_BY" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GOVERNED_BY_update" ON neksur."GOVERNED_BY" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GOVERNED_BY_delete" ON neksur."GOVERNED_BY" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."GOVERNED_BY" ADD CONSTRAINT "GOVERNED_BY_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 32/43  STORED_IN ------------------------------------------------
ALTER TABLE neksur."STORED_IN" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."STORED_IN" FORCE ROW LEVEL SECURITY;
CREATE POLICY "STORED_IN_select" ON neksur."STORED_IN" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "STORED_IN_insert" ON neksur."STORED_IN" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "STORED_IN_update" ON neksur."STORED_IN" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "STORED_IN_delete" ON neksur."STORED_IN" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."STORED_IN" ADD CONSTRAINT "STORED_IN_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 33/43  RUNS_ON ------------------------------------------------
ALTER TABLE neksur."RUNS_ON" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RUNS_ON" FORCE ROW LEVEL SECURITY;
CREATE POLICY "RUNS_ON_select" ON neksur."RUNS_ON" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "RUNS_ON_insert" ON neksur."RUNS_ON" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "RUNS_ON_update" ON neksur."RUNS_ON" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "RUNS_ON_delete" ON neksur."RUNS_ON" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."RUNS_ON" ADD CONSTRAINT "RUNS_ON_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 34/43  SUPERSEDES ------------------------------------------------
ALTER TABLE neksur."SUPERSEDES" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."SUPERSEDES" FORCE ROW LEVEL SECURITY;
CREATE POLICY "SUPERSEDES_select" ON neksur."SUPERSEDES" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "SUPERSEDES_insert" ON neksur."SUPERSEDES" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "SUPERSEDES_update" ON neksur."SUPERSEDES" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "SUPERSEDES_delete" ON neksur."SUPERSEDES" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."SUPERSEDES" ADD CONSTRAINT "SUPERSEDES_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 35/43  INTENDED_WRITE ------------------------------------------------
ALTER TABLE neksur."INTENDED_WRITE" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."INTENDED_WRITE" FORCE ROW LEVEL SECURITY;
CREATE POLICY "INTENDED_WRITE_select" ON neksur."INTENDED_WRITE" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "INTENDED_WRITE_insert" ON neksur."INTENDED_WRITE" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "INTENDED_WRITE_update" ON neksur."INTENDED_WRITE" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "INTENDED_WRITE_delete" ON neksur."INTENDED_WRITE" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."INTENDED_WRITE" ADD CONSTRAINT "INTENDED_WRITE_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 36/43  ACTUAL_WRITE ------------------------------------------------
ALTER TABLE neksur."ACTUAL_WRITE" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."ACTUAL_WRITE" FORCE ROW LEVEL SECURITY;
CREATE POLICY "ACTUAL_WRITE_select" ON neksur."ACTUAL_WRITE" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "ACTUAL_WRITE_insert" ON neksur."ACTUAL_WRITE" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "ACTUAL_WRITE_update" ON neksur."ACTUAL_WRITE" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "ACTUAL_WRITE_delete" ON neksur."ACTUAL_WRITE" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."ACTUAL_WRITE" ADD CONSTRAINT "ACTUAL_WRITE_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 37/43  VIOLATION_DETECTED_BY ------------------------------------------------
ALTER TABLE neksur."VIOLATION_DETECTED_BY" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."VIOLATION_DETECTED_BY" FORCE ROW LEVEL SECURITY;
CREATE POLICY "VIOLATION_DETECTED_BY_select" ON neksur."VIOLATION_DETECTED_BY" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "VIOLATION_DETECTED_BY_insert" ON neksur."VIOLATION_DETECTED_BY" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "VIOLATION_DETECTED_BY_update" ON neksur."VIOLATION_DETECTED_BY" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "VIOLATION_DETECTED_BY_delete" ON neksur."VIOLATION_DETECTED_BY" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."VIOLATION_DETECTED_BY" ADD CONSTRAINT "VIOLATION_DETECTED_BY_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 38/43  BELONGS_TO ------------------------------------------------
ALTER TABLE neksur."BELONGS_TO" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."BELONGS_TO" FORCE ROW LEVEL SECURITY;
CREATE POLICY "BELONGS_TO_select" ON neksur."BELONGS_TO" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "BELONGS_TO_insert" ON neksur."BELONGS_TO" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "BELONGS_TO_update" ON neksur."BELONGS_TO" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "BELONGS_TO_delete" ON neksur."BELONGS_TO" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."BELONGS_TO" ADD CONSTRAINT "BELONGS_TO_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 39/43  OF_TABLE ------------------------------------------------
ALTER TABLE neksur."OF_TABLE" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."OF_TABLE" FORCE ROW LEVEL SECURITY;
CREATE POLICY "OF_TABLE_select" ON neksur."OF_TABLE" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "OF_TABLE_insert" ON neksur."OF_TABLE" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "OF_TABLE_update" ON neksur."OF_TABLE" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "OF_TABLE_delete" ON neksur."OF_TABLE" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."OF_TABLE" ADD CONSTRAINT "OF_TABLE_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 40/43  USED_ENGINE ------------------------------------------------
ALTER TABLE neksur."USED_ENGINE" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."USED_ENGINE" FORCE ROW LEVEL SECURITY;
CREATE POLICY "USED_ENGINE_select" ON neksur."USED_ENGINE" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "USED_ENGINE_insert" ON neksur."USED_ENGINE" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "USED_ENGINE_update" ON neksur."USED_ENGINE" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "USED_ENGINE_delete" ON neksur."USED_ENGINE" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."USED_ENGINE" ADD CONSTRAINT "USED_ENGINE_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 41/43  USES_DIMENSION ------------------------------------------------
ALTER TABLE neksur."USES_DIMENSION" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."USES_DIMENSION" FORCE ROW LEVEL SECURITY;
CREATE POLICY "USES_DIMENSION_select" ON neksur."USES_DIMENSION" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "USES_DIMENSION_insert" ON neksur."USES_DIMENSION" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "USES_DIMENSION_update" ON neksur."USES_DIMENSION" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "USES_DIMENSION_delete" ON neksur."USES_DIMENSION" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."USES_DIMENSION" ADD CONSTRAINT "USES_DIMENSION_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 42/43  RAN_ON ------------------------------------------------
ALTER TABLE neksur."RAN_ON" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."RAN_ON" FORCE ROW LEVEL SECURITY;
CREATE POLICY "RAN_ON_select" ON neksur."RAN_ON" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "RAN_ON_insert" ON neksur."RAN_ON" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "RAN_ON_update" ON neksur."RAN_ON" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "RAN_ON_delete" ON neksur."RAN_ON" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."RAN_ON" ADD CONSTRAINT "RAN_ON_tenant_id_required" CHECK (properties ? 'tenant_id');

-- ----- 43/43  GOVERNS ------------------------------------------------
ALTER TABLE neksur."GOVERNS" ENABLE ROW LEVEL SECURITY;
ALTER TABLE neksur."GOVERNS" FORCE ROW LEVEL SECURITY;
CREATE POLICY "GOVERNS_select" ON neksur."GOVERNS" FOR SELECT USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GOVERNS_insert" ON neksur."GOVERNS" FOR INSERT WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GOVERNS_update" ON neksur."GOVERNS" FOR UPDATE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true)) WITH CHECK ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
CREATE POLICY "GOVERNS_delete" ON neksur."GOVERNS" FOR DELETE USING ((properties->>'tenant_id') = current_setting('app.current_tenant', true));
ALTER TABLE neksur."GOVERNS" ADD CONSTRAINT "GOVERNS_tenant_id_required" CHECK (properties ? 'tenant_id');


COMMIT;
