-- =====================================================================
-- V0070 — Phase 2 public-tier engine registry table (D-2.08).
--
-- `public.engines` is the per-tenant registry of which query engines a
-- tenant has connected to Neksur for cross-engine policy enforcement.
-- The cross-engine compiler (Plan 02-04+) reads this table to discover
-- which engine kinds + versions need a `CompiledPolicy` artifact for
-- each Policy node.
--
-- Per D-2.08, supported engine kinds are {trino, spark, dremio, snowflake}
-- — Snowflake is plumbing-only in Phase 2 (no live dialect yet; deferred
-- to Phase 5 per ADR-005 ordering). The CHECK constraint locks the
-- taxonomy so downstream code can switch on a known set.
--
-- `UNIQUE (tenant_id, kind, version)` lets a tenant register multiple
-- versions of the same engine kind (e.g., Trino 467 + Trino 470 during
-- a rolling upgrade) without duplicates. Each row gets its own
-- CompiledPolicy artifact per Policy.
--
-- Threat T-2-engine-registry-cross-tenant-write (PLAN threat model):
-- `tenant_id NOT NULL REFERENCES public.tenants(id)` + V0042-style RLS
-- (this migration lands the table; per-tenant RLS predicate added below).
-- CHECK on engine kind allowlist locks the surface.
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS;
-- the FORCE RLS + policies use idempotent ALTER + pg_policies guards.
-- =====================================================================

CREATE TABLE IF NOT EXISTS public.engines (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    kind         text        NOT NULL
                             CHECK (kind IN ('trino','spark','dremio','snowflake')),
    version      text        NOT NULL,
    endpoint_url text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, kind, version)
);

CREATE INDEX IF NOT EXISTS idx_engines_tenant_kind
    ON public.engines (tenant_id, kind);

-- ----- RLS: per-tenant SELECT, admin-only INSERT/UPDATE/DELETE --------
-- Mirror V0042 public.tenant_billing pattern: tenant role reads its own
-- rows via tenant_id::text predicate; mutations are admin-only (no policy
-- = default deny under FORCE RLS; admin_role bypasses via BYPASSRLS).
ALTER TABLE public.engines ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.engines FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'public' AND tablename = 'engines' AND policyname = 'engines_select') THEN
        CREATE POLICY engines_select ON public.engines
            FOR SELECT
            USING (tenant_id::text = current_setting('app.current_tenant', true));
    END IF;
END
$$ LANGUAGE plpgsql;

-- admin_role needs INSERT/UPDATE/DELETE for tenant onboarding scripts.
GRANT INSERT, UPDATE, DELETE ON public.engines TO admin_role;
GRANT SELECT ON public.engines TO neksur_app;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    tbl_ok     boolean;
    forced_ok  boolean;
    policy_ok  boolean;
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_tables
        WHERE schemaname = 'public' AND tablename = 'engines'
    ) INTO tbl_ok;
    IF tbl_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0070 verify: public.engines not created';
    END IF;

    SELECT (c.relrowsecurity AND c.relforcerowsecurity) INTO forced_ok
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'engines';
    IF forced_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0070 verify: FORCE RLS not enabled on public.engines';
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'engines' AND policyname = 'engines_select'
    ) INTO policy_ok;
    IF policy_ok IS NOT TRUE THEN
        RAISE EXCEPTION 'V0070 verify: engines_select policy missing';
    END IF;

    RAISE NOTICE 'V0070 OK — public.engines + FORCE RLS + select policy installed.';
END
$$ LANGUAGE plpgsql;
