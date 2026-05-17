-- =====================================================================
-- V0051 — Phase 3 property + structural edge indexes (GC-01 carryover).
--
-- Three index groups land here (same shape as V0041 for Phase 2):
--
-- (1) AGE property indexes via the V0020 polyfill create_property_index:
--     - SnapshotPin.pin_name        (named pin lookup)
--     - SnapshotPin.expiry_utc      (TTL expiry sweep)
--     - SnapshotPin.at_snapshot_id  (pin-to-snapshot binding)
--     - PartitionSpec.spec_id       (spec version lookup)
--     - PartitionSpec.table_id      (table-to-spec binding)
--     - DivergenceEvent.detected_at (time-ordered divergence query)
--     - DivergenceEvent.engine_kind (per-engine divergence filter)
--
-- (2) GC-01 structural btree indexes on each new elabel's start_id /
--     end_id (3 new elabels × 2 sides = 6 indexes). AGE 1.6's planner
--     forces Nested Loop on edge joins unless these btrees exist; without
--     them the Pitfall 12 cartesian-blowup surfaces on Phase 3 traversals.
--
-- All groups use CREATE INDEX IF NOT EXISTS / the polyfill's
-- internal IF NOT EXISTS so this migration is fully idempotent.
-- =====================================================================

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- (1) Property indexes (AGE polyfill from V0020) ---------------------
SELECT create_property_index('neksur', 'SnapshotPin', 'pin_name');
SELECT create_property_index('neksur', 'SnapshotPin', 'expiry_utc');
SELECT create_property_index('neksur', 'SnapshotPin', 'at_snapshot_id');
SELECT create_property_index('neksur', 'PartitionSpec', 'spec_id');
SELECT create_property_index('neksur', 'PartitionSpec', 'table_id');
SELECT create_property_index('neksur', 'DivergenceEvent', 'detected_at');
SELECT create_property_index('neksur', 'DivergenceEvent', 'engine_kind');

-- ----- (2) GC-01 structural btree on each new elabel's start_id/end_id ----
-- Mirror V0041 pattern: AGE planner needs btrees on start_id/end_id to
-- avoid Seq Scan on edge-join paths. Without these the Pitfall 12
-- "Nested Loop cartesian" surfaces on the new Phase 3 traversals.

-- PINS: SnapshotPin -> Table -------------------------------------------
CREATE INDEX IF NOT EXISTS idx_pins_start_id    ON neksur."PINS"    (start_id);
CREATE INDEX IF NOT EXISTS idx_pins_end_id      ON neksur."PINS"    (end_id);

-- USES_SPEC: Table -> PartitionSpec ------------------------------------
CREATE INDEX IF NOT EXISTS idx_uses_spec_start_id ON neksur."USES_SPEC" (start_id);
CREATE INDEX IF NOT EXISTS idx_uses_spec_end_id   ON neksur."USES_SPEC" (end_id);

-- DIVERGED_AT: CompiledPolicy -> DivergenceEvent -----------------------
CREATE INDEX IF NOT EXISTS idx_diverged_at_start_id ON neksur."DIVERGED_AT" (start_id);
CREATE INDEX IF NOT EXISTS idx_diverged_at_end_id   ON neksur."DIVERGED_AT" (end_id);
