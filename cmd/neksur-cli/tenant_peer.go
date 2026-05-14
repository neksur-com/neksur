package main

// tenant_peer.go — `neksur-cli tenant peer` subcommand. Steps (h), (i),
// and (j) of D-0.5.19.
//
//   Default flow (--customer-vpc + --customer-region given):
//     Step (h) — terraform apply -target=module.customer_peering[<uuid>]
//                via internal/tenant.Provisioner.InitiatePeering.
//
//   --status                Step (j) — poll AWS peering status; prints
//                           one of `active`, `pending-acceptance`,
//                           `expired`, etc. Plan 04 owns the Go code;
//                           Plan 05 ships the Terraform module + the
//                           `aws ec2 describe-vpc-peering-connections`
//                           shell-out. For Plan 04 we wire a STUB that
//                           prints `pending-acceptance` by default, or
//                           the value of NEKSUR_FAKE_PEER_STATUS env var
//                           (lets the integration test simulate the
//                           async-acceptance pattern).
//
//   --show-customer-module  Step (i) — prints the canned customer-side
//                           Terraform module text to stdout. The
//                           operator pipes this to a file and sends to
//                           the customer.
//
// Validation: --customer-vpc passes ValidateCustomerVPCID; --customer-region
// passes ValidateAWSRegion. T-0.5-prov-injection mitigation runs BEFORE
// terraform is invoked.

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantPeer(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant peer", flag.ContinueOnError)
	var (
		tenantUUID      = fs.String("tenant-uuid", "", "UUID v4 of the tenant (required)")
		customerVPC     = fs.String("customer-vpc", "", "customer AWS VPC ID, e.g., vpc-0123456789abcdef0")
		customerRegion  = fs.String("customer-region", "", "customer AWS region, e.g., us-east-1")
		customerAccount = fs.String("customer-account", "", "customer AWS account ID (optional)")
		status          = fs.Bool("status", false, "poll peering status only (prints active | pending-acceptance | ...)")
		showCustomer    = fs.Bool("show-customer-module", false, "print customer-side Terraform module text")
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

	// --show-customer-module: prints the canned module; no other args
	// required.
	if *showCustomer {
		fmt.Fprint(os.Stdout, customerPeeringModuleTemplate(*tenantUUID))
		return 0
	}

	// --status: poll. Stub for Plan 04 — defers to env var or default.
	if *status {
		fakeStatus := envOrEmpty("NEKSUR_FAKE_PEER_STATUS")
		if fakeStatus == "" {
			fakeStatus = "pending-acceptance"
		}
		fmt.Fprintln(os.Stdout, fakeStatus)
		return 0
	}

	// Default flow — initiate peering. Requires --customer-vpc + --customer-region.
	if *customerVPC == "" || *customerRegion == "" {
		fmt.Fprintln(os.Stderr, "missing --customer-vpc and/or --customer-region")
		fs.Usage()
		return 2
	}
	// Validation BEFORE any shell-out (T-0.5-prov-injection).
	if err := tenant.ValidateCustomerVPCID(*customerVPC); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := tenant.ValidateAWSRegion(*customerRegion); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	id, err := uuid.Parse(*tenantUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uuid.Parse: %v\n", err)
		return 2
	}

	// For the Plan 04 dry-run path, allow the operator to skip the
	// actual terraform apply (the module lives in Plan 05). When
	// PROVISION_MOCK_TERRAFORM=1, print a fake result and return 0
	// — integration tests use this to simulate the async-acceptance
	// pattern without a real Terraform deployment.
	if envOrEmpty("PROVISION_MOCK_TERRAFORM") == "1" {
		fmt.Fprintf(os.Stdout, "MOCK terraform apply tenant=%s vpc=%s region=%s account=%s\n",
			id, *customerVPC, *customerRegion, *customerAccount)
		fmt.Fprintln(os.Stdout, "MOCK connection_id=pcx-MOCK00 neksur_sg_id=sg-MOCK00 pending_acceptance=true")
		return 0
	}

	dsn, code := requireEnv("DATABASE_URL")
	if code != 0 {
		return code
	}
	tfDir, code := requireEnv("TF_DIR")
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
	provisioner := tenant.NewProvisioner(nil, pool, repo, dsn, tfDir, envOrEmpty("PRIVATE_CA_ARN"))

	result, err := provisioner.InitiatePeering(ctx, tenant.PeeringOpts{
		TenantID:        id,
		CustomerVPCID:   *customerVPC,
		CustomerRegion:  *customerRegion,
		CustomerAccount: *customerAccount,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "InitiatePeering: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "OK tenant peer: connection_id=%s neksur_sg_id=%s pending=%v\n",
		result.ConnectionID, result.NeksurSGID, result.PendingAcceptance)
	return 0
}

// customerPeeringModuleTemplate returns the canned customer-side
// Terraform module text. The customer applies this on their AWS
// account; once applied, the peering becomes ACTIVE and Neksur's
// pgwire NLB becomes reachable from the customer's Spark client.
//
// The module is intentionally minimal (~30 lines) per D-0.5.06. The
// customer fills in the peering_connection_id printed by Step (h)
// above; route-table-id + their security group are their choice.
//
// Plan 05 ships the corresponding requester-side module
// (modules/customer-peering/main.tf); the customer-side template here
// is the operator-facing copy of that module's `customer` variant.
func customerPeeringModuleTemplate(tenantUUID string) string {
	return fmt.Sprintf(`# Customer-side VPC peering module for Neksur SaaS tenant %[1]s
#
# Generated by `+"`neksur-cli tenant peer --show-customer-module`"+`.
# Apply this on the customer's AWS account in the customer's VPC's region.
#
# Inputs: peering_connection_id (from Neksur operator) + customer_route_table_ids.

variable "peering_connection_id" {
  type        = string
  description = "VPC peering connection ID provided by Neksur operations."
}

variable "customer_route_table_ids" {
  type        = list(string)
  description = "Route table IDs in the customer VPC that should route Neksur traffic via the peering connection."
}

variable "neksur_vpc_cidr" {
  type        = string
  default     = "10.0.0.0/16"
  description = "Neksur VPC CIDR (do not change unless Neksur ops directs)."
}

resource "aws_vpc_peering_connection_accepter" "neksur" {
  vpc_peering_connection_id = var.peering_connection_id
  auto_accept               = true

  tags = {
    Name      = "neksur-saas-tenant-%[1]s"
    ManagedBy = "neksur-customer-peering"
  }
}

resource "aws_route" "to_neksur" {
  for_each                  = toset(var.customer_route_table_ids)
  route_table_id            = each.value
  destination_cidr_block    = var.neksur_vpc_cidr
  vpc_peering_connection_id = aws_vpc_peering_connection_accepter.neksur.id
}
`, tenantUUID)
}
