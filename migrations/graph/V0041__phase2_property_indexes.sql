-- =====================================================================
-- V0041 — Phase 2 property + structural edge indexes (GC-01 carryover).
--
-- Three index groups land here (same shape as V0031 for Phase 1):
--
-- (1) AGE property indexes via the V0020 polyfill create_property_index:
--     - CompiledPolicy.source_policy_id      (lookup by source Policy id)
--     - CompiledPolicy.engine_kind           (filter by engine kind)
--     - CompiledPolicy.status                (filter by {pending,active,probe_failed,compile_failed})
--     - CompiledPolicy.source_policy_version (version pinning per D-2.04)
--     - Attribute.name                       (ABAC attribute lookup by name)
--
-- (2) GC-01 structural btree indexes on each new elabel's start_id /
--     end_id (10 new elabels × 2 sides = 20 indexes). AGE 1.6's planner
--     forces Nested Loop on edge joins unless these btrees exist; without
--     them the Pitfall 12 cartesian-blowup surfaces on any new traversal.
--
-- All three groups use CREATE INDEX IF NOT EXISTS / the polyfill's
-- internal IF NOT EXISTS so this migration is fully idempotent.
-- =====================================================================

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- (1) Property indexes (AGE polyfill from V0020) ---------------------
SELECT create_property_index('neksur', 'CompiledPolicy', 'source_policy_id');
SELECT create_property_index('neksur', 'CompiledPolicy', 'engine_kind');
SELECT create_property_index('neksur', 'CompiledPolicy', 'status');
SELECT create_property_index('neksur', 'CompiledPolicy', 'source_policy_version');
SELECT create_property_index('neksur', 'Attribute', 'name');

-- ----- (2) GC-01 structural btree on each new elabel's start_id/end_id ----
-- Mirror V0031 pattern: AGE planner needs btrees on start_id/end_id to
-- avoid Seq Scan on edge-join paths. Without these the Pitfall 12
-- "Nested Loop cartesian" surfaces on the new Phase 2 traversals.

-- D-2.02 (6 governs edges) ----------------------------------------------
CREATE INDEX IF NOT EXISTS idx_residency_governs_start_id      ON neksur."RESIDENCY_GOVERNS"      (start_id);
CREATE INDEX IF NOT EXISTS idx_residency_governs_end_id        ON neksur."RESIDENCY_GOVERNS"      (end_id);

CREATE INDEX IF NOT EXISTS idx_classification_governs_start_id ON neksur."CLASSIFICATION_GOVERNS" (start_id);
CREATE INDEX IF NOT EXISTS idx_classification_governs_end_id   ON neksur."CLASSIFICATION_GOVERNS" (end_id);

CREATE INDEX IF NOT EXISTS idx_partition_governs_start_id      ON neksur."PARTITION_GOVERNS"      (start_id);
CREATE INDEX IF NOT EXISTS idx_partition_governs_end_id        ON neksur."PARTITION_GOVERNS"      (end_id);

CREATE INDEX IF NOT EXISTS idx_row_filter_governs_start_id     ON neksur."ROW_FILTER_GOVERNS"     (start_id);
CREATE INDEX IF NOT EXISTS idx_row_filter_governs_end_id       ON neksur."ROW_FILTER_GOVERNS"     (end_id);

CREATE INDEX IF NOT EXISTS idx_column_mask_governs_start_id    ON neksur."COLUMN_MASK_GOVERNS"    (start_id);
CREATE INDEX IF NOT EXISTS idx_column_mask_governs_end_id      ON neksur."COLUMN_MASK_GOVERNS"    (end_id);

CREATE INDEX IF NOT EXISTS idx_abac_governs_start_id           ON neksur."ABAC_GOVERNS"           (start_id);
CREATE INDEX IF NOT EXISTS idx_abac_governs_end_id             ON neksur."ABAC_GOVERNS"           (end_id);

-- D-2.04 (3 compile-artifact edges) -------------------------------------
CREATE INDEX IF NOT EXISTS idx_compiled_from_start_id          ON neksur."COMPILED_FROM"          (start_id);
CREATE INDEX IF NOT EXISTS idx_compiled_from_end_id            ON neksur."COMPILED_FROM"          (end_id);

CREATE INDEX IF NOT EXISTS idx_applies_to_start_id             ON neksur."APPLIES_TO"             (start_id);
CREATE INDEX IF NOT EXISTS idx_applies_to_end_id               ON neksur."APPLIES_TO"             (end_id);

CREATE INDEX IF NOT EXISTS idx_governed_by_start_id            ON neksur."GOVERNED_BY"            (start_id);
CREATE INDEX IF NOT EXISTS idx_governed_by_end_id              ON neksur."GOVERNED_BY"            (end_id);

-- D-2.10 (1 ABAC edge) --------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_has_attribute_start_id          ON neksur."HAS_ATTRIBUTE"          (start_id);
CREATE INDEX IF NOT EXISTS idx_has_attribute_end_id            ON neksur."HAS_ATTRIBUTE"          (end_id);
