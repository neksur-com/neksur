-- =====================================================================
-- V0062 — Per-tenant detection_runs table (applied to every tenant_<uuid> schema).
--
-- Phase 1 L3 regex post-commit detection (Plan 01-07) records one row per
-- scan kicked off against a Snapshot's data files. The
-- `snapshot_metadata_location UNIQUE` constraint is the Pitfall 10 mitigation
-- (RESEARCH §Pitfall 10 line 1512) — two replicas of `neksur-server` behind
-- ALB each receive the same SNS event; one wins the race, the second's
-- INSERT raises SQLSTATE 23505 (unique_violation) which workers swallow as
-- "already in flight".
--
-- The `scan_strategy` CHECK locks Phase 1 to regex; Phase 6 widens it to
-- include 'ml' per ADR-007 §1 (Phase 1 (M5-M10) ML classifier deferred).
--
-- ADR-001 §3.4 — keyset pagination on (started_at DESC, id DESC).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS detection_runs (
    id                         bigserial    PRIMARY KEY,
    run_id                     uuid         NOT NULL,
    snapshot_metadata_location text         NOT NULL UNIQUE,
    started_at                 timestamptz  NOT NULL DEFAULT now(),
    finished_at                timestamptz,
    scan_strategy              text         NOT NULL
                                            CHECK (scan_strategy IN ('regex')),
    sample_size                int          NOT NULL,
    findings_count             int          NOT NULL DEFAULT 0
);

-- Keyset pagination index per ADR-001 §3.4 — admin UI's detection-runs
-- paginator scans in this order so a fixed-size keyset query is O(rows-per-page).
CREATE INDEX IF NOT EXISTS idx_detection_runs_started_at
    ON detection_runs (started_at DESC, id DESC);

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    tbl_ok      boolean;
    idx_ok      boolean;
    uniq_ok     boolean;
    schema_name text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = schema_name
          AND tablename  = 'detection_runs'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0062 verify: detection_runs not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'detection_runs'
          AND indexname  = 'idx_detection_runs_started_at'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0062 verify: idx_detection_runs_started_at not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = schema_name
          AND t.relname = 'detection_runs'
          AND c.contype = 'u'
    ) INTO uniq_ok;

    IF uniq_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0062 verify: snapshot_metadata_location UNIQUE missing in schema % (Pitfall 10)', schema_name;
    END IF;

    RAISE NOTICE 'V0062 OK — detection_runs + keyset index + UNIQUE constraint ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
