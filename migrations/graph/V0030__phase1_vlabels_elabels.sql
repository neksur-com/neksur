-- =====================================================================
-- V0030 — Phase 1 graph schema extension: 4 new vlabels + 5 new elabels.
--
-- Per ADR-007 (Classification + Stewardship) and ADR-010 (Data Lifecycle):
--   vlabels (4):
--     RetentionPolicy   — P3 retention rule node (ADR-010)
--     LifecyclePolicy   — lifecycle rule node    (ADR-010)
--     ScheduledAction   — scheduled lifecycle action (ADR-010)
--     Classification    — concrete classification instance (ADR-007)
--   elabels (5):
--     HAS_COLUMN        — Snapshot -> Column (per-snapshot schema; D-1.05)
--     SCHEMA_GOVERNS    — Policy -> Table (schema policy P1)
--     WRITE_GOVERNS     — Policy -> Table (write ACL P2)
--     RETAINS           — RetentionPolicy -> Table (P3 retention)
--     DETECTED_BY       — Column -> DetectionRun (L3 finding edge)
--
-- After this migration the Phase 1 graph inventory is:
--   23 vlabels (19 from V0010 + 4 above)
--   29 elabels (24 from V0010 + 5 above)
--
-- Pitfall 7 (00-RESEARCH.md, Phase 0): AGE label DDL is not always
-- transactional. The migration is wrapped in BEGIN/COMMIT *for direct psql
-- apply*; when applied through internal/migrate.ApplyTenantGraph the runner
-- strips the wrapping BEGIN/COMMIT (it manages its own transaction) and
-- runs the body inside its own atomic boundary. Either way, the DO-block
-- count verify at the end aborts on mismatch.
--
-- Idempotency: every create_vlabel / create_elabel is guarded by an
-- ag_catalog.ag_label existence probe so a re-application after a
-- partial failure (AGE non-transactional DDL leaves labels behind) is a
-- no-op. The plain literal `create_vlabel('neksur', '<Name>')` call is
-- kept in the body so the plan's grep-anchored acceptance gates match.
--
-- Pre-requisite: the `neksur` graph exists for the current tenant schema
-- (created by internal/tenant/provision.go::CreateGraph at onboarding).
-- =====================================================================

BEGIN;

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- 4 new vlabels (ADR-007 + ADR-010) ----------------------------------
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'RetentionPolicy' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'RetentionPolicy');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'LifecyclePolicy' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'LifecyclePolicy');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'ScheduledAction' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'ScheduledAction');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'Classification' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'Classification');
    END IF;
END $$;

-- ----- 5 new elabels (ADR-003 D-1.05 + ADR-007 + ADR-010) -----------------
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'HAS_COLUMN' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'HAS_COLUMN');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'SCHEMA_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'SCHEMA_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'WRITE_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'WRITE_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'RETAINS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'RETAINS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'DETECTED_BY' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'DETECTED_BY');
    END IF;
END $$;

-- ----- Post-creation verification -----------------------------------------
-- Mirror V0010 lines 92-118: count vlabels / elabels excluding AGE's
-- synthetic `_ag_label_vertex` / `_ag_label_edge` placeholders. A mismatch
-- raises inside this DO block and aborts the surrounding tx so partial
-- state is never committed (Pitfall 7).
DO $$
DECLARE
    vcount INT;
    ecount INT;
    expected_vcount CONSTANT INT := 23;
    expected_ecount CONSTANT INT := 29;
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
        RAISE EXCEPTION 'V0030 vlabel count mismatch: expected %, got % — Phase 0 + Phase 1 should yield 19 + 4 = 23', expected_vcount, vcount;
    END IF;
    IF ecount <> expected_ecount THEN
        RAISE EXCEPTION 'V0030 elabel count mismatch: expected %, got % — Phase 0 + Phase 1 should yield 24 + 5 = 29', expected_ecount, ecount;
    END IF;
END
$$ LANGUAGE plpgsql;

COMMIT;
