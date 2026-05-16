-- =====================================================================
-- V0073 — Per-tenant policy_changed LISTEN/NOTIFY trigger (D-2.05 substrate).
--
-- Per D-2.05 (PROJECT-level decision): the cross-engine compiler runs at
-- `Policy` graph write time. The compiler subscribes to Postgres
-- LISTEN/NOTIFY on the `policy_changed` channel — this migration lands
-- the producer side.
--
-- Trigger fires AFTER INSERT OR UPDATE on the per-tenant `policies`
-- table (created by V0052) and emits a Postgres notification on the
-- `policy_changed` channel with the JSON payload {tenant_id, policy_id}.
--
-- The `tenant_id` field is read from `current_setting('app.current_tenant')`
-- — same GUC the Phase 0.5 Layer 1 search_path discipline sets per
-- request. The LISTEN consumer (Plan 02-04) MUST validate the payload's
-- tenant_id matches an allowed tenant before invoking ExecuteInTenant
-- (T-2-cross-replica-listen-leak mitigation, PLAN threat model).
--
-- DELETE intentionally NOT covered: Policy deletion should NOT trigger
-- a recompile — instead, the gateway sees the missing Policy node on
-- next read and fails closed (D-1.09). Compile-on-delete would race
-- with the compiler reading the just-deleted Policy.
--
-- IMPORTANT (Pitfall 8 from Phase 0.5): `current_setting('app.current_tenant')`
-- WITHOUT the `true` arg ERRORS when the GUC is unset. We want the trigger
-- to fail loudly in that case — a Policy write without a tenant GUC is
-- a Layer 1 isolation bug (Phase 0.5 D-004.03). Using the strict form
-- surfaces the bug instead of emitting a NULL tenant_id NOTIFY payload.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE OR REPLACE FUNCTION + DROP TRIGGER IF EXISTS +
-- CREATE TRIGGER (Postgres 16 lacks CREATE OR REPLACE TRIGGER for the
-- per-tenant pattern; the drop-then-create idiom is the canonical form).
-- =====================================================================

CREATE OR REPLACE FUNCTION notify_policy_changed() RETURNS trigger AS $$
DECLARE
    payload text;
BEGIN
    -- Build the NOTIFY payload as JSON. The strict current_setting (no
    -- `true` arg) raises if the GUC is unset — Phase 0.5 Layer 1 bug
    -- if it fires. Plan 02-04 LISTEN consumer parses the JSON and
    -- validates tenant_id against an allowed-tenant set.
    payload := json_build_object(
        'tenant_id', current_setting('app.current_tenant'),
        'policy_id', NEW.id::text
    )::text;
    PERFORM pg_notify('policy_changed', payload);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Drop-then-create: Postgres 16 lacks CREATE OR REPLACE TRIGGER for the
-- per-tenant idempotent pattern. DROP IF EXISTS is safe.
DROP TRIGGER IF EXISTS policy_changed_trigger ON policies;
CREATE TRIGGER policy_changed_trigger
    AFTER INSERT OR UPDATE ON policies
    FOR EACH ROW
    EXECUTE FUNCTION notify_policy_changed();

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    fn_ok        boolean;
    trg_ok       boolean;
    schema_name  text := current_schema();
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE p.proname = 'notify_policy_changed'
          AND n.nspname = schema_name
    ) INTO fn_ok;
    IF fn_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0073 verify: notify_policy_changed() not created in schema %', schema_name;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_trigger t
        JOIN pg_class c ON c.oid = t.tgrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = schema_name
          AND c.relname = 'policies'
          AND t.tgname  = 'policy_changed_trigger'
          AND NOT t.tgisinternal
    ) INTO trg_ok;
    IF trg_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0073 verify: policy_changed_trigger missing on policies in schema %', schema_name;
    END IF;

    RAISE NOTICE 'V0073 OK — notify_policy_changed() + policy_changed_trigger ready in schema % (D-2.05 LISTEN/NOTIFY substrate).', schema_name;
END
$$ LANGUAGE plpgsql;
