-- =====================================================================
-- V0065 — Extend per-tenant audit_log with WriteEvent columns (Phase 1).
--
-- Phase 0.5 V0050 established audit_log with (id, occurred_at, actor_user_id,
-- event_type, payload). Phase 1 adds three columns so commit-time policy
-- decisions can be journalled in a queryable shape:
--   - `decision`            APPROVED | REJECTED (the L1 gateway verdict)
--   - `principal_source`    mtls_san | auth_header | session (Pitfall 8 —
--                            which auth path produced the principal)
--   - `commit_request_hash` SHA-256 of the commit body (request-replay dedup
--                            + audit-log forensics tying audit row to gateway
--                            request)
--
-- The GRANT contract from V0050 (INSERT-only for neksur_app; UPDATE/DELETE
-- REVOKED) is unchanged — ALTER TABLE ADD COLUMN does not perturb table-level
-- GRANTs. Audit-log tamper resistance (T-1-audit-tamper in PLAN.md threat
-- model) is preserved.
--
-- The filtered index `(decision, occurred_at DESC) WHERE decision IS NOT NULL`
-- lets the admin UI's "write decisions" view skip non-WriteEvent audit rows.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: ALTER TABLE ADD COLUMN IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS decision text
    CHECK (decision IN ('APPROVED','REJECTED'));

ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS principal_source text
    CHECK (principal_source IN ('mtls_san','auth_header','session'));

ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS commit_request_hash bytea;

-- Filtered index — skip NULL rows so non-WriteEvent audit entries (Phase 0.5
-- shapes) stay out of the write-decisions view.
CREATE INDEX IF NOT EXISTS idx_audit_log_decision_occurred_at
    ON audit_log (decision, occurred_at DESC)
    WHERE decision IS NOT NULL;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    cols_ok     int;
    idx_ok      boolean;
    schema_name text := current_schema();
BEGIN
    SELECT count(*)::int INTO cols_ok
    FROM information_schema.columns
    WHERE table_schema = schema_name
      AND table_name   = 'audit_log'
      AND column_name IN ('decision','principal_source','commit_request_hash');

    IF cols_ok <> 3 THEN
        RAISE EXCEPTION 'V0065 verify: expected 3 new audit_log columns, found % (schema %)', cols_ok, schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'audit_log'
          AND indexname  = 'idx_audit_log_decision_occurred_at'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0065 verify: idx_audit_log_decision_occurred_at missing in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0065 OK — audit_log extended with decision/principal_source/commit_request_hash in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
