package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithTenantTx is the canonical per-request transaction helper for
// Phase 0.5. Every API handler that talks to Postgres goes through this
// function: it pulls the tenant ID from ctx, opens a transaction on
// pool, applies the three SET LOCAL layers, then runs fn.
//
// The three layers are exactly D-0.5.03:
//
//  1. Layer 1 — `SET LOCAL search_path TO <schema>, public`
//     Tenant data lives in tenant_<uuid_underscored>.* schemas; this
//     puts the tenant schema first so unqualified label/index references
//     resolve there. `public` second so cross-tenant relational tables
//     (public.tenants etc.) remain reachable.
//
//  2. Layer 2 — `SET LOCAL ROLE <schema>_role`
//     The tenant role has GRANTs only on its own schema (created at
//     provisioning by Plan 04). Even if Layer 1 were bypassed, a query
//     against another tenant's schema would fail on Layer 2 with
//     "permission denied".
//
//  3. Layer 3 — `SELECT set_config('app.current_tenant', $1, true)`
//     Sets the GUC that public.tenants / public.tenant_billing RLS
//     policies (V0042) read via current_setting(). Belt-and-suspenders
//     for the shared public-tier tables.
//
// All three use the SET LOCAL form (Layer 3 via set_config(.., is_local=true))
// so the values clear at COMMIT or ROLLBACK; they do not leak past the
// transaction. The pgxpool BeforeAcquire DISCARD ALL hook (graph/pool.go)
// is the second line of defence — even if a transaction is rolled back
// outside our control, the next acquisition will DISCARD ALL.
//
// IMPORTANT: `SET LOCAL <name> = $1` is a parse error in Postgres
// (SET is a utility statement, not a query — it cannot take bind
// parameters). Layer 3 therefore uses the function form set_config(...);
// Layer 1 and Layer 2 take identifier names which can never be bound
// (Postgres has no IDENTIFIER parameter type), so we identifier-quote
// via pgx.Identifier{...}.Sanitize() and splice into the SQL.
//
// Sanitize() is the canonical pgx way to safely quote an identifier
// (it doubles embedded double-quotes). Combined with the schemaname /
// rolename being computed from a parsed uuid.UUID (which the type
// constructor already validated to be hex-only), this is injection-safe.
//
// fn receives the tx and MUST NOT commit/rollback itself. WithTenantTx
// commits on fn returning nil; rolls back on any error.
func WithTenantTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (retErr error) {
	tenantID, ok := IDFromContext(ctx)
	if !ok {
		return ErrTenantNotInContext
	}
	schemaName := SchemaName(tenantID)
	roleName := RoleName(tenantID)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("tenant: begintx: %w", err)
	}
	// Best-effort rollback: if fn or Commit fails, the deferred call
	// rolls the tx back; if Commit succeeds, Rollback on a committed tx
	// is a no-op per pgx contract.
	defer func() {
		if retErr != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// Layer 1: search_path
	stmtL1 := fmt.Sprintf("SET LOCAL search_path TO %s, public",
		(pgx.Identifier{schemaName}).Sanitize())
	if _, err := tx.Exec(ctx, stmtL1); err != nil {
		return fmt.Errorf("tenant: layer1 search_path: %w", err)
	}
	// Layer 2: SET ROLE
	stmtL2 := fmt.Sprintf("SET LOCAL ROLE %s",
		(pgx.Identifier{roleName}).Sanitize())
	if _, err := tx.Exec(ctx, stmtL2); err != nil {
		return fmt.Errorf("tenant: layer2 set role: %w", err)
	}
	// Layer 3: app.current_tenant GUC. Use set_config(..., is_local=true)
	// — the only form that accepts a bind parameter. `SET LOCAL
	// app.current_tenant = $1` is a parse error per Phase 0 deviation #6
	// (CLAUDE.md line 97).
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.current_tenant', $1, true)",
		tenantID.String(),
	); err != nil {
		return fmt.Errorf("tenant: layer3 set_config: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: commit: %w", err)
	}
	return nil
}
