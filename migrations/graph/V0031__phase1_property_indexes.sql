-- =====================================================================
-- V0031 — Phase 1 property + edge indexes + GC-01 structural carryover.
--
-- Three index groups land here:
--
-- (1) AGE property indexes via the V0020 polyfill create_property_index:
--     - Snapshot.metadata_location   (D-1.04 — natural key for MERGE)
--     - Column.snapshot_loc          (D-1.05 — per-snapshot Column key)
--     - Policy.id                    (CEL policy lookup by node id)
--     - RetentionPolicy.id
--     - Classification.tag_id        (ADR-007 — tag-keyed classification)
--     - Classification.detection_run
--     - DetectionRun.run_id          (admin-UI detection-run lookup)
--
-- (2) AGE edge property indexes via create_property_index_edge:
--     - HAS_COLUMN.ordinal           (per-Snapshot column order)
--     - DETECTED_BY.created_at       (alert timeline scans)
--
-- (3) GC-01 structural btree indexes on each new elabel's start_id /
--     end_id. AGE 1.6 does NOT auto-index elabel join columns; the
--     planner falls back to Seq Scan on join unless we add the btrees
--     ourselves. This is the same Phase 0 GC-01 carryover that V0020+V0025
--     applied to the 24 ADR-001 elabels. 5 new elabels x 2 sides = 10
--     indexes.
--
-- All three groups use CREATE INDEX IF NOT EXISTS / the polyfill's
-- internal IF NOT EXISTS so this migration is fully idempotent.
-- =====================================================================

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- (1) Property indexes (AGE polyfill from V0020) ---------------------
SELECT create_property_index('neksur', 'Snapshot', 'metadata_location');
SELECT create_property_index('neksur', 'Column', 'snapshot_loc');
SELECT create_property_index('neksur', 'Policy', 'id');
SELECT create_property_index('neksur', 'RetentionPolicy', 'id');
SELECT create_property_index('neksur', 'Classification', 'tag_id');
SELECT create_property_index('neksur', 'Classification', 'detection_run');
SELECT create_property_index('neksur', 'DetectionRun', 'run_id');

-- ----- (2) Edge property indexes (AGE polyfill from V0020) ----------------
SELECT create_property_index_edge('neksur', 'HAS_COLUMN', 'ordinal');
SELECT create_property_index_edge('neksur', 'DETECTED_BY', 'created_at');

-- ----- (3) GC-01 structural btree on each new elabel's start_id/end_id ----
-- Mirrors V0020/V0025 pattern: AGE planner needs btrees on start_id/end_id
-- to avoid Seq Scan on edge-join paths. Without these the Pitfall 12
-- "Nested Loop cartesian" surfaces on the new HAS_COLUMN traversals.

CREATE INDEX IF NOT EXISTS idx_has_column_start_id     ON neksur."HAS_COLUMN"     (start_id);
CREATE INDEX IF NOT EXISTS idx_has_column_end_id       ON neksur."HAS_COLUMN"     (end_id);

CREATE INDEX IF NOT EXISTS idx_schema_governs_start_id ON neksur."SCHEMA_GOVERNS" (start_id);
CREATE INDEX IF NOT EXISTS idx_schema_governs_end_id   ON neksur."SCHEMA_GOVERNS" (end_id);

CREATE INDEX IF NOT EXISTS idx_write_governs_start_id  ON neksur."WRITE_GOVERNS"  (start_id);
CREATE INDEX IF NOT EXISTS idx_write_governs_end_id    ON neksur."WRITE_GOVERNS"  (end_id);

CREATE INDEX IF NOT EXISTS idx_retains_start_id        ON neksur."RETAINS"        (start_id);
CREATE INDEX IF NOT EXISTS idx_retains_end_id          ON neksur."RETAINS"        (end_id);

CREATE INDEX IF NOT EXISTS idx_detected_by_start_id    ON neksur."DETECTED_BY"    (start_id);
CREATE INDEX IF NOT EXISTS idx_detected_by_end_id      ON neksur."DETECTED_BY"    (end_id);
