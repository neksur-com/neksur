-- =====================================================================
-- V0010 — Create the `neksur` AGE graph and all canonical labels.
--
-- Schema per ADR-001 D-001.05 + D-001.06, AMENDED by ADR-003 D-003.06:
--   • 19 vlabels  (17 ADR-001 original + WriteEvent + DetectionRun)
--   • 24 elabels  (15 mandatory ADR-001 + INTENDED_WRITE + ACTUAL_WRITE
--                  + VIOLATION_DETECTED_BY per ADR-003 §12.2,
--                  plus 6 engineering supplement)
--
-- Total label tables: 43 (19 + 24).
--
-- Order: this migration runs AFTER V0001 (extensions). Indexes are added
-- in V0020 (property/edge indexes) and V0025 (tenant + GIN) — both BEFORE
-- any data load to avoid AGE issue #1010 (GIN-after-load silent bypass).
--
-- Pitfall 7 (00-RESEARCH.md): AGE label DDL is not always transactional.
-- We wrap the whole migration in BEGIN/COMMIT and verify the catalog
-- count at the end; a count mismatch raises an exception inside a DO
-- block, aborting the transaction so partial state is never committed.
-- =====================================================================

BEGIN;

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- Create the graph itself.
SELECT create_graph('neksur');

-- ----- 19 vlabels ---------------------------------------------------------
-- Original 17 per ADR-001 D-001.05 (order matches the ADR + architecture
-- engineering supplement).
SELECT create_vlabel('neksur', 'Table');
SELECT create_vlabel('neksur', 'Column');
SELECT create_vlabel('neksur', 'Snapshot');
SELECT create_vlabel('neksur', 'Metric');
SELECT create_vlabel('neksur', 'Dimension');
SELECT create_vlabel('neksur', 'View');
SELECT create_vlabel('neksur', 'Dashboard');
SELECT create_vlabel('neksur', 'Pipeline');
SELECT create_vlabel('neksur', 'Query');
SELECT create_vlabel('neksur', 'Person');
SELECT create_vlabel('neksur', 'Team');
SELECT create_vlabel('neksur', 'Policy');
SELECT create_vlabel('neksur', 'GlossaryTerm');
SELECT create_vlabel('neksur', 'Tag');
SELECT create_vlabel('neksur', 'DataContract');
SELECT create_vlabel('neksur', 'Engine');
SELECT create_vlabel('neksur', 'Catalog');
-- Added by ADR-003 D-003.06 (write-path enforcement amendment §12.2)
SELECT create_vlabel('neksur', 'WriteEvent');
SELECT create_vlabel('neksur', 'DetectionRun');

-- ----- 24 elabels ---------------------------------------------------------
-- 15 mandatory ADR-001 D-001.06 (order matches ADR §3.3)
SELECT create_elabel('neksur', 'LINEAGE_OF');
SELECT create_elabel('neksur', 'OWNS');
SELECT create_elabel('neksur', 'MEMBER_OF');
SELECT create_elabel('neksur', 'DEPENDS_ON');
SELECT create_elabel('neksur', 'CLASSIFIED_AS');
SELECT create_elabel('neksur', 'APPLIES_TO');
SELECT create_elabel('neksur', 'DEFINED_BY');
SELECT create_elabel('neksur', 'WROTE');
SELECT create_elabel('neksur', 'READ');
SELECT create_elabel('neksur', 'PRODUCES');
SELECT create_elabel('neksur', 'CONSUMES');
SELECT create_elabel('neksur', 'GOVERNED_BY');
SELECT create_elabel('neksur', 'STORED_IN');
SELECT create_elabel('neksur', 'RUNS_ON');
SELECT create_elabel('neksur', 'SUPERSEDES');
-- Added by ADR-003 D-003.06 (write-path enforcement amendment §12.2)
SELECT create_elabel('neksur', 'INTENDED_WRITE');
SELECT create_elabel('neksur', 'ACTUAL_WRITE');
SELECT create_elabel('neksur', 'VIOLATION_DETECTED_BY');
-- 6 engineering supplement per D-001.06 (graph-architecture v0.1 §2.3)
SELECT create_elabel('neksur', 'BELONGS_TO');
SELECT create_elabel('neksur', 'OF_TABLE');
SELECT create_elabel('neksur', 'USED_ENGINE');
SELECT create_elabel('neksur', 'USES_DIMENSION');
SELECT create_elabel('neksur', 'RAN_ON');
SELECT create_elabel('neksur', 'GOVERNS');

-- ----- Post-creation verification -----------------------------------------
-- Per Pitfall 7 we MUST verify the counts in-band. A count mismatch raises
-- inside the DO block, causing this migration's BEGIN/COMMIT to rollback.
DO $$
DECLARE
    vcount INT;
    ecount INT;
    expected_vcount CONSTANT INT := 19;  -- D-001.05 + D-003.06 amendment
    expected_ecount CONSTANT INT := 24;  -- D-001.06 + D-003.06 amendment
BEGIN
    SELECT count(*) INTO vcount
    FROM ag_catalog.ag_label
    WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
      AND kind = 'v';

    SELECT count(*) INTO ecount
    FROM ag_catalog.ag_label
    WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
      AND kind = 'e';

    IF vcount <> expected_vcount THEN
        RAISE EXCEPTION 'V0010 vlabel count mismatch: expected %, got % — D-001.05 amended by D-003.06 requires 19', expected_vcount, vcount;
    END IF;
    IF ecount <> expected_ecount THEN
        RAISE EXCEPTION 'V0010 elabel count mismatch: expected %, got % — D-001.06 amended by D-003.06 requires 24', expected_ecount, ecount;
    END IF;
END
$$ LANGUAGE plpgsql;

COMMIT;
