-- =====================================================================
-- V0020 — D-001.07 property + edge indexes + Postgres functional indexes.
--
-- Runs AFTER V0010 (graph + labels) and BEFORE any data load. This matters:
-- AGE issue #1010 documents that GIN indexes on `properties` created
-- AFTER data is loaded do not catch existing rows. We sidestep that in
-- V0025 by also creating GIN indexes BEFORE load. For btree on properties
-- (created via create_property_index) the index does pick up later inserts,
-- but consistency benefits from "all indexes before any data" — and so
-- does EXPLAIN behaviour in load tests.
--
-- Indexes per ADR-001 §3.6 + D-001.07:
--   • 11 property indexes via AGE's create_property_index()
--   • 3 edge timestamp indexes via create_property_index_edge()
--   • 2 Postgres functional indexes (idx_table_namespace, idx_snapshot_time)
-- =====================================================================

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- 11 property indexes (D-001.07) -------------------------------------
SELECT create_property_index('neksur', 'Table', 'uri');
SELECT create_property_index('neksur', 'Table', 'catalog_id');
SELECT create_property_index('neksur', 'Column', 'uri');
SELECT create_property_index('neksur', 'Column', 'parent_table_uri');
SELECT create_property_index('neksur', 'Snapshot', 'snapshot_id');
SELECT create_property_index('neksur', 'Snapshot', 'table_uri');
SELECT create_property_index('neksur', 'Snapshot', 'committed_at');
SELECT create_property_index('neksur', 'Metric', 'name');
SELECT create_property_index('neksur', 'Person', 'email');
SELECT create_property_index('neksur', 'Tag', 'id');
SELECT create_property_index('neksur', 'Query', 'query_id');

-- ----- 3 edge timestamp indexes (D-001.07) --------------------------------
SELECT create_property_index_edge('neksur', 'LINEAGE_OF', 'created_at');
SELECT create_property_index_edge('neksur', 'READ', 'at');
SELECT create_property_index_edge('neksur', 'WROTE', 'at');

-- ----- 2 Postgres functional indexes (D-001.07 + ADR §3.6) ----------------
-- Namespace filter is common on Table queries.
CREATE INDEX IF NOT EXISTS idx_table_namespace
    ON neksur."Table"
    USING btree ((properties->>'namespace'));

-- Time-range queries on Snapshot (typical AI-context retrieval pattern).
CREATE INDEX IF NOT EXISTS idx_snapshot_time
    ON neksur."Snapshot"
    USING btree (((properties->>'committed_at')::timestamptz));
