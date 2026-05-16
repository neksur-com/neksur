-- =====================================================================
-- V0072 — Per-tenant policy_compile_log (applied to every tenant_<uuid>).
--
-- Per D-2.05 + Open Question 4 resolution (PLAN line 47):
--   The cross-engine compiler (Plan 02-04) emits one row per compile
--   attempt: (policy_id, engine_kind, engine_version, status, error_message,
--   compiled_at). Status discriminates {pending, active, probe_failed,
--   compile_failed} matching the CompiledPolicy.status enum (PLAN
--   <specifics> line 194).
--
-- OQ4 chose a RELATIONAL table (not a new vlabel) for the audit shape:
--   - cleaner audit query surface (no Cypher needed for "what compiles
--     failed in the last 24h?");
--   - separate from the CompiledPolicy graph node which carries the
--     successful artifact;
--   - per-tenant scope keeps the table small + RLS-isolated.
--
-- `UNIQUE (policy_id, engine_kind, engine_version, compiled_at)` lets
-- the compiler retry on probe_failed without ON CONFLICT plumbing —
-- each retry has a distinct compiled_at timestamp.
--
-- Threat T-2-policy-compile-log-tamper (PLAN threat model):
-- Append-only INSERT GRANT to tenant role; UPDATE/DELETE to operator
-- role only (mirror V0050 audit_log INSERT-only contract).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS policy_compile_log (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id       text         NOT NULL,
    engine_kind     text         NOT NULL
                                 CHECK (engine_kind IN ('trino','spark','dremio','snowflake')),
    engine_version  text         NOT NULL,
    status          text         NOT NULL
                                 CHECK (status IN ('pending','active','probe_failed','compile_failed')),
    error_message   text,
    compiled_at     timestamptz  NOT NULL DEFAULT now(),
    UNIQUE (policy_id, engine_kind, engine_version, compiled_at)
);

-- Index for the audit query path: "what's the latest compile result per
-- (policy_id, engine_kind)?" — Plan 02-04 LISTEN consumer reads via this.
CREATE INDEX IF NOT EXISTS idx_policy_compile_log_policy_engine
    ON policy_compile_log (policy_id, engine_kind, compiled_at DESC);

-- Index for the alert query path: "what compile_failed / probe_failed
-- in the last 24h?" — admin UI + monitoring dashboards read via this.
CREATE INDEX IF NOT EXISTS idx_policy_compile_log_status_compiled_at
    ON policy_compile_log (status, compiled_at DESC)
    WHERE status IN ('compile_failed','probe_failed');

-- INSERT-only contract for tenant role (T-2-policy-compile-log-tamper).
-- The tenant role is granted INSERT but explicitly NOT UPDATE/DELETE.
-- Future ALTERs to the table will need to be reviewed against this
-- audit-tamper-resistance contract (mirror V0050 audit_log discipline).
GRANT INSERT ON policy_compile_log TO neksur_app;
GRANT SELECT ON policy_compile_log TO neksur_app;
REVOKE UPDATE, DELETE, TRUNCATE ON policy_compile_log FROM neksur_app;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    tbl_ok       boolean;
    idx1_ok      boolean;
    idx2_ok      boolean;
    schema_name  text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = schema_name
          AND tablename  = 'policy_compile_log'
    ) INTO tbl_ok;
    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0072 verify: policy_compile_log not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'policy_compile_log'
          AND indexname  = 'idx_policy_compile_log_policy_engine'
    ) INTO idx1_ok;
    IF idx1_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0072 verify: idx_policy_compile_log_policy_engine missing in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'policy_compile_log'
          AND indexname  = 'idx_policy_compile_log_status_compiled_at'
    ) INTO idx2_ok;
    IF idx2_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0072 verify: idx_policy_compile_log_status_compiled_at missing in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0072 OK — policy_compile_log + 2 indexes ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
