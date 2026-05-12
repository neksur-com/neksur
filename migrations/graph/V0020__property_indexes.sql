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
--
-- POLYFILL NOTE (deviation from plan): AGE 1.6.0 does NOT ship
-- `create_property_index` / `create_property_index_edge` as documented in
-- ADR-001 §3.6 — those names were carried over from a community proposal
-- that did not land in the 1.6 GA. The function calls below resolve to a
-- thin SQL polyfill defined immediately after the LOAD statement; it
-- emits the equivalent `CREATE INDEX ... USING btree((properties->>'<p>'))`
-- against the underlying `neksur."<Label>"` table. When AGE 1.7+ adds the
-- functions natively the polyfill can be retired (or made conditional via
-- pg_proc lookup). The runtime effect is identical: a btree property index
-- usable by EXPLAIN's planner — verified by test_indexes_used_in_explain.
-- =====================================================================

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- Polyfill for AGE 1.6.0 missing create_property_index* --------------
-- Defined in `ag_catalog` so the unqualified calls below resolve here.
-- IF NOT EXISTS handles the AGE-version-with-native case gracefully.
CREATE OR REPLACE FUNCTION ag_catalog.create_property_index(
    graph_name text,
    label_name text,
    property_name text
) RETURNS void
LANGUAGE plpgsql
AS $POLY$
BEGIN
    -- The ::text cast on the RHS resolves to AGE's `agtype ->> text`
    -- operator. Without it, the bare 'literal' is ambiguous between
    -- text and agtype and parses as agtype, failing on non-quoted
    -- identifiers like "uri".
    EXECUTE format(
        'CREATE INDEX IF NOT EXISTS %I ON %I.%I USING btree (((properties->>%L::text)))',
        'idx_' || label_name || '_' || property_name,
        graph_name,
        label_name,
        property_name
    );
END
$POLY$;

CREATE OR REPLACE FUNCTION ag_catalog.create_property_index_edge(
    graph_name text,
    label_name text,
    property_name text
) RETURNS void
LANGUAGE plpgsql
AS $POLY$
BEGIN
    EXECUTE format(
        'CREATE INDEX IF NOT EXISTS %I ON %I.%I USING btree (((properties->>%L::text)))',
        'idx_' || label_name || '_' || property_name || '_edge',
        graph_name,
        label_name,
        property_name
    );
END
$POLY$;

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
-- The `::text` cast on the RHS of `->>` resolves to AGE's
-- `agtype ->> text` operator (see header POLYFILL NOTE).
CREATE INDEX IF NOT EXISTS idx_table_namespace
    ON neksur."Table"
    USING btree ((properties->>'namespace'::text));

-- Time-range queries on Snapshot (typical AI-context retrieval pattern).
-- We index the text form directly — Iceberg snapshots write committed_at
-- as ISO-8601 with a 'Z' suffix; ISO-8601 lexicographic order matches
-- chronological order, so a btree on the text form supports range queries
-- without needing a timestamptz cast. The original ADR §3.6 used
-- ::timestamptz, but that cast is STABLE (not IMMUTABLE) and Postgres
-- rejects it inside index expressions. The text-based index is the
-- DataReplit-portable equivalent; queries that need timestamptz can
-- cast at query time without losing the index (Postgres will still pick
-- the index for `WHERE committed_at >= '2025-01-01'` text comparisons).
CREATE INDEX IF NOT EXISTS idx_snapshot_time
    ON neksur."Snapshot"
    USING btree ((properties->>'committed_at'::text));
