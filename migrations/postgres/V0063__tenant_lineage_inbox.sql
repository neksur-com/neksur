-- =====================================================================
-- V0063 — Per-tenant lineage_inbox table (applied to every tenant_<uuid> schema).
--
-- Append-only durability layer for OpenLineage events sent by upstream Spark
-- (and other producers). Pitfall 5 mitigation (RESEARCH §Pitfall 5 line 1473):
-- Spark's OpenLineage HTTP transport is at-least-once; on a transient network
-- blip Spark retries, and without the `UNIQUE (producer, run_id)` constraint
-- here the consumer would MERGE the same event twice, perturbing
-- `created_at` timestamps relative to actual Iceberg snapshot commit time.
--
-- Plan 01-04 introduces the consumer worker that walks unprocessed rows in
-- `received_at` order, calls the Cypher MERGE templates from RESEARCH §Pattern 3,
-- then sets `processed_at`. The UNIQUE constraint causes retried events to
-- raise SQLSTATE 23505 on the inbox INSERT — workers swallow that as "already
-- buffered".
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS.
-- =====================================================================

CREATE TABLE IF NOT EXISTS lineage_inbox (
    id           bigserial    PRIMARY KEY,
    producer     text         NOT NULL,
    run_id       text         NOT NULL,
    event_type   text         NOT NULL,
    received_at  timestamptz  NOT NULL DEFAULT now(),
    processed_at timestamptz,
    payload      jsonb        NOT NULL,
    UNIQUE (producer, run_id)
);

-- Consumer worker scans newest-first via this index.
CREATE INDEX IF NOT EXISTS idx_lineage_inbox_received_at
    ON lineage_inbox (received_at DESC);

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
          AND tablename  = 'lineage_inbox'
    ) INTO tbl_ok;

    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0063 verify: lineage_inbox not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = schema_name
          AND tablename  = 'lineage_inbox'
          AND indexname  = 'idx_lineage_inbox_received_at'
    ) INTO idx_ok;

    IF idx_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0063 verify: idx_lineage_inbox_received_at not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = schema_name
          AND t.relname = 'lineage_inbox'
          AND c.contype = 'u'
    ) INTO uniq_ok;

    IF uniq_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0063 verify: (producer, run_id) UNIQUE missing in schema % (Pitfall 5)', schema_name;
    END IF;

    RAISE NOTICE 'V0063 OK — lineage_inbox + received_at index + UNIQUE constraint ready in schema %.', schema_name;
END
$$ LANGUAGE plpgsql;
