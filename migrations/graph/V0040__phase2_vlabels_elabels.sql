-- =====================================================================
-- V0040 — Phase 2 graph schema extension: 2 new vlabels + 10 new elabels.
--
-- Per Phase 2 D-2.02 + D-2.04 + D-2.10 (PROJECT-level decisions):
--   vlabels (2):
--     CompiledPolicy   — per-engine compiled policy artifact (D-2.04)
--     Attribute        — ABAC attribute store node                (D-2.10)
--   elabels (10):
--     D-2.02 (6 "governs" edges from Policy -> Table/Column):
--       RESIDENCY_GOVERNS         — P4 data residency
--       CLASSIFICATION_GOVERNS    — P5 classification requirement
--       PARTITION_GOVERNS         — P7 partition-spec constraint
--       ROW_FILTER_GOVERNS        — row-filter policy (SQL fragment)
--       COLUMN_MASK_GOVERNS       — column-mask policy (SQL fragment, col-level)
--       ABAC_GOVERNS              — ABAC policy (CEL predicate)
--     D-2.04 (3 compile-artifact edges):
--       COMPILED_FROM             — CompiledPolicy -> Policy
--       APPLIES_TO                — CompiledPolicy -> Table
--       GOVERNED_BY               — CompiledPolicy -> Engine (Engine vlabel from V0010 — reused)
--     D-2.10 (1 ABAC edge):
--       HAS_ATTRIBUTE             — Principal -> Attribute
--
-- After this migration the Phase 2 graph inventory is:
--   25 vlabels (19 from V0010 + 4 from V0030 + 2 above)
--   39 elabels (24 from V0010 + 5 from V0030 + 10 above)
--
-- IMPORTANT: Engine vlabel from V0010 line 48 is REUSED — Phase 2 does
-- NOT recreate it (the GOVERNED_BY edge targets the existing Engine
-- vlabel; recreating would error "label already exists" without IF NOT
-- EXISTS).
--
-- Pitfall 7 (Phase 0 RESEARCH): AGE label DDL is not always
-- transactional. The BEGIN/COMMIT wrapping is for direct psql apply;
-- when applied through internal/migrate.ApplyTenantGraph the runner
-- strips it (it manages its own transaction). The DO-block count verify
-- at the end aborts on mismatch.
--
-- Pitfall 9 (AGE 1.6 disjunction edge labels): no `[:A|B]` disjunctions
-- — every elabel is declared separately. Pattern documented here for
-- downstream Plans 02-03..02-07 that query these edges; they MUST split
-- multi-label MATCH queries into per-label queries.
--
-- AGE 1.6 reminder for downstream plans (none of this matters in V0040
-- itself because we only declare labels, not write data, but downstream
-- compilers + ABAC bindings MUST honor):
--   - `tenant_id` is mandatory in inline MERGE properties JSONB.
--   - One MERGE per `cypher()` call.
--   - Schema-qualified writes: `MERGE (n:CompiledPolicy { ... })` inside
--     `tenant_<uuid>.` schema, never in `public`.
--   - `graph.SanitizeCypherLiteral` for any user input.
--   - No `nodes(path)` list comprehensions — walk paths app-side.
--
-- Idempotency: every create_vlabel / create_elabel is guarded by an
-- ag_catalog.ag_label existence probe (mirror V0030). Pre-requisite:
-- the `neksur` graph exists for the current tenant schema (created by
-- internal/tenant/provision.go::CreateGraph at onboarding) AND V0030's
-- 23 vlabels + 29 elabels are already in place (the count verify
-- raises if not — linear migration ordering enforced).
-- =====================================================================

BEGIN;

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- ----- 2 new vlabels (D-2.04 + D-2.10) -----------------------------------
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'CompiledPolicy' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'CompiledPolicy');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'Attribute' AND kind = 'v') THEN
        PERFORM create_vlabel('neksur', 'Attribute');
    END IF;
END $$;

-- ----- 10 new elabels --------------------------------------------------

-- D-2.02 (6 "governs" edges): Policy -> Table (or Policy -> Column for COLUMN_MASK_GOVERNS).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'RESIDENCY_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'RESIDENCY_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'CLASSIFICATION_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'CLASSIFICATION_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'PARTITION_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'PARTITION_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'ROW_FILTER_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'ROW_FILTER_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'COLUMN_MASK_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'COLUMN_MASK_GOVERNS');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'ABAC_GOVERNS' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'ABAC_GOVERNS');
    END IF;
END $$;

-- D-2.04 (3 compile-artifact edges): CompiledPolicy -> {Policy, Table, Engine}.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'COMPILED_FROM' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'COMPILED_FROM');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'APPLIES_TO' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'APPLIES_TO');
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'GOVERNED_BY' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'GOVERNED_BY');
    END IF;
END $$;

-- D-2.10 (1 ABAC edge): Principal -> Attribute.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM ag_catalog.ag_label
                   WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
                     AND name = 'HAS_ATTRIBUTE' AND kind = 'e') THEN
        PERFORM create_elabel('neksur', 'HAS_ATTRIBUTE');
    END IF;
END $$;

-- ----- Post-creation verification -----------------------------------------
-- Mirror V0030 lines 122-148: count vlabels / elabels excluding AGE's
-- synthetic `_ag_label_vertex` / `_ag_label_edge` placeholders. The
-- DO-block raises EXCEPTION on mismatch, aborting the surrounding tx so
-- a partial state is never committed (Pitfall 7).
DO $$
DECLARE
    vcount INT;
    ecount INT;
    expected_vcount CONSTANT INT := 25;
    expected_ecount CONSTANT INT := 39;
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
        RAISE EXCEPTION 'V0040 vlabel count mismatch: expected %, got % — Phase 0+1+2 should yield 19 + 4 + 2 = 25', expected_vcount, vcount;
    END IF;
    IF ecount <> expected_ecount THEN
        RAISE EXCEPTION 'V0040 elabel count mismatch: expected %, got % — Phase 0+1+2 should yield 24 + 5 + 10 = 39', expected_ecount, ecount;
    END IF;
END
$$ LANGUAGE plpgsql;

COMMIT;
