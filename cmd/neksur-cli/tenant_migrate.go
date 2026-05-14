package main

// tenant_migrate.go — `neksur-cli tenant migrate` subcommand. Steps
// (c)+(d) (bootstrap-schema) or (e) (apply-versioned) of D-0.5.19.
//
//   --step bootstrap-schema   = AGE create_graph + Postgres role + GRANTs
//   --step apply-versioned    = Atlas tenant-loop apply V0050+V0051+V0052
//                               + REVOKE UPDATE/DELETE on audit_log
//                               (T-0.5-audit-tamper)
//
// Exit codes match tenant_create.

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

func runTenantMigrate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant migrate", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant (required)")
		step       = fs.String("step", "", "one of: bootstrap-schema | apply-versioned (required)")
	)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenantUUID == "" || *step == "" {
		fs.Usage()
		return 2
	}
	if err := tenant.ValidateUUIDv4(*tenantUUID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch *step {
	case "bootstrap-schema", "apply-versioned":
	default:
		fmt.Fprintf(os.Stderr, "invalid --step %q (expected bootstrap-schema | apply-versioned)\n", *step)
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

	switch *step {
	case "bootstrap-schema":
		if err := provisioner.CreateGraph(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "CreateGraph: %v\n", err)
			return 1
		}
		if err := provisioner.CreateRole(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "CreateRole: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "OK tenant migrate bootstrap-schema: %s\n", tenant.SchemaName(id))
	case "apply-versioned":
		if err := provisioner.ApplyTenantMigrations(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "ApplyTenantMigrations: %v\n", err)
			return 1
		}
		// After Atlas creates audit_log, run the REVOKE step
		// (T-0.5-audit-tamper). This is a no-op if audit_log
		// didn't get created (e.g., the role's default privileges
		// didn't include UPDATE).
		if err := provisioner.RevokeAuditLogWrites(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "RevokeAuditLogWrites: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "OK tenant migrate apply-versioned: %s\n", tenant.SchemaName(id))
	}
	return 0
}
