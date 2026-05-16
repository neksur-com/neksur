-- =====================================================================
-- V0071 — Phase 2 ABAC layer 3 fallback: tenant_default_attributes.
--
-- Per D-2.10: ABAC attribute resolution walks three layers in order:
--   (1) OIDC claims from WorkOS         — read from validated JWT.
--   (2) Per-principal `HAS_ATTRIBUTE`   — graph-stored overrides.
--   (3) Tenant-default attributes       — THIS COLUMN.
--
-- The third layer is the safety net: when neither the principal's JWT
-- nor their graph-stored overrides supply a given attribute, the
-- tenant-wide default applies. `principal.attribute(name)` returns null
-- if all three layers fail — and per ADR-007 fail-closed contract, a
-- null in a non-null-handling CEL predicate denies (the safe default).
--
-- Mirror V0065 ALTER TABLE ADD COLUMN IF NOT EXISTS idempotency pattern.
-- NOT NULL DEFAULT '{}'::jsonb keeps every existing tenant row in a
-- well-defined state without backfill.
--
-- Threat T-2-tenant-default-attrs-injection (PLAN threat model):
-- JSONB column is admin-write-only via the Phase 0.5 admin pool GRANT
-- (admin_role has INSERT/UPDATE on public.tenants per V0041 line 139);
-- tenant role has SELECT-only via the Phase 0.5 Layer 3 RLS predicate
-- on public.tenants (V0042). The ABAC consumer (Plan 02-03 attribute.go)
-- MUST type-cast values defensively.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
-- =====================================================================

ALTER TABLE public.tenants
    ADD COLUMN IF NOT EXISTS tenant_default_attributes jsonb NOT NULL DEFAULT '{}'::jsonb;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    col_ok boolean;
BEGIN
    SELECT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name   = 'tenants'
          AND column_name  = 'tenant_default_attributes'
          AND data_type    = 'jsonb'
          AND is_nullable  = 'NO'
    ) INTO col_ok;
    IF col_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0071 verify: tenant_default_attributes jsonb NOT NULL missing on public.tenants';
    END IF;

    RAISE NOTICE 'V0071 OK — public.tenants.tenant_default_attributes ready (D-2.10 ABAC layer 3).';
END
$$ LANGUAGE plpgsql;
