-- =====================================================================
-- V0043 — public.system_audit_log + INSERT-only GRANT discipline (D-0.5.21).
--
-- T-0.5-audit-tamper mitigation: the application role (neksur_app) can
-- only INSERT into the audit log. UPDATE, DELETE, TRUNCATE are explicitly
-- REVOKEd. Retention cleanup runs as admin_role on a scheduled job.
--
-- Indexing: keyset pagination on (occurred_at DESC, id DESC) per ADR-001
-- §3.4 + PATTERNS.md shared-patterns line 814 — the admin UI paginates
-- the audit log via this index.
--
-- RLS is NOT enabled on system_audit_log: it's an admin-only table for
-- cross-tenant ops visibility. tenant_id-scoped audit lives in per-tenant
-- audit_log tables (Plan 04 V0050).
--
-- Atlas wraps each migration file in its own transaction (default
-- `tx-mode = file`); we omit the explicit BEGIN/COMMIT here.
-- =====================================================================

CREATE TABLE IF NOT EXISTS public.system_audit_log (
    id                   bigserial    PRIMARY KEY,
    occurred_at          timestamptz  NOT NULL DEFAULT now(),
    actor_user_id        text,
    actor_workos_org_id  text,
    target_tenant_id     uuid         REFERENCES public.tenants(id),
    event_type           text         NOT NULL,
    payload              jsonb        NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS idx_system_audit_log_keyset
    ON public.system_audit_log (occurred_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_system_audit_log_target_tenant
    ON public.system_audit_log (target_tenant_id)
    WHERE target_tenant_id IS NOT NULL;

-- ----- GRANT discipline (D-0.5.21) -----------------------------------
-- neksur_app: INSERT-only. The bigserial sequence is also USAGE-only
-- (USAGE allows nextval()), not SELECT (no sequence-reset attempts).
GRANT INSERT ON public.system_audit_log TO neksur_app;
GRANT USAGE  ON SEQUENCE public.system_audit_log_id_seq TO neksur_app;

-- Explicit REVOKE for UPDATE/DELETE/TRUNCATE — Postgres default for a
-- newly-created table is "no privileges granted to non-owner" so these
-- REVOKEs are belt-and-suspenders against future DEFAULT PRIVILEGE
-- statements that might quietly grant them. The grep gate in Task 1
-- explicitly asserts the REVOKE UPDATE, DELETE clause.
REVOKE UPDATE, DELETE ON public.system_audit_log FROM neksur_app;
REVOKE TRUNCATE       ON public.system_audit_log FROM neksur_app;

-- admin_role: full control for retention cleanup. Inherits via
-- `pg_read_all_data` for SELECT; explicit DML for cleanup jobs.
GRANT INSERT, UPDATE, DELETE, TRUNCATE ON public.system_audit_log TO admin_role;

-- ----- Verify block --------------------------------------------------
DO $$
DECLARE
    has_insert  boolean;
    has_update  boolean;
    has_delete  boolean;
BEGIN
    SELECT bool_or(privilege_type = 'INSERT') INTO has_insert
    FROM information_schema.role_table_grants
    WHERE grantee   = 'neksur_app'
      AND table_schema = 'public'
      AND table_name   = 'system_audit_log';

    SELECT bool_or(privilege_type = 'UPDATE') INTO has_update
    FROM information_schema.role_table_grants
    WHERE grantee   = 'neksur_app'
      AND table_schema = 'public'
      AND table_name   = 'system_audit_log';

    SELECT bool_or(privilege_type = 'DELETE') INTO has_delete
    FROM information_schema.role_table_grants
    WHERE grantee   = 'neksur_app'
      AND table_schema = 'public'
      AND table_name   = 'system_audit_log';

    IF has_insert IS NOT TRUE THEN
        RAISE EXCEPTION 'V0043 verify: neksur_app missing INSERT on public.system_audit_log';
    END IF;
    IF has_update IS TRUE THEN
        RAISE EXCEPTION 'V0043 verify: neksur_app should NOT have UPDATE on public.system_audit_log (T-0.5-audit-tamper)';
    END IF;
    IF has_delete IS TRUE THEN
        RAISE EXCEPTION 'V0043 verify: neksur_app should NOT have DELETE on public.system_audit_log (T-0.5-audit-tamper)';
    END IF;

    RAISE NOTICE 'V0043 OK — system_audit_log INSERT-only for neksur_app, full DML for admin_role.';
END
$$ LANGUAGE plpgsql;
