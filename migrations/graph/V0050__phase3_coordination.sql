-- =====================================================================
-- V0050 — Phase 3 graph schema extension: 3 new vlabels + 3 new elabels.
--
-- Per Phase 3 D-3.05 + 03-PATTERNS §9:
--   vlabels (3):
--     SnapshotPin    — per-named-pin node anchoring snapshot_id reads
--     PartitionSpec  — partition-spec version node (D-3.05 spec versioning)
--     DivergenceEvent — cross-engine policy divergence detection record
--
--   elabels (3):
--     PINS          — SnapshotPin -> Table (pin scope binding)
--     USES_SPEC     — Table -> PartitionSpec (current spec binding)
--     DIVERGED_AT   — CompiledPolicy -> DivergenceEvent (audit trail)
--
-- After this migration the Phase 3 graph inventory is:
--   28 vlabels (25 from V0040 + 3 above)
--   42 elabels (39 from V0040 + 3 above)
--
-- IMPORTANT: READ elabel (from V0030) is REUSED with new properties
-- {pinned, pinned_by, at_snapshot} — properties are property-free in
-- AGE label DDL; no migration needed. Documented here for downstream
-- consumers (compiled.go, pin.go, partitionspec/versioning.go):
--
--   (Query)-[:READ {pinned: true, pinned_by: '<principal_id>',
--                   at_snapshot: '<snapshot_id>'}]->(Table)
--
-- The new SnapshotPin node is the authoritative pin record:
--   (SnapshotPin {pin_name, pinned_by_principal, at_snapshot_id,
--                 pinned_at, expiry_utc, tenant_id})
--   (SnapshotPin)-[:PINS]->(Table)
--
-- graph.MustSanitizeCypherLiteral MUST be applied by all downstream
-- callers writing literals into the new vlabels / elabels per Phase 02
-- CR-01 mitigation. This is documented here so Plans 03-06 (pin.go) and
-- 03-08 (partitionspec/versioning.go) reference this header as the
-- canonical safety reminder.
--
-- AGE 1.6 carryovers honored:
--   - No [:A|B] disjunctions — every elabel declared separately.
--   - tenant_id mandatory in inline MERGE properties JSONB.
--   - One MERGE per cypher() call.
--   - Schema-qualified writes via ExecuteInTenant.
--   - graph.MustSanitizeCypherLiteral for every user-supplied literal.
--
-- Idempotency: every create_vlabel / create_elabel is guarded by an
-- ag_catalog.ag_label existence probe (mirror V0040 lines 67-73).
-- Pre-requisite: V0040-V0042 must be applied (Phase 2 baseline).
-- The count-verify DO-block at the end raises EXCEPTION on mismatch,
-- aborting the surrounding transaction so a partial state is never
-- committed (Pitfall 7 from Phase 0.5 RESEARCH).
-- =====================================================================

BEGIN;

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- 3 new vlabels (D-3.05) ----------------------------------------

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'SnapshotPin' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'SnapshotPin');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'PartitionSpec' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'PartitionSpec');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'DivergenceEvent' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'DivergenceEvent');
    END IF;
END $$;

-- ----- 3 new elabels (D-3.05) ----------------------------------------

-- PINS: SnapshotPin -> Table (pin scope binding per D-3.05 SnapshotPin schema).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'PINS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'PINS');
    END IF;
END $$;

-- USES_SPEC: Table -> PartitionSpec (current spec binding per D-3.05 partition-spec versioning).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'USES_SPEC' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'USES_SPEC');
    END IF;
END $$;

-- DIVERGED_AT: CompiledPolicy -> DivergenceEvent (audit trail per D-3.05 verifier).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'DIVERGED_AT' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'DIVERGED_AT');
    END IF;
END $$;

-- ----- Post-creation verification -----------------------------------------
-- Mirror V0040 lines 173-199: count vlabels / elabels excluding AGE's
-- synthetic `_ag_label_vertex` / `_ag_label_edge` placeholders. The
-- DO-block raises EXCEPTION on mismatch, aborting the surrounding tx so
-- a partial state is never committed (Pitfall 7).
DO $$
DECLARE
    vcount INT;
    ecount INT;
    expected_vcount CONSTANT INT := 28;
    expected_ecount CONSTANT INT := 42;
BEGIN
    SELECT count(*) INTO vcount
    FROM ag_catalog.ag_label
    WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
      AND kind = 'v'
      AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\';

    SELECT count(*) INTO ecount
    FROM ag_catalog.ag_label
    WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
      AND kind = 'e'
      AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\';

    IF vcount <> expected_vcount THEN
        RAISE EXCEPTION 'V0050 vlabel count mismatch: expected %, got % — Phase 0+1+2+3 should yield 19+4+2+3 = 28', expected_vcount, vcount;
    END IF;
    IF ecount <> expected_ecount THEN
        RAISE EXCEPTION 'V0050 elabel count mismatch: expected %, got % — Phase 0+1+2+3 should yield 24+5+10+3 = 42', expected_ecount, ecount;
    END IF;
END
$$ LANGUAGE plpgsql;

COMMIT;
