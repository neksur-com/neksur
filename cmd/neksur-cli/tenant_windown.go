package main

// tenant_windown.go — `neksur-cli tenant wind-down` subcommand. Plan 07
// D-0.5.20 state-machine transition: active|suspended → wind_down.
//
// Semantics: 30-day post-cancellation read-only window starts now. The
// admin UI computes the drop-dead date as `updated_at + 30 days`; a
// Phase 1+ cron will auto-transition wind_down → deleted after the
// window expires (manual via `neksur-cli tenant delete --yes` until then).
//
// Reads continue; writes return 503 (same gateway semantics as suspended).
// The state distinction matters for billing + admin UI surfacing only.
//
// Exit codes:
//   0 — success (idempotent re-run: returns 0 with "already wind_down" message)
//   1 — runtime failure
//   2 — input validation failure

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantWindDown(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant wind-down", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant to wind down (required)")
		actor      = fs.String("actor", "", "operator identifier written into system_audit_log payload; default winddown@neksur.com")
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
		*actor = "winddown@neksur.com"
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

	if err := repo.WindDown(ctx, id, *actor); err != nil {
		if errors.Is(err, tenant.ErrTenantNotFound) {
			fmt.Fprintf(os.Stderr,
				"WARN tenant %s not in 'active' or 'suspended' state (already wind_down/deleted, or missing): %v\n",
				id, err)
			fmt.Fprintf(os.Stdout, "OK tenant wind-down: id=%s (no-op; not in active/suspended state)\n", id)
			return 0
		}
		fmt.Fprintf(os.Stderr, "repo.WindDown: %v\n", err)
		return 1
	}

	deleteDate := time.Now().UTC().Add(30 * 24 * time.Hour).Format("2006-01-02")
	fmt.Fprintf(os.Stdout, "OK tenant wind-down: id=%s state=wind_down actor=%s\n", id, *actor)
	fmt.Fprintf(os.Stdout, "  30-day countdown started; auto-delete on or after %s (D-0.5.20)\n", deleteDate)
	fmt.Fprintln(os.Stdout, "  Customer may still download audit_log + policies; no new writes accepted.")
	return 0
}
