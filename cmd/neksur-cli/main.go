// neksur-cli — admin / operations command-line tool.
//
// Phase 0.5 Plan 04 surface (D-0.5.19): five subcommands wired up to
// internal/tenant.Provisioner so scripts/provision-tenant.sh can
// orchestrate the 12-step provisioning flow:
//
//   tenant create       — public.tenants INSERT + CreateGraph + CreateRole
//   tenant migrate      — Atlas tenant-loop apply (V0050+V0051+V0052)
//   tenant peer         — Initiate VPC peering / poll status / print customer module
//   tenant smoke        — Run gateway+policy+cross-tenant smoke tests
//   tenant cert-issue   — Issue per-customer mTLS client cert via AWS Private CA
//
// Dispatch via os.Args[1..2] — `tenant <verb>`. No external CLI library
// (no cobra; CLAUDE.md anti-dependency stance). Each subcommand owns
// its own *flag.FlagSet declared in its own file (tenant_*.go).
//
// The subcommand functions take the parsed flags, the global env
// (DATABASE_URL, TF_DIR, PRIVATE_CA_ARN, …), and run their step. They
// return an int exit code so dispatch is one-line.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// Top-level dispatch — `tenant` / `polaris` / `policy` verbs.
	switch os.Args[1] {
	case "tenant":
		os.Exit(dispatchTenant(ctx))
	case "polaris":
		os.Exit(dispatchPolaris(ctx))
	case "policy":
		os.Exit(dispatchPolicy(ctx))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown top-level command: %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// dispatchPolicy routes `policy <verb>` subcommands. Plan 01-09 Task 3
// adds the first verb: `compile` (CEL syntax dogfood for SecOps
// policy authors before they push policy text to the per-tenant
// graph — Pitfall 7 mitigation per CONTEXT line 84).
func dispatchPolicy(ctx context.Context) int {
	if len(os.Args) < 3 {
		policyUsage()
		return 2
	}
	verb := os.Args[2]
	subArgs := os.Args[3:]
	switch verb {
	case "compile":
		return runPolicyCompile(ctx, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown policy verb: %q\n", verb)
		policyUsage()
		return 2
	}
}

func policyUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  neksur-cli policy <verb> [flags]

Verbs:
  compile  Validate a CEL policy file against the L1 gateway env (Plan 01-09)

Exit codes:
  0  Policy compiles cleanly.
  1  CEL syntax error / undeclared binding (wrapped cel.ErrCompileFailed).
  2  Usage error / file missing / unreadable.
`)
}

// dispatchPolaris routes `polaris <verb>` subcommands. Plan 01-07
// adds the first verb: `webhook-register`.
func dispatchPolaris(ctx context.Context) int {
	if len(os.Args) < 3 {
		polarisUsage()
		return 2
	}
	verb := os.Args[2]
	subArgs := os.Args[3:]
	switch verb {
	case "webhook-register":
		return runPolarisWebhookRegister(ctx, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown polaris verb: %q\n", verb)
		polarisUsage()
		return 2
	}
}

func polarisUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  neksur-cli polaris <verb> [flags]

Verbs:
  webhook-register  Register Neksur as a Polaris webhook subscriber (Plan 01-07)
`)
}

func dispatchTenant(ctx context.Context) int {
	if len(os.Args) < 3 {
		tenantUsage()
		return 2
	}
	verb := os.Args[2]
	subArgs := os.Args[3:]

	switch verb {
	case "create":
		return runTenantCreate(ctx, subArgs)
	case "migrate":
		return runTenantMigrate(ctx, subArgs)
	case "migrate-to-pool-b":
		return runTenantMigrateToPoolB(ctx, subArgs)
	case "peer":
		return runTenantPeer(ctx, subArgs)
	case "smoke":
		return runTenantSmoke(ctx, subArgs)
	case "cert-issue":
		return runTenantCertIssue(ctx, subArgs)
	case "suspend":
		return runTenantSuspend(ctx, subArgs)
	// Accept both "wind-down" (preferred) and the legacy "windown" alias.
	case "wind-down", "windown":
		return runTenantWindDown(ctx, subArgs)
	case "delete":
		return runTenantDelete(ctx, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown tenant verb: %q\n", verb)
		tenantUsage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `neksur-cli — admin / operations CLI

Usage:
  neksur-cli tenant <verb> [flags]
  neksur-cli polaris <verb> [flags]
  neksur-cli policy <verb> [flags]

Tenant verbs:
  create             Create public.tenants row + AGE create_graph + per-tenant role
  migrate            Apply Atlas tenant-loop migrations (V0050+V0051+V0052)
  migrate-to-pool-b  Pool A → Pool B migration via pg_dump/pg_restore + row-count validation
  peer               Initiate / poll / print VPC peering for the customer side
  smoke              Run the post-provisioning smoke tests
  cert-issue         Issue per-customer mTLS client cert via AWS Private CA
  suspend            Transition active → suspended (D-0.5.20 lifecycle; gateway returns 503 on writes)
  wind-down          Transition active|suspended → wind_down (30-day post-cancellation read-only window)
  delete             IRREVERSIBLE — drop schema + destroy peering + audit (requires --yes)

Polaris verbs:
  webhook-register   Register Neksur as a Polaris webhook subscriber (Plan 01-07)

Policy verbs:
  compile            Validate a CEL policy file against the L1 gateway env (Plan 01-09)

Environment:
  DATABASE_URL         Admin pool DSN (required)
  TF_DIR               Absolute path to neksur-infra phase0-pilot (for peer)
  PRIVATE_CA_ARN       AWS Private CA ARN (for cert-issue; mock if empty)
`)
}

func tenantUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  neksur-cli tenant <verb> [flags]

Verbs:
  create | migrate | migrate-to-pool-b | peer | smoke | cert-issue | suspend | wind-down | delete
`)
}

// envOrEmpty returns os.Getenv(name) — kept as a one-liner so subcommands
// don't each re-import "os" purely for env reads.
func envOrEmpty(name string) string { return os.Getenv(name) }

// requireEnv returns the env value or prints an error and exits the
// dispatch loop with code 2. Subcommand callers should pre-check the
// env they need; this is the common-case helper.
func requireEnv(name string) (string, int) {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required env var: %s\n", name)
		return "", 2
	}
	return v, 0
}
