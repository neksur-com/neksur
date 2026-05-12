package graph

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SetTenantContext sets the `app.current_tenant` GUC on the transaction
// represented by tx so subsequent RLS-policy evaluations (per V0030) see
// the requested tenant ID via current_setting('app.current_tenant').
//
// Implementation note (Deviation #6 from Wave 1):
// Postgres's `SET LOCAL <name> = $1` is a parse error — `SET` is a
// utility statement that takes a literal, not a bind parameter. The
// equivalent function form `set_config(name, value, true)` accepts
// proper bind parameters. `is_local=true` matches `SET LOCAL` semantics
// — the value is scoped to the surrounding transaction and cleared by
// COMMIT or ROLLBACK.
//
// tenantID is treated as opaque text and never interpolated into the
// SQL string; it goes through pgx's positional bind ($1) so even a
// caller-controlled tenant string (which should not happen in
// production, but is the safe contract for defence-in-depth) cannot
// inject SQL.
func SetTenantContext(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		return fmt.Errorf("graph: SetTenantContext set_config: %w", err)
	}
	return nil
}

// SetTenantContextOnConn is the connection-scoped variant of
// SetTenantContext: used by tests / advanced callers that need to set
// the GUC on an arbitrary pgx.Conn (e.g., from a fixture connection
// that owns its own transaction lifecycle). Production code should
// prefer GraphClient.ExecuteInTenant which wires SetTenantContext +
// ResetSession + transaction lifecycle correctly.
//
// Like SetTenantContext, the tenant ID is bound, not concatenated.
func SetTenantContextOnConn(ctx context.Context, conn *pgx.Conn, tenantID string) error {
	if _, err := conn.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		return fmt.Errorf("graph: SetTenantContextOnConn set_config: %w", err)
	}
	return nil
}

// ResetSession runs `DISCARD ALL` against conn. This clears all
// session-level state — prepared statements, temp tables, cursors,
// advisory locks, AND custom session GUCs like `app.current_tenant`.
// It is the connection-pool reset hook that prevents Pitfall 5:
// a returned-then-reacquired connection leaking the previous holder's
// tenant context into a new transaction.
//
// `DISCARD ALL` is a Postgres utility statement and takes no
// parameters, so direct execution is safe — no user input flows in.
//
// Verified end-to-end by tests/security/rls_isolation_test.go::
// TestSessionVarBleed.
func ResetSession(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, "DISCARD ALL"); err != nil {
		return fmt.Errorf("graph: ResetSession DISCARD ALL: %w", err)
	}
	return nil
}
