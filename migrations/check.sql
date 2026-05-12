-- =====================================================================
-- check.sql — Migration verification harness.
--
-- Run after V0001..V0025 to confirm the schema is in the expected state.
-- Emits a PASS/FAIL row per check. CI gates on `grep -q FAIL` against the
-- output: zero FAIL rows = green.
--
-- Schema contract per ADR-001 D-001.05/.06 amended by ADR-003 D-003.06:
--   • 19 vlabels
--   • 24 elabels
--   • 11 D-001.07 property indexes
--   • 3 edge timestamp indexes
--   • 19 per-vlabel tenant btree indexes (V0025)
--   • 19 per-vlabel GIN indexes on properties (V0025)
--   • 2 Postgres functional indexes (idx_table_namespace, idx_snapshot_time)
--   • Extensions: age, pgaudit, pg_stat_statements
-- =====================================================================

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- vlabel count (expect 19; excludes AGE's synthetic _ag_label_vertex)
SELECT
    CASE WHEN count(*) = 19 THEN 'PASS' ELSE 'FAIL' END AS status,
    'vlabel_count' AS check_name,
    count(*) AS actual,
    19 AS expected
FROM ag_catalog.ag_label
WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
  AND kind = 'v'
  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\';

-- elabel count (expect 24; excludes AGE's synthetic _ag_label_edge)
SELECT
    CASE WHEN count(*) = 24 THEN 'PASS' ELSE 'FAIL' END AS status,
    'elabel_count' AS check_name,
    count(*) AS actual,
    24 AS expected
FROM ag_catalog.ag_label
WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
  AND kind = 'e'
  AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\';

-- Required extensions
SELECT
    CASE WHEN count(*) = 3 THEN 'PASS' ELSE 'FAIL' END AS status,
    'extensions_present' AS check_name,
    count(*) AS actual,
    3 AS expected
FROM pg_extension
WHERE extname IN ('age', 'pgaudit', 'pg_stat_statements');

-- Per-vlabel tenant indexes (expect 19)
SELECT
    CASE WHEN count(*) = 19 THEN 'PASS' ELSE 'FAIL' END AS status,
    'tenant_btree_indexes' AS check_name,
    count(*) AS actual,
    19 AS expected
FROM pg_indexes
WHERE schemaname = 'neksur'
  AND indexname ~ '^idx_[A-Za-z]+_tenant$';

-- Per-vlabel GIN indexes (expect 19)
SELECT
    CASE WHEN count(*) = 19 THEN 'PASS' ELSE 'FAIL' END AS status,
    'gin_props_indexes' AS check_name,
    count(*) AS actual,
    19 AS expected
FROM pg_indexes
WHERE schemaname = 'neksur'
  AND indexname ~ '^idx_[A-Za-z]+_props_gin$';

-- Postgres functional indexes (expect both present)
SELECT
    CASE WHEN count(*) = 2 THEN 'PASS' ELSE 'FAIL' END AS status,
    'functional_indexes' AS check_name,
    count(*) AS actual,
    2 AS expected
FROM pg_indexes
WHERE schemaname = 'neksur'
  AND indexname IN ('idx_table_namespace', 'idx_snapshot_time');
