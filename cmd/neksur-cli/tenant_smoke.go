package main

// tenant_smoke.go — `neksur-cli tenant smoke` subcommand. Step (k) of
// D-0.5.19. Three smoke checks run against the freshly-provisioned
// tenant:
//
//   1. GatewayCommitAuditEdge — INSERT + SELECT-back on audit_log via
//                               WithTenantTx (Layer 1+2+3 isolation).
//   2. PolicyFetch           — SELECT count(*) FROM policies via
//                               WithTenantTx.
//   3. CrossTenantProbe      — attempt to read a probe tenant's
//                               audit_log; expect 42501 (or 42P01 if
//                               schema not yet populated).
//
// The probe tenant defaults to the all-zeros UUID — if --probe-tenant-uuid
// is empty AND no NEKSUR_SMOKE_PROBE_TENANT env var is set, the cross-
// tenant probe is SKIPPED (operator-driven probe is opt-in per D-0.5.19
// step (k) note about "negative-test tenant").
//
// On success: writes a row to public.system_audit_log
// (event_type='tenant.onboarded') and prints "OK tenant smoke".

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantSmoke(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant smoke", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant (required)")
		probeUUID  = fs.String("probe-tenant-uuid", "", "UUID v4 of an OTHER tenant to use for the cross-tenant probe (optional)")
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

	dsn, code := requireEnv("DATABASE_URL")
	if code != 0 {
		return code
	}

	id, err := uuid.Parse(*tenantUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uuid.Parse: %v\n", err)
		return 2
	}

	// Probe tenant — flag wins, then env var, then skip.
	probeStr := *probeUUID
	if probeStr == "" {
		probeStr = envOrEmpty("NEKSUR_SMOKE_PROBE_TENANT")
	}
	var probe uuid.UUID
	if probeStr != "" {
		if err := tenant.ValidateUUIDv4(probeStr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		probe, err = uuid.Parse(probeStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "uuid.Parse probe: %v\n", err)
			return 2
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgxpool.New: %v\n", err)
		return 1
	}
	defer pool.Close()

	gc, err := graph.NewGraphClient(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "graph.NewGraphClient: %v\n", err)
		return 1
	}
	defer gc.Close()

	repo := tenant.NewRepo(pool)
	provisioner := tenant.NewProvisioner(gc, pool, repo, dsn, envOrEmpty("TF_DIR"), envOrEmpty("PRIVATE_CA_ARN"))

	if err := provisioner.RunSmoke(ctx, id, probe); err != nil {
		fmt.Fprintf(os.Stderr, "RunSmoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "OK tenant smoke: tenant=%s probe=%s\n", id, probeStr)
	return 0
}
