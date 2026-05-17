-- =====================================================================
-- V0080 — Per-tenant schema_changed LISTEN/NOTIFY trigger (D-3.05 substrate).
--
-- Per D-3.05 (PROJECT-level decision): the schema-cache invalidation
-- broadcaster subscribes to Postgres LISTEN/NOTIFY on the `schema_changed`
-- channel — this migration lands the producer side.
--
-- Trigger fires AFTER INSERT on the per-tenant `snapshots` table ONLY
-- when NEW.operation IN ('schema_change', 'add_partition_spec',
-- 'replace_partition_spec'). Per RESEARCH Pitfall 1: firing on data
-- appends (operation='append', 'overwrite', etc.) would cause
-- engine-side cache thrash with zero benefit — schema caches don't
-- change when data files are added. The DDL-only filter is the critical
-- performance gate.
--
-- Payload: jsonb_build_object('tenant_id', NEW.tenant_id, 'table_id',
-- NEW.table_id, 'snapshot_id', NEW.snapshot_id, 'operation',
-- NEW.operation)::text — mirrors V0073 payload shape with schema-change
-- specific fields.
--
-- The `tenant_id` field is read from NEW.tenant_id — the snapshots table
-- carries an explicit tenant_id column (per Phase 0 schema-per-tenant
-- invariant). The LISTEN consumer (Plan 03-07) MUST validate the payload's
-- tenant_id matches an allowed tenant before invoking ExecuteInTenant
-- (same T-2-cross-replica-listen-leak mitigation pattern as V0073).
--
-- Threat T-3-02-notify-poison (PLAN threat model): trigger function reads
-- only AGE-managed columns (NEW.tenant_id, NEW.table_id, NEW.snapshot_id,
-- NEW.operation) — no operator-supplied payload reaches pg_notify. Phase
-- 02 RESEARCH §Pitfall 4 documented this for policy_changed; same
-- mitigation for schema_changed.
--
-- DELETE + UPDATE intentionally NOT covered: schema_changed is a
-- forward-only event (appended Iceberg snapshots). Deletes (snapshot
-- expiry) are handled by the compaction coordinator (Plan 03-12); updates
-- don't happen on immutable Iceberg snapshot rows.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE OR REPLACE FUNCTION + DROP TRIGGER IF EXISTS +
-- CREATE TRIGGER (Postgres 16 lacks CREATE OR REPLACE TRIGGER for the
-- per-tenant pattern; the drop-then-create idiom is the canonical form
-- per V0073 precedent).
-- =====================================================================

CREATE OR REPLACE FUNCTION notify_schema_changed() RETURNS trigger AS $$
DECLARE
    payload text;
BEGIN
    -- DDL-only filter per RESEARCH Pitfall 1: only schema-structural
    -- Iceberg operations warrant cache invalidation. Data-only operations
    -- ('append', 'overwrite', 'replace', 'delete', 'compaction') are
    -- explicitly excluded — they don't change the schema so there is
    -- nothing for engines to invalidate.
    IF NEW.operation NOT IN ('schema_change', 'add_partition_spec', 'replace_partition_spec') THEN
        RETURN NEW;
    END IF;

    -- Build the NOTIFY payload as JSON. NEW.tenant_id is an explicit
    -- column on the snapshots table (Phase 0 schema-per-tenant invariant);
    -- we use it directly rather than current_setting('app.current_tenant')
    -- to avoid the strict-GUC pitfall (snapshots table may be written by
    -- the Iceberg REST adapter which doesn't set the GUC — RESEARCH §Pattern
    -- 5). Plan 03-07 LISTEN consumer parses the JSON and validates tenant_id
    -- against an allowed-tenant set before invoking ExecuteInTenant.
    payload := jsonb_build_object(
        'tenant_id',   NEW.tenant_id::text,
        'table_id',    NEW.table_id::text,
        'snapshot_id', NEW.snapshot_id::text,
        'operation',   NEW.operation::text
    )::text;
    PERFORM pg_notify('schema_changed', payload);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Drop-then-create: Postgres 16 lacks CREATE OR REPLACE TRIGGER for the
-- per-tenant idempotent pattern. DROP IF EXISTS is safe.
DROP TRIGGER IF EXISTS schema_changed_trigger ON snapshots;
CREATE TRIGGER schema_changed_trigger
    AFTER INSERT ON snapshots
    FOR EACH ROW
    EXECUTE FUNCTION notify_schema_changed();

-- ----- Verify block --------------------------------------------------
-- Mirror V0073 lines 64-95: assert both the function and the trigger
-- exist in the current schema before committing.
DO $$
DECLARE
    fn_ok        boolean;
    trg_ok       boolean;
    schema_name  text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE p.proname = 'notify_schema_changed'
          AND n.nspname = schema_name
    ) INTO fn_ok;
    IF fn_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0080 verify: notify_schema_changed() not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_trigger t
        JOIN pg_class c ON c.oid = t.tgrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = schema_name
          AND c.relname = 'snapshots'
          AND t.tgname  = 'schema_changed_trigger'
          AND NOT t.tgisinternal
    ) INTO trg_ok;
    IF trg_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0080 verify: schema_changed_trigger missing on snapshots in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0080 OK — notify_schema_changed() + schema_changed_trigger ready in schema % (D-3.05 LISTEN/NOTIFY substrate).', schema_name;
END
$$ LANGUAGE plpgsql;
