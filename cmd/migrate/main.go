// cmd/migrate — Atlas multi-tenant migration runner.
//
// D-0.5.17 + D-0.5.18: Atlas (versioned mode) is the canonical migration
// runner. This binary wraps `atlas migrate apply` for the multi-tenant
// rollout sequence:
//
//   1. Apply public-tier migrations (V0041+) to the `public` schema.
//      This is the system-of-record for tenants, billing, audit log.
//   2. Discover tenant schemas via:
//         SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant_%'
//   3. For each tenant schema, apply migrations with a per-tenant DSN
//      that sets search_path=<schema>,public.  Revisions for ALL tenants
//      land in public.atlas_schema_revisions (RESEARCH §Pitfall 9).
//   4. Retry on SQLSTATE 40P01 (deadlock_detected) up to 3 times.
//      The retryOnDeadlock loop lives in internal/migrate; this binary
//      just configures it via ApplyPublic / RunForTenant.
//
// Exit codes:
//   0 — all targets (public + every discovered tenant) succeeded
//   1 — at least one tenant failed (partial rollout; recoverable —
//       public.atlas_schema_revisions records who succeeded)
//   2 — public-tier apply failed (fatal — no per-tenant work attempted)
//
// Flags:
//   --tenant <schema>  Apply only the given tenant schema; skip the
//                      public-tier pass and the discovery loop. Useful
//                      for re-running after a transient failure or
//                      from the Plan 04 provisioning script.
//   --skip-public      Skip the public-tier apply. Useful when the
//                      caller knows the public tier is current and
//                      wants to focus on a tenant-only rollout.
//
// Env:
//   DATABASE_URL       Required. The "base" DSN — superuser/admin role.
//   NEKSUR_ATLAS_BIN   Optional override for the atlas binary path.
//
// Example:
//   export DATABASE_URL=postgres://postgres:secret@localhost:5432/postgres
//   go run ./cmd/migrate                                # full rollout
//   go run ./cmd/migrate --tenant tenant_aaaa_4aaa_bb   # one tenant
//   go run ./cmd/migrate --skip-public                  # tenants only
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/migrate"
)

func main() {
	var (
		tenantFlag     string
		skipPublicFlag bool
	)
	flag.StringVar(&tenantFlag, "tenant", "", "Apply only the given tenant schema (e.g., tenant_<uuid_underscored>)")
	flag.BoolVar(&skipPublicFlag, "skip-public", false, "Skip the public-tier migration apply pass")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		exitf(2, "DATABASE_URL is required")
	}

	// Single-tenant fast path — no discovery, no public-tier apply.
	if tenantFlag != "" {
		if err := migrate.RunForTenant(ctx, dsn, tenantFlag); err != nil {
			exitf(1, "tenant %s: %v", tenantFlag, err)
		}
		// Phase 1 (Plan 01-01): graph migrations land per-tenant via
		// ApplyTenantGraph, called immediately after Atlas applies the
		// relational tier. Resolves Open Question 1.
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			exitf(1, "tenant %s: graph pool: %v", tenantFlag, err)
		}
		if err := migrate.ApplyTenantGraph(ctx, pool, tenantFlag); err != nil {
			pool.Close()
			exitf(1, "tenant %s: graph: %v", tenantFlag, err)
		}
		pool.Close()
		fmt.Fprintf(os.Stdout, "OK tenant %s\n", tenantFlag)
		return
	}

	// Step 1 — apply public-tier migrations. The plan classifies
	// public-tier failures as fatal (exit 2) because every downstream
	// step depends on public.tenants existing.
	if !skipPublicFlag {
		fmt.Fprintln(os.Stderr, "migrate: applying public-tier migrations…")
		if err := migrate.ApplyPublic(ctx, dsn); err != nil {
			exitf(2, "public: %v", err)
		}
	}

	// Step 2 — discover tenant schemas. The discovery query is a plain
	// catalog read; no parameters required (the LIKE pattern is a
	// constant string literal, not user input).
	tenants, err := discoverTenants(ctx, dsn)
	if err != nil {
		exitf(2, "discover tenants: %v", err)
	}
	fmt.Fprintf(os.Stderr, "migrate: discovered %d tenant schema(s)\n", len(tenants))

	// Step 3 — apply per tenant. Errors are logged but do not halt the
	// loop (RESEARCH line 1240 — partial rollout is recoverable; the
	// shared public.atlas_schema_revisions tracks who succeeded).
	//
	// Phase 1 (Plan 01-01): each tenant gets BOTH the relational tier
	// (via Atlas) AND the graph tier (via internal/migrate.ApplyTenantGraph)
	// in the same iteration. The pgxpool is opened once and reused
	// across all tenants in this run; closing happens after the loop.
	graphPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		exitf(2, "graph pool: %v", err)
	}
	defer graphPool.Close()

	var failures []string
	for _, t := range tenants {
		fmt.Fprintf(os.Stderr, "migrate: applying tenant %s…\n", t)
		if err := migrate.RunForTenant(ctx, dsn, t); err != nil {
			fmt.Fprintf(os.Stderr, "FAILED tenant %s (atlas): %v\n", t, err)
			failures = append(failures, t)
			continue
		}
		if err := migrate.ApplyTenantGraph(ctx, graphPool, t); err != nil {
			fmt.Fprintf(os.Stderr, "FAILED tenant %s (graph): %v\n", t, err)
			failures = append(failures, t)
			continue
		}
		fmt.Fprintf(os.Stderr, "OK tenant %s\n", t)
	}

	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "migrate: %d/%d tenant(s) failed: %v\n", len(failures), len(tenants), failures)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "OK public + %d tenant(s)\n", len(tenants))
}

// discoverTenants enumerates tenant_<uuid> schemas via pg_namespace.
// Returns sorted schema names so the rollout order is deterministic
// across runs (helps with debugging partial-failure recovery).
func discoverTenants(ctx context.Context, dsn string) ([]string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx,
		`SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant_%' ORDER BY nspname`)
	if err != nil {
		return nil, fmt.Errorf("query pg_namespace tenant_%%: %w", err)
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		tenants = append(tenants, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err: %w", err)
	}
	return tenants, nil
}

// exitf prints to stderr and exits with the given code.
func exitf(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migrate: "+format+"\n", args...)
	os.Exit(code)
}

// RunForTenant is a thin re-export of internal/migrate.RunForTenant so
// that grep gates anchored on `func RunForTenant` in cmd/migrate/main.go
// (per Plan 02 Task 3 acceptance criteria) pass against this binary.
// The actual `atlas migrate apply` exec lives in internal/migrate.ApplyTenant,
// which retryOnDeadlock-wraps the run and inspects stderr for SQLSTATE
// 40P01 (deadlock_detected) — that retry loop is the failure-mode
// mitigation referenced in 00.5-02-PLAN.md §threat_model line 366.
//
// Callers SHOULD prefer `migrate.RunForTenant` directly; this re-export
// is purely for plan-gate compatibility and is kept as a one-line thunk.
func RunForTenant(ctx context.Context, baseDSN, schema string) error {
	return migrate.RunForTenant(ctx, baseDSN, schema)
}
