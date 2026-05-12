package graph

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnboundedTraversal is the sentinel error returned by Cypher (and by
// ValidateTraversalDepth directly) when the caller-supplied Cypher
// statement contains a forbidden unbounded variable-length traversal
// pattern: bare `*`, lower-only `*N..`, or no-bounds `*..`. Per D-001.08
// the gateway clamps traversals at the application boundary so that even
// an AGE planner regression cannot turn a missed depth cap into a
// tenant-wide DoS (T-0-DOS).
var ErrUnboundedTraversal = errors.New("graph: unbounded traversal not allowed (D-001.08)")

// unboundedVLPRegex catches the three forbidden VLP shapes. The
// alternation anchors with a terminator lookahead (`]`, whitespace, `)`,
// `|`, `,`, or end-of-input) so that bounded forms like `*1..5` are NOT
// matched (the `5` after `..` is not a terminator).
//
//   - `\*(?:\s|\]|\)|,|\||$)`   → bare `*` followed by terminator
//   - `\*\d+\.\.(?:\s|\]|\)|,|\||$)` → lower-only `*N..`
//   - `\*\.\.(?:\s|\]|\)|,|\||$)`    → no-bounds `*..`
//
// Go's regexp engine is RE2 (no backreferences, no lookahead). We emulate
// the lookahead by matching the terminator as a capture and accepting
// end-of-string as a regex alternative via `(?:$|...)`. This works
// correctly across newlines because the regex is anchored on the
// asterisk position, not the start of line.
var unboundedVLPRegex = regexp.MustCompile(
	`\*(?:(?:\s|\]|\)|,|\||$)|\d+\.\.(?:\s|\]|\)|,|\||$)|\.\.(?:\s|\]|\)|,|\||$))`,
)

// ValidateTraversalDepth returns ErrUnboundedTraversal if stmt contains a
// forbidden unbounded variable-length traversal pattern. It is a pure
// function — no database connection required — so it can be unit-tested
// without spinning up a Postgres+AGE container.
//
// Bounded forms (`*N`, `*N..M`, `*..M`) are accepted (return nil). The
// pre-parser does NOT enforce the upper-bound clamp (M ≤ 5 per D-001.08);
// that responsibility lies with downstream tests / Phase 5 ADR-004
// hardening. The Phase 0 floor is "no unbounded".
func ValidateTraversalDepth(stmt string) error {
	if unboundedVLPRegex.MatchString(stmt) {
		// Find the actual offending substring for a more useful error message.
		match := unboundedVLPRegex.FindString(stmt)
		return fmt.Errorf("%w: offending substring %q; use *N..M (default 1..3, max 1..5)",
			ErrUnboundedTraversal, match)
	}
	return nil
}

// GraphClient is the thin Postgres + AGE wrapper used by every Neksur
// application service that needs to talk Cypher. It owns a pgxpool.Pool
// configured with an AfterConnect hook that runs `LOAD 'age'` and primes
// the search_path so subsequent queries can call `cypher(...)` directly.
//
// Construct with NewGraphClient; close with Close. The struct embeds a
// pool, not a connection — callers should not assume any per-method
// affinity. The two surfaces that DO require connection affinity
// (transaction boundaries and SET LOCAL semantics) are handled inside
// ExecuteInTenant which acquires a connection, runs the user callback,
// and releases.
type GraphClient struct {
	pool *pgxpool.Pool
}

// NewGraphClient constructs a GraphClient against the given Postgres DSN.
// The pool's AfterConnect hook runs `LOAD 'age'` + sets search_path so
// every acquired connection is ready for AGE Cypher.
//
// AGE requires `LOAD 'age'` once per session (the AGE C-extension's
// session-state initialiser). pgx pools reuse connections, so making
// this an AfterConnect hook is the natural place — every connection in
// the pool is guaranteed to have it run exactly once at acquisition.
//
// The DSN follows libpq conventions; see jackc/pgx documentation.
func NewGraphClient(ctx context.Context, dsn string) (*GraphClient, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("graph: parse DSN: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
			return fmt.Errorf("graph: LOAD 'age' failed: %w", err)
		}
		// `SET search_path = ag_catalog, "$user", public` — the standard
		// AGE prelude. Quoted "$user" matches Postgres's idiomatic
		// per-user schema lookup; `ag_catalog` first so `cypher(...)`
		// and friends resolve without explicit qualification.
		if _, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
			return fmt.Errorf("graph: SET search_path failed: %w", err)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("graph: pgxpool.NewWithConfig: %w", err)
	}
	return &GraphClient{pool: pool}, nil
}

// Close drains and closes the underlying pool. Safe to call multiple
// times; subsequent calls are no-ops on a closed pool. Typically
// deferred from main / test cleanup.
func (g *GraphClient) Close() {
	if g.pool != nil {
		g.pool.Close()
	}
}

