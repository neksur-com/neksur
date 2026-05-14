package main

// tenant_suspend.go — `neksur-cli tenant suspend` subcommand. Plan 07
// D-0.5.20 state-machine transition: active → suspended.
//
// Semantics: an active tenant becomes read-only at the gateway layer
// (D-0.5.20: gateway returns 503 on commit; reads still work). The
// underlying lifecycle.Suspend method:
//   1. UPDATEs public.tenants.lifecycle_state from 'active' → 'suspended'
//      atomically (WHERE lifecycle_state = 'active'); 0 rows updated →
//      ErrTenantNotFound (already suspended / deleted / wrong state).
//   2. INSERTs a public.system_audit_log row with event_type='tenant.suspended'
//      so the lifecycle transition is queryable.
//
// Inputs flow through tenant.ValidateUUIDv4 BEFORE any DB call
// (T-0.5-prov-injection mitigation; same idiom as tenant_create.go).
//
// Exit codes:
//   0 — success (idempotent re-run: returns 0 with "already suspended" message)
//   1 — runtime failure (DB connection error, etc.)
//   2 — input validation failure

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantSuspend(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant suspend", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant to suspend (required)")
		actor      = fs.String("actor", "", "operator identifier written into system_audit_log payload; default suspend@neksur.com")
	)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenantUUID == "" {
		fs.Usage()
		return 2
	}
	if err := tenant.ValidateUUIDv4(*tenantUUID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *actor == "" {
		*actor = "suspend@neksur.com"
	}

	id, err := uuid.Parse(*tenantUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uuid.Parse: %v\n", err)
		return 2
	}

	dsn, code := requireEnv("DATABASE_URL")
	if code != 0 {
		return code
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgxpool.New: %v\n", err)
		return 1
	}
	defer pool.Close()

	repo := tenant.NewRepo(pool)

	if err := repo.Suspend(ctx, id, *actor); err != nil {
		if errors.Is(err, tenant.ErrTenantNotFound) {
			fmt.Fprintf(os.Stderr,
				"WARN tenant %s not in 'active' state (already suspended/deleted, or missing): %v\n",
				id, err)
			// Treat "already in target state" as idempotent success.
			// Operator gets a non-zero-message warning on stderr but the
			// exit code stays 0 so re-running scripts are idempotent.
			fmt.Fprintf(os.Stdout, "OK tenant suspend: id=%s (no-op; not in active state)\n", id)
			return 0
		}
		fmt.Fprintf(os.Stderr, "repo.Suspend: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "OK tenant suspend: id=%s state=suspended actor=%s\n", id, *actor)
	fmt.Fprintln(os.Stdout, "  Gateway will now return 503 on writes for this tenant; reads continue (D-0.5.20).")
	return 0
}
