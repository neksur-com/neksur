-- =====================================================================
-- V0044 — public.tenant_by_workos_org(text) RETURNS uuid (SECURITY DEFINER STABLE).
--
-- Purpose: Plan 03 middleware looks up the tenant id from a WorkOS
-- organization id BEFORE `app.current_tenant` is set. Layer 3 RLS on
-- public.tenants would return 0 rows for a SELECT issued without the
-- tenant GUC (the predicate `id::text = current_setting('app.current_tenant', true)`
-- becomes `id::text = NULL` → false). The middleware would never
-- discover the tenant, breaking authentication entirely.
--
-- The standard fix is a single SECURITY DEFINER function that owns
-- RLS bypass for one tightly-scoped lookup. The function:
--   1. Runs as its owner (postgres / superuser) so RLS is bypassed
--      via the table-owner exception (postgres is also the table owner;
--      and superusers bypass RLS unconditionally per Phase 0 deviation #7).
--   2. Is STABLE — same input → same output within a transaction —
--      so the planner caches results and avoids re-running per row.
--   3. Returns NULL for unknown/deleted tenants, so the caller gets
--      a clean "no tenant" signal instead of an exception.
--   4. EXECUTE is REVOKEd from PUBLIC and GRANTed only to neksur_app.
--
-- This function is the SOLE intended RLS-bypass entry point in the
-- application path. All other tenant.* lookups go through
-- public.tenants + app.current_tenant + Layer 3 RLS.
--
-- T-0.5-rls-bypass-missing-guc: the function does not depend on
-- app.current_tenant, so it works correctly when called from middleware
-- before the GUC is set.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
-- =====================================================================

CREATE OR REPLACE FUNCTION public.tenant_by_workos_org(p_org text)
    RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    STABLE
    SET search_path = public, pg_temp
AS $$
    SELECT id
      FROM public.tenants
     WHERE workos_org_id  = p_org
       AND lifecycle_state IN ('active','suspended')
     LIMIT 1
$$;

-- Lock down EXECUTE: PUBLIC by default has EXECUTE on functions; revoke
-- to make this a deliberate grant rather than an accidental capability.
REVOKE ALL    ON FUNCTION public.tenant_by_workos_org(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.tenant_by_workos_org(text) TO neksur_app;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    proc_kind        char;
    proc_volatile    char;
    proc_secdef      boolean;
    has_neksur_app   boolean;
BEGIN
    SELECT p.prokind, p.provolatile, p.prosecdef
      INTO proc_kind, proc_volatile, proc_secdef
      FROM pg_proc p
      JOIN pg_namespace n ON n.oid = p.pronamespace
     WHERE n.nspname = 'public'
       AND p.proname = 'tenant_by_workos_org';

    IF NOT FOUND THEN
        RAISE EXCEPTION 'V0044 verify: function public.tenant_by_workos_org not created';
    END IF;
    IF proc_kind <> 'f' THEN
        RAISE EXCEPTION 'V0044 verify: tenant_by_workos_org is not a plain function (prokind=%)', proc_kind;
    END IF;
    IF proc_volatile <> 's' THEN
        RAISE EXCEPTION 'V0044 verify: tenant_by_workos_org must be STABLE (provolatile=%, expected s)', proc_volatile;
    END IF;
    IF proc_secdef IS NOT TRUE THEN
        RAISE EXCEPTION 'V0044 verify: tenant_by_workos_org must be SECURITY DEFINER';
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM information_schema.role_routine_grants
         WHERE grantee = 'neksur_app'
           AND routine_schema = 'public'
           AND routine_name   = 'tenant_by_workos_org'
           AND privilege_type = 'EXECUTE'
    ) INTO has_neksur_app;
    IF has_neksur_app IS NOT TRUE THEN
        RAISE EXCEPTION 'V0044 verify: neksur_app missing EXECUTE on tenant_by_workos_org';
    END IF;

    RAISE NOTICE 'V0044 OK — tenant_by_workos_org is SECURITY DEFINER STABLE; neksur_app has EXECUTE.';
END
$$ LANGUAGE plpgsql;
