package graph

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithBeforeAcquireDiscardAll mutates cfg so that every pool acquisition
// runs `DISCARD ALL` against the connection BEFORE handing it to the
// caller. On Exec failure the hook returns false, which tells pgxpool
// to destroy the connection and acquire/establish a fresh one rather
// than risk handing a tainted conn to the caller.
//
// Why this hook is the #1 landmine of Phase 0.5
// ---------------------------------------------
// pgx connections are shared resources. Without a reset, session-level
// state (search_path, custom GUCs like app.current_tenant, the role
// from a SET ROLE) persists across pool acquisitions. A request that
// uses `SET LOCAL` inside a transaction is safe at commit/rollback,
// but a request that crashed BEFORE the transaction (or that used the
// non-LOCAL form) leaks state forever. The next request, for a
// DIFFERENT tenant, then sees that state — a cross-tenant data leak.
//
// RESEARCH §Summary line 13 + Pitfall 1 calls this out: "pgx's
// AfterRelease hook runs ASYNC with NO CONTEXT (per jackc/pgx#1666)
// so it cannot be used reliably for session reset. The correct hook
// is BeforeAcquire, which runs synchronously and can fail the
// acquisition." This function wires that hook.
//
// Defence-in-depth: even though internal/tenant.WithTenantTx uses
// `SET LOCAL` exclusively (so values clear at COMMIT/ROLLBACK), we
// run DISCARD ALL on every acquire to handle:
//   - Code paths outside WithTenantTx that forget to use SET LOCAL.
//   - Code paths that PANIC mid-transaction (the deferred Rollback
//     still fires, which DOES clear SET LOCAL values, but a malicious
//     internal handler could SET app.current_tenant outside a tx and
//     hand the conn back to the pool dirty).
//   - Future Plan 04+ paths that haven't been written yet.
//
// Caveat (RESEARCH Pitfall 10 line 1187): `SET ROLE` to a NOLOGIN
// tenant role MAY block `DISCARD ALL`. Mitigation: WithTenantTx uses
// `SET LOCAL ROLE` (auto-reverts at COMMIT), and BeforeAcquire runs at
// pool acquire (where current_user == session_user, before any SET
// ROLE has fired). The TestSessionBleed integration test
// (tests/integration/session_bleed_test.go) proves this end-to-end
// against a real Postgres+AGE container.
//
// Why mutate rather than replace? Callers compose multiple pool config
// concerns (AfterConnect from Phase 0's graph.NewGraphClient, custom
// MaxConns / MinConns for Pool B, etc.). A mutator lets each concern
// own its piece of the config without trying to re-export every Phase 0
// option through this constructor.
//
// Used by:
//   - cmd/neksur-server/main.go            — the production app pool
//   - tests/integration/session_bleed_test.go  — the canonical proof
//   - (future) Plan 04 provisioning pool
//   - (future) Plan 06 Pool B pool
func WithBeforeAcquireDiscardAll(cfg *pgxpool.Config) {
	cfg.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
		if _, err := conn.Exec(ctx, "DISCARD ALL"); err != nil {
			// Return false → pgxpool destroys this conn and either
			// creates a new one or returns the acquire error to the
			// caller. Better to fail the acquire than to hand back a
			// tainted connection.
			return false
		}
		return true
	}
}
