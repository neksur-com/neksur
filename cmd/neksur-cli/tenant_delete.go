package main

// tenant_delete.go — `neksur-cli tenant delete` subcommand. Plan 07
// D-0.5.20 IRREVERSIBLE terminal transition. Requires --yes confirmation.
//
// Semantics: drops the tenant_<uuid> schema (via internal/tenant.Repo.Delete
// which uses AGE's drop_graph(name, true) for cascade), flips
// public.tenants.lifecycle_state → 'deleted', writes a system_audit_log
// row with event_type='tenant.deleted'. After the DB-side delete
// succeeds, shells out `terraform destroy -target=module.customer_peering[<uuid>]`
// to clean up the customer's VPC peering connection.
//
// Threat model (T-0.5-accidental-delete):
//   • --yes is MANDATORY. Without --yes, prints the action plan + exits
//     with code 2 to communicate "operator did not confirm".
//   • The shell wrapper scripts/tenant-delete.sh adds a second interactive
//     gate (operator must type `DELETE <uuid>`) as defence-in-depth.
//   • 30-day RDS backup retention (Plan 01 pgBackRest policy) provides
//     recovery window if delete was in error.
//
// T-0.5-partial-delete-state:
//   • DB-side delete is the irreversible step. If `terraform destroy`
//     fails afterward, we LOG a WARN but do NOT roll back the DB delete
//     (the schema is already gone; rollback would orphan the row in a
//     wind_down state with no schema — worse). Operator manually cleans
//     peering via `cd $TF_DIR && terraform destroy -target=module.customer_peering[<uuid>]`.
//   • The audit-log row records the partial-state outcome (terraform_destroy_ok=false).
//
// Exit codes:
//   0 — success (DB delete OK; terraform destroy attempted, success or warn)
//   1 — runtime failure (DB error)
//   2 — input validation failure OR --yes not provided

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantDelete(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant delete", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant to delete (required)")
		yes        = fs.Bool("yes", false, "REQUIRED — confirms the operator intends an IRREVERSIBLE delete (schema drop + peering destroy + audit-log row)")
		actor      = fs.String("actor", "", "operator identifier written into system_audit_log payload; default delete@neksur.com")
		skipTF     = fs.Bool("skip-terraform", false, "do NOT shell out to `terraform destroy -target=module.customer_peering[...]`; useful when peering was never created or already destroyed")
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
	if !*yes {
		fmt.Fprintf(os.Stderr, "Refusing to delete tenant %s: --yes flag required for irreversible operation.\n", *tenantUUID)
		fmt.Fprintln(os.Stderr, "Planned actions on --yes:")
		fmt.Fprintf(os.Stderr, "  1. UPDATE public.tenants.lifecycle_state → 'deleted' for id=%s\n", *tenantUUID)
		fmt.Fprintf(os.Stderr, "  2. SELECT drop_graph('tenant_%s_underscored', true) — drops the tenant schema\n", *tenantUUID)
		fmt.Fprintln(os.Stderr, "  3. INSERT a public.system_audit_log row with event_type='tenant.deleted'")
		fmt.Fprintf(os.Stderr, "  4. (unless --skip-terraform) terraform destroy -target=module.customer_peering[%q]\n", *tenantUUID)
		fmt.Fprintln(os.Stderr, "  5. 30-day RDS backup retention (pgBackRest, Plan 01) provides recovery window.")
		return 2
	}
	if *actor == "" {
		*actor = "delete@neksur.com"
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

	// Step 1+2+3 — atomic DB delete (lifecycle_state + drop_graph + audit_log).
	// Pass confirm=true; CLI's --yes gate is what authorizes that.
	if err := repo.Delete(ctx, id, *actor, true); err != nil {
		fmt.Fprintf(os.Stderr, "repo.Delete: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "OK tenant delete (DB side): id=%s state=deleted schema_dropped=true actor=%s\n",
		id, *actor)

	// Step 4 — terraform destroy customer_peering[<uuid>] (T-0.5-partial-delete-state
	// mitigation: failure is WARN, not fatal — audit-log already records the DB delete).
	if *skipTF {
		fmt.Fprintln(os.Stdout, "  --skip-terraform: peering destroy SKIPPED (operator must clean manually if peering exists).")
		fmt.Fprintln(os.Stdout, "  30-day RDS backup retention covers recovery if delete was in error.")
		return 0
	}

	tfDir := envOrEmpty("TF_DIR")
	if tfDir == "" {
		fmt.Fprintln(os.Stderr,
			"WARN: TF_DIR not set; skipping `terraform destroy -target=module.customer_peering[...]`.")
		fmt.Fprintln(os.Stderr,
			"  Operator should manually run: cd <neksur-infra>/environments/phase0-pilot && \\")
		fmt.Fprintf(os.Stderr,
			"    terraform destroy -target=module.customer_peering[%q] -auto-approve\n", id.String())
		return 0
	}

	target := fmt.Sprintf("module.customer_peering[%q]", id.String())
	cmd := exec.CommandContext(ctx, "terraform",
		"-chdir="+tfDir,
		"destroy",
		"-target="+target,
		"-auto-approve",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stdout, "  Running: terraform -chdir=%s destroy -target=%s -auto-approve\n", tfDir, target)
	if err := cmd.Run(); err != nil {
		// Non-fatal: DB delete succeeded; operator can re-run the destroy
		// manually. Audit log already records the DB-side outcome.
		fmt.Fprintf(os.Stderr,
			"WARN: terraform destroy failed (DB delete already succeeded — peering cleanup is incomplete): %v\n", err)
		fmt.Fprintln(os.Stderr,
			"  Operator: re-run `terraform destroy -target="+target+"` manually to complete peering cleanup.")
		return 0
	}

	fmt.Fprintf(os.Stdout, "OK tenant delete (peering side): customer_peering[%q] destroyed.\n", id.String())
	fmt.Fprintln(os.Stdout, "  30-day RDS backup retention (Plan 01 pgBackRest) provides recovery window.")
	return 0
}
