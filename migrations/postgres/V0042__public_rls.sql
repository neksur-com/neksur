-- =====================================================================
-- V0042 — Layer 3 RLS policies on shared `public.*` tables (D-0.5.03).
--
-- Phase 0's V0030 enabled RLS on the 43 AGE label tables using the
-- agtype-correct predicate `(properties->>'tenant_id'::text) = current_setting(...)`.
-- Phase 0.5 extends Layer 3 to the SHARED public-tier tables using the
-- RELATIONAL form — `id::text = current_setting(...)` and
-- `tenant_id::text = current_setting(...)`. The `properties->>` form
-- is REJECTED here on purpose (Task 1 grep gate guards against accidental
-- copy-paste from V0030 — see threat T-0.5-rls-bypass-properties-form
-- in 00.5-02-PLAN.md).
--
-- Policy shape: SELECT-only for the tenant role; INSERT/UPDATE/DELETE
-- remain admin-role-only (no policy = default deny under FORCE RLS).
-- This matches D-0.5.21 audit-log discipline: tenant code can READ its
-- own `public.tenants` row but cannot mutate it; lifecycle transitions
-- go through admin_role (Plan 04/07 provisioning + lifecycle scripts).
--
-- `public.design_partner_contracts` gets RLS + FORCE RLS + NO policy
-- → tenant role sees 0 rows. Admin role bypasses via BYPASSRLS.
--
-- `public.atlas_schema_revisions` is NOT under RLS — Atlas needs free
-- write access from the migration runner (which runs as superuser /
-- admin_role). The cross-tenant audit query is admin-only and would
-- collide with per-tenant filtering anyway.
--
-- `public.system_audit_log` RLS is configured in V0043 alongside its
-- DDL (INSERT-only GRANT discipline).
--
-- T-0.5-rls-bypass-missing-guc mitigation: the predicate uses
-- `current_setting('app.current_tenant', true)` — the `true` arg makes
-- the function return NULL (not error) when the GUC is unset; NULL = NULL
-- evaluates to NULL → false → 0 rows. RESEARCH §Security Domain line 1635.
-- =====================================================================

BEGIN;

-- ----- public.tenants ------------------------------------------------
ALTER TABLE public.tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenants_select ON public.tenants
    FOR SELECT
    USING (id::text = current_setting('app.current_tenant', true));
-- INSERT/UPDATE/DELETE: no policy → default deny under FORCE RLS.
-- admin_role + table owner bypass via BYPASSRLS for cleanup jobs.

-- ----- public.tenant_billing -----------------------------------------
ALTER TABLE public.tenant_billing ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_billing FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_billing_select ON public.tenant_billing
    FOR SELECT
    USING (tenant_id::text = current_setting('app.current_tenant', true));
-- INSERT/UPDATE/DELETE: admin_role only (Stripe webhook + manual ops in M7).

-- ----- public.design_partner_contracts -------------------------------
-- Admin-role-only table; FORCE RLS + zero policies = default deny for all
-- non-bypass roles. The tenant role MUST NOT see contract amounts.
ALTER TABLE public.design_partner_contracts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.design_partner_contracts FORCE ROW LEVEL SECURITY;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    rls_count    int;
    policy_count int;
BEGIN
    SELECT COUNT(*) INTO rls_count
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public'
      AND c.relname IN ('tenants','tenant_billing','design_partner_contracts')
      AND c.relrowsecurity = true
      AND c.relforcerowsecurity = true;
    IF rls_count <> 3 THEN
        RAISE EXCEPTION 'V0042 verify: expected FORCE RLS on 3 public-tier tables, found %', rls_count;
    END IF;

    SELECT COUNT(*) INTO policy_count
    FROM pg_policies
    WHERE schemaname = 'public'
      AND tablename IN ('tenants','tenant_billing')
      AND policyname IN ('tenants_select','tenant_billing_select');
    IF policy_count <> 2 THEN
        RAISE EXCEPTION 'V0042 verify: expected 2 SELECT policies, found %', policy_count;
    END IF;

    RAISE NOTICE 'V0042 OK — 3 public-tier tables under FORCE RLS, 2 select policies installed.';
END
$$ LANGUAGE plpgsql;

COMMIT;
