-- =====================================================================
-- V0081 — Per-tenant write_conflict_policy column on policies table.
--
-- Per D-3.05 (Claude's Discretion write-conflict): the `write_conflict_policy`
-- column lives in the per-tenant `policies` table for queryability. The
-- policy-publish side (Plan 03-10) syncs this value to the Iceberg table
-- property at policy-publish time. The L1 gateway read path consumes the
-- column directly via the compiled policy cache — no separate lookup.
--
-- Column: `write_conflict_policy text NOT NULL DEFAULT 'retry-with-backoff'
--   CHECK (write_conflict_policy IN ('lww', 'abort', 'retry-with-backoff'))`
--
-- Semantics per D-3.05 and CONTEXT.md Claude's Discretion:
--   - 'lww' (last-writer-wins): the last commit wins; previous writers
--     see their commit silently superseded. Suitable for analytical
--     tables with non-overlapping appends (S3 file-level CAS).
--   - 'abort': any concurrent write is rejected with HTTP 409. Suitable
--     for transactional tables where partial overlaps are intolerable.
--   - 'retry-with-backoff': the gateway retries the commit with
--     exponential backoff on snapshot conflict. DEFAULT — aligns with
--     Iceberg's native optimistic concurrency model (safe baseline for
--     streaming ingest). Per ADR-003 D-OQ.05 fail-closed contract this
--     is the safest default for tables without an explicit policy.
--
-- Threat T-3-05-write-conflict-bypass (PLAN threat model):
-- CHECK constraint allows only {lww, abort, retry-with-backoff}.
-- Plan 03-10 wires license-feature-flag gating for the write path:
-- 'lww' and 'abort' are L2 features (Commercial); 'retry-with-backoff'
-- is L1 (BSL Core / Free) because it maps to Iceberg's own behavior.
--
-- Per D-3.04: the `write_conflict_policy` column lives in L1-readable
-- surface (no build tag) because the gateway read path uses it; the
-- policy-write side is gated by license in Plan 03-10.
--
-- Mirror V0071 ALTER TABLE ADD COLUMN IF NOT EXISTS idempotency pattern.
-- NOT NULL DEFAULT 'retry-with-backoff' keeps every existing row in a
-- well-defined state without backfill.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
-- =====================================================================

ALTER TABLE policies
    ADD COLUMN IF NOT EXISTS write_conflict_policy text
        NOT NULL DEFAULT 'retry-with-backoff'
        CHECK (write_conflict_policy IN ('lww', 'abort', 'retry-with-backoff'));

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    col_ok  boolean;
    chk_ok  boolean;
BEGIN
    SELECT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name   = 'policies'
          AND column_name  = 'write_conflict_policy'
          AND data_type    = 'text'
          AND is_nullable  = 'NO'
    ) INTO col_ok;
    IF col_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0081 verify: write_conflict_policy text NOT NULL missing on %.policies', current_schema();
    END IF;

    -- Confirm the CHECK constraint exists by verifying known-bad value is
    -- rejected. We probe via pg_constraint rather than a live INSERT to
    -- avoid transaction side-effects in the verify block.
    SELECT EXISTS (
        SELECT 1
        FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = current_schema()
          AND t.relname = 'policies'
          AND c.contype = 'c'
          AND pg_get_constraintdef(c.oid) LIKE '%write_conflict_policy IN%'
    ) INTO chk_ok;
    IF chk_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0081 verify: CHECK constraint on write_conflict_policy missing on %.policies', current_schema();
    END IF;

    RAISE NOTICE 'V0081 OK — %.policies.write_conflict_policy ready (D-3.05 write-conflict semantics).', current_schema();
END
$$ LANGUAGE plpgsql;