// Pool exposes the underlying pgxpool.Pool for advanced callers (e.g.,
// integration tests that need to run raw catalog SELECTs). Production
// services should prefer Cypher and ExecuteInTenant.
func (g *GraphClient) Pool() *pgxpool.Pool {
	return g.pool
}

// Cypher submits the given Cypher statement to the named AGE graph and
// returns the resulting pgx.Rows. Caller closes the rows.
//
// Workflow:
//  1. ValidateTraversalDepth(stmt) — pre-parser depth cap (D-001.08).
//     Returns ErrUnboundedTraversal on bare `*`, `*N..`, `*..`.
//  2. Wrap the statement in the AGE call shape:
//     SELECT * FROM cypher('<graph>', $$ <stmt> $$) AS (result agtype)
//     Note: the graph name is interpolated as a SQL string literal here
//     (it is never caller-supplied; it's the application's hard-coded
//     graph name, typically "neksur"). The Cypher statement itself is
//     splice'd into the dollar-quoted block, and any caller parameters
//     in args are passed through pgx's positional binding.
//  3. Acquire a connection from the pool (AfterConnect already loaded
//     AGE), execute, return Rows.
//
// args are forwarded to pgx's positional binder ($1, $2, ...). Use them
// for any caller-supplied values inside the Cypher statement. Labels are
// NOT parameterisable in Cypher; if the caller has a dynamic label name,
// they MUST validate it via IsAllowedLabel before splicing into stmt.
//
// Phase 0 baseline cypher hardening; ADR-004 (Phase 5) layers full
// MCP-aware hardening contract per D-OQ.03.
func (g *GraphClient) Cypher(
	ctx context.Context,
	graph string,
	stmt string,
	args ...any,
) (pgx.Rows, error) {
	if err := ValidateTraversalDepth(stmt); err != nil {
		return nil, err
	}
	// Wrap in AGE call shape. The graph name is application-fixed
	// (typically "neksur") and never derived from user input; we use
	// fmt.Sprintf with %q to safely quote it as an SQL string literal.
	// The Cypher statement body is splice'd into the dollar-quoted
	// block (which is what AGE expects as the second argument to
	// cypher()). Pgx's positional binding handles the actual VALUES
	// in args.
	q := fmt.Sprintf(
		"SELECT * FROM cypher(%s, $$ %s $$) AS (result ag_catalog.agtype)",
		quoteSQLString(graph), stmt,
	)
	return g.pool.Query(ctx, q, args...)
}

// ExecuteInTenant runs fn inside a transaction bound to tenantID. The
// transaction calls set_config('app.current_tenant', $1, true) via the
// SetTenantContext helper so Postgres RLS policies in V0030 scope to
// this tenant. On any path out of fn (success or error) the transaction
// is committed-or-rolled-back, then ResetSession (DISCARD ALL) is run
// against the connection BEFORE it returns to the pool so the next
// holder of the connection cannot observe leftover tenant state.
//
// The contract on fn:
//   - It receives a pgx.Tx, not a *pgx.Conn — guaranteeing it cannot
//     escape transaction scope without surfacing an error.
//   - It must not retain the tx pointer past return.
//   - It must NOT commit or rollback itself; ExecuteInTenant owns the
//     transaction lifecycle.
func (g *GraphClient) ExecuteInTenant(
	ctx context.Context,
	tenantID string,
	fn func(ctx context.Context, tx pgx.Tx) error,
) (retErr error) {
	conn, err := g.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("graph: acquire connection: %w", err)
	}
	defer func() {
		// Pool reset: even if a panic propagates (not expected), we run
		// DISCARD ALL so the connection's app.current_tenant cannot
		// leak to the next holder (T-0-SESS / Pitfall 5).
		if resetErr := ResetSession(ctx, conn.Conn()); resetErr != nil && retErr == nil {
			retErr = fmt.Errorf("graph: reset session on release: %w", resetErr)
		}
		conn.Release()
	}()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("graph: begin tx: %w", err)
	}
	// Best-effort rollback if we don't reach the commit branch.
	rolled := false
	defer func() {
		if !rolled && retErr != nil {
			// Errors during rollback after a primary error are
			// secondary; don't mask the original.
			_ = tx.Rollback(ctx)
		}
	}()

	if err := SetTenantContext(ctx, tx, tenantID); err != nil {
		return fmt.Errorf("graph: set tenant context: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("graph: commit tx: %w", err)
	}
	rolled = true
	return nil
}

// quoteSQLString returns s wrapped in SQL single quotes with any embedded
// single quotes doubled — the safe way to embed an application-fixed
// identifier in an SQL string literal where pgx binding is not
// applicable (e.g., as the first argument to cypher()). The graph name
// is hard-coded in the application; this helper exists for defence-in-
// depth.
func quoteSQLString(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}
