-- =====================================================================
-- V0025 — Per-vlabel tenant + GIN indexes on `properties`.
--
-- CRITICAL: this migration MUST run BEFORE any bulk data load. Per AGE
-- issue #1010, GIN indexes on `properties` created AFTER data is inserted
-- silently do not catch existing rows — Cypher MATCH ... WHERE n.foo = X
-- falls back to Seq Scan and latency explodes. The integration test
-- tests/integration/test_indexes.py::test_indexes_used_in_explain asserts
-- Index Scan, not Seq Scan, after seeding a row.
--
-- Coverage: all 19 vlabels (D-001.05 + D-003.06 amendment).
--   • idx_<Label>_tenant — btree on (properties->>'tenant_id'::text) — required
--     for RLS policy performance (V0030 references the same expression).
--     The ::text cast resolves to the `agtype ->> text` operator (AGE's
--     properties column is `agtype`, not jsonb — the plan's bare
--     (properties->>'tenant_id') is jsonb-shaped and fails to parse).
--   • idx_<Label>_props_gin — GIN on the whole properties JSONB — supports
--     ad-hoc property filters and the @> containment operator used by
--     observability queries.
--
-- DEVIATION NOTE: the plan documents the index expressions as
-- `(properties->>'tenant_id')` (no cast). That syntax fails at parse
-- time against AGE's agtype. We use the agtype-correct `'tenant_id'::text`
-- form. See 00-02-SUMMARY.md ## Deviations from Plan, Rule 1.
--
-- Edge tables are NOT given GIN indexes here. They share the tenant_id
-- column via RLS in V0030; their hot-path access is index-on-edge-id +
-- the property indexes from V0020 (LINEAGE_OF.created_at, READ.at, WROTE.at).
-- =====================================================================

-- Need ag_catalog on the search path so the `agtype ->> text` operator
-- and the agtype type itself resolve unqualified.
SET search_path = ag_catalog, "$user", public;

-- Tenant + GIN for each vlabel — 19 × 2 = 38 indexes total.
-- Order matches V0010 / D-003.06.

CREATE INDEX IF NOT EXISTS idx_Table_tenant           ON neksur."Table"           USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Table_props_gin           ON neksur."Table"           USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Column_tenant          ON neksur."Column"          USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Column_props_gin          ON neksur."Column"          USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Snapshot_tenant        ON neksur."Snapshot"        USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Snapshot_props_gin        ON neksur."Snapshot"        USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Metric_tenant          ON neksur."Metric"          USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Metric_props_gin          ON neksur."Metric"          USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Dimension_tenant       ON neksur."Dimension"       USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Dimension_props_gin       ON neksur."Dimension"       USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_View_tenant            ON neksur."View"            USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_View_props_gin            ON neksur."View"            USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Dashboard_tenant       ON neksur."Dashboard"       USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Dashboard_props_gin       ON neksur."Dashboard"       USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Pipeline_tenant        ON neksur."Pipeline"        USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Pipeline_props_gin        ON neksur."Pipeline"        USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Query_tenant           ON neksur."Query"           USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Query_props_gin           ON neksur."Query"           USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Person_tenant          ON neksur."Person"          USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Person_props_gin          ON neksur."Person"          USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Team_tenant            ON neksur."Team"            USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Team_props_gin            ON neksur."Team"            USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Policy_tenant          ON neksur."Policy"          USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Policy_props_gin          ON neksur."Policy"          USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_GlossaryTerm_tenant    ON neksur."GlossaryTerm"    USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_GlossaryTerm_props_gin    ON neksur."GlossaryTerm"    USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Tag_tenant             ON neksur."Tag"             USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Tag_props_gin             ON neksur."Tag"             USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_DataContract_tenant    ON neksur."DataContract"    USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_DataContract_props_gin    ON neksur."DataContract"    USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Engine_tenant          ON neksur."Engine"          USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Engine_props_gin          ON neksur."Engine"          USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_Catalog_tenant         ON neksur."Catalog"         USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_Catalog_props_gin         ON neksur."Catalog"         USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_WriteEvent_tenant      ON neksur."WriteEvent"      USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_WriteEvent_props_gin      ON neksur."WriteEvent"      USING GIN (properties);

CREATE INDEX IF NOT EXISTS idx_DetectionRun_tenant    ON neksur."DetectionRun"    USING btree ((properties->>'tenant_id'::text));
CREATE INDEX IF NOT EXISTS idx_DetectionRun_props_gin    ON neksur."DetectionRun"    USING GIN (properties);

