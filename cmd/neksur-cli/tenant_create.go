package main

// tenant_create.go — `neksur-cli tenant create` subcommand. Step (a)+(b)
// of D-0.5.19: writes the public.tenants row (ON CONFLICT DO NOTHING),
// creates the AGE graph, and creates the per-tenant Postgres role.
//
// Inputs flow through ValidateUUIDv4 + ValidateWorkOSOrgID BEFORE any
// psql operation — T-0.5-prov-injection mitigation.
//
// Exit codes:
//   0 — success (idempotent re-runs also return 0)
//   1 — runtime failure (DB connection error, etc.)
//   2 — input validation failure

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

func runTenantCreate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant (required)")
		workosOrg  = fs.String("workos-org", "", "WorkOS organization ID, e.g., org_ABC123 (required)")
	)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenantUUID == "" || *workosOrg == "" {
		fs.Usage()
		return 2
	}

	// Validate inputs BEFORE any DB call (T-0.5-prov-injection).
	if err := tenant.ValidateUUIDv4(*tenantUUID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := tenant.ValidateWorkOSOrgID(*workosOrg); err != nil {
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

	// Build admin pool + GraphClient. GraphClient owns its own pool
	// with LOAD 'age' AfterConnect — we use it for the create_graph
	// step. The admin pool is for the public.tenants INSERT + GRANTs.
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

	// Step (a/b) — INSERT public.tenants (ON CONFLICT DO NOTHING).
	if err := repo.Create(ctx, tenant.Tenant{
		ID:             id,
		WorkOSOrgID:    *workosOrg,
		LifecycleState: "active",
		Pool:           "A",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "repo.Create: %v\n", err)
		return 1
	}

	// Step (c) — AGE create_graph (idempotent).
	if err := provisioner.CreateGraph(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "provisioner.CreateGraph: %v\n", err)
		return 1
	}

	// Step (d) — per-tenant role + GRANTs (idempotent).
	if err := provisioner.CreateRole(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "provisioner.CreateRole: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "OK tenant create: id=%s workos_org=%s schema=%s role=%s\n",
		id, *workosOrg, tenant.SchemaName(id), tenant.RoleName(id))
	return 0
}
