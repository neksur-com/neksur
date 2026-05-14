-- =====================================================================
-- V0041 — Phase 0.5 public-tier shared tables + base Postgres roles.
--
-- Establishes the SaaS pilot infrastructure substrate:
--   * public.tenants                     — tenant registry (D-0.5.04 UUID v4 + D-0.5.20 lifecycle states)
--   * public.tenant_billing              — billing stub (D-0.5.12; activated at M7)
--   * public.design_partner_contracts    — manual revenue forecasting (ROADMAP §Phase 0.5 success-criterion #7)
--   * public.atlas_schema_revisions      — Atlas migration history (RESEARCH §Pitfall 9 — revisions_schema=public)
--   * Role: neksur_app  (LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION)
--   * Role: admin_role  (LOGIN NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION)
--
-- Idempotent: all `CREATE TABLE` use `IF NOT EXISTS`; both roles are
-- created inside guarded DO blocks that check `pg_roles` first. This
-- migration is safe to re-apply (Atlas will hash-lock it after first run).
--
-- D-0.5.18 numbering: continues Phase 0 V0030 — V0041 follows logically.
-- D-0.5.17 tool: Atlas versioned mode. Run via `cmd/migrate`.
--
-- Layer 3 RLS predicates land in V0042 (separate file so the table DDL
-- in V0041 can be verified independently before policy attachment).
--
-- Atlas runs each migration file in its own transaction (default
-- `tx-mode = file`), so we do NOT include an explicit BEGIN/COMMIT —
-- doing so triggers `pq: unexpected transaction status idle`.
-- =====================================================================

-- ----- public.tenants ------------------------------------------------
CREATE TABLE IF NOT EXISTS public.tenants (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    workos_org_id           text        UNIQUE NOT NULL
                                        CHECK (workos_org_id ~ '^org_[A-Z0-9]+$'),
    lifecycle_state         text        NOT NULL DEFAULT 'active'
                                        CHECK (lifecycle_state IN ('active','suspended','wind_down','deleted')),
    pool                    text        NOT NULL DEFAULT 'A'
                                        CHECK (pool IN ('A','B')),
    connection_dsn          text,
    onboarded_at            timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    last_audit_log_event    timestamptz
);

CREATE INDEX IF NOT EXISTS idx_tenants_workos_org_id
    ON public.tenants (workos_org_id);

CREATE INDEX IF NOT EXISTS idx_tenants_lifecycle_state
    ON public.tenants (lifecycle_state)
    WHERE lifecycle_state != 'deleted';

-- ----- public.tenant_billing -----------------------------------------
CREATE TABLE IF NOT EXISTS public.tenant_billing (
    tenant_id               uuid        PRIMARY KEY REFERENCES public.tenants(id) ON DELETE CASCADE,
    stripe_subscription_id  text,
    stripe_customer_id      text,
    tier                    text        NOT NULL DEFAULT 'design_partner'
                                        CHECK (tier IN ('design_partner','team','business','enterprise','multi_engine_enterprise')),
    current_period_start    timestamptz,
    current_period_end      timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_tenant_billing_stripe_sub
    ON public.tenant_billing (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

-- ----- public.design_partner_contracts -------------------------------
CREATE TABLE IF NOT EXISTS public.design_partner_contracts (
    tenant_id                    uuid           NOT NULL REFERENCES public.tenants(id) ON DELETE RESTRICT,
    started_at                   timestamptz    NOT NULL DEFAULT now(),
    expected_arr_post_ga         numeric(12,2),
    actual_paid                  numeric(12,2)  NOT NULL DEFAULT 0,
    commercial_credits_remaining numeric(12,2)  NOT NULL DEFAULT 0,
    notes                        text,
    PRIMARY KEY (tenant_id, started_at)
);

-- ----- public.atlas_schema_revisions ---------------------------------
-- Pre-created here with the minimal column set Atlas expects; Atlas may
-- extend (jsonb columns, etc.) on first apply. The IF NOT EXISTS makes
-- that safe. RESEARCH §Pitfall 9 — `revisions_schema = "public"` in
-- atlas.hcl ensures all tenant-loop runs share this one table.
CREATE TABLE IF NOT EXISTS public.atlas_schema_revisions (
    version          varchar(255) PRIMARY KEY,
    description      varchar(255),
    type             bigint       NOT NULL DEFAULT 2,
    applied          bigint       NOT NULL DEFAULT 0,
    total            bigint       NOT NULL DEFAULT 0,
    executed_at      timestamptz  NOT NULL DEFAULT now(),
    execution_time   bigint       NOT NULL DEFAULT 0,
    error            text,
    error_stmt       text,
    hash             varchar(255) NOT NULL DEFAULT '',
    partial_hashes   jsonb,
    operator_version varchar(255) NOT NULL DEFAULT ''
);

-- ----- Base Postgres roles -------------------------------------------
-- These roles are prerequisites for the GRANTs in V0043 (audit-log
-- INSERT-only for neksur_app) and for the per-tenant role-creation
-- pattern in Plan 04. Guarded so re-apply against Phase 0 testfixtures
-- (which already created `neksur_app` in createAppRole) is a no-op.

-- neksur_app: the application role. NOSUPERUSER + NOBYPASSRLS so all
-- three layers of D-0.5.03 isolation actually enforce.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'neksur_app') THEN
        CREATE ROLE neksur_app
            WITH LOGIN
                 NOSUPERUSER
                 NOBYPASSRLS
                 NOCREATEDB
                 NOCREATEROLE
                 NOREPLICATION;
    END IF;
END
$$ LANGUAGE plpgsql;

-- admin_role: cleanup + retention jobs (D-0.5.21 audit-log retention).
-- BYPASSRLS so admin queries see across tenants, but NOSUPERUSER so
-- the role is still ALTER TABLE OWNER-bound by Layer 2 GRANTs.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_role') THEN
        CREATE ROLE admin_role
            WITH LOGIN
                 NOSUPERUSER
                 BYPASSRLS
                 NOCREATEDB
                 NOCREATEROLE
                 NOREPLICATION;
    END IF;
END
$$ LANGUAGE plpgsql;

-- admin_role read-all for cross-tenant observability + INSERT/UPDATE/DELETE
-- on the public-tier tables (cleanup, lifecycle transitions, retention).
GRANT pg_read_all_data TO admin_role;
GRANT INSERT, UPDATE, DELETE ON public.tenants               TO admin_role;
GRANT INSERT, UPDATE, DELETE ON public.tenant_billing         TO admin_role;
GRANT INSERT, UPDATE, DELETE ON public.design_partner_contracts TO admin_role;

-- neksur_app needs USAGE on the schema + SELECT on the read-tier tables.
-- INSERT/UPDATE/DELETE on public.tenants is admin-only (Plan 04 provisioning
-- script runs under admin_role). The Plan 03 middleware reads `public.tenants`
-- via the SECURITY DEFINER lookup function declared in V0044.
GRANT USAGE ON SCHEMA public TO neksur_app;
GRANT SELECT ON public.tenants         TO neksur_app;
GRANT SELECT ON public.tenant_billing  TO neksur_app;

-- ----- Verify block --------------------------------------------------
-- Sanity-checks at migration end (Phase 0 V0001 + V0030 pattern).
DO $$
DECLARE
    table_count int;
    role_count  int;
BEGIN
    SELECT COUNT(*) INTO table_count
    FROM pg_tables
    WHERE schemaname = 'public'
      AND tablename IN ('tenants','tenant_billing','design_partner_contracts','atlas_schema_revisions');
    IF table_count <> 4 THEN
        RAISE EXCEPTION 'V0041 verify: expected 4 public-tier tables, found %', table_count;
    END IF;

    SELECT COUNT(*) INTO role_count
    FROM pg_roles
    WHERE rolname IN ('neksur_app','admin_role');
    IF role_count <> 2 THEN
        RAISE EXCEPTION 'V0041 verify: expected roles neksur_app + admin_role, found % matching role(s)', role_count;
    END IF;

    RAISE NOTICE 'V0041 OK — 4 public-tier tables + 2 base roles ready.';
END
$$ LANGUAGE plpgsql;
