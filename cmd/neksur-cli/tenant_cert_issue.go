package main

// tenant_cert_issue.go — `neksur-cli tenant cert-issue` subcommand. Step
// (i) of D-0.5.19. Issues a per-customer mTLS client cert via AWS
// Private CA (Plan 01 modules/private-ca output). The PEM bundle is
// written to stdout — operator pipes to a file with 0600 perms and
// ships to the customer via a secure-channel handoff (runbook owned
// by Plan 07).
//
// When PRIVATE_CA_ARN is unset OR PROVISION_MOCK_PRIVATE_CA=1 is set,
// the function returns a synthetic mock bundle (Plan 04 test path).
// Production callers MUST set PRIVATE_CA_ARN.

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/tenant"
)

func runTenantCertIssue(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tenant cert-issue", flag.ContinueOnError)
	var (
		tenantUUID = fs.String("tenant-uuid", "", "UUID v4 of the tenant (required)")
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

	id, err := uuid.Parse(*tenantUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uuid.Parse: %v\n", err)
		return 2
	}

	provisioner := tenant.NewProvisioner(nil, nil, nil, "", envOrEmpty("TF_DIR"), envOrEmpty("PRIVATE_CA_ARN"))

	bundle, err := provisioner.IssueClientCert(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "IssueClientCert: %v\n", err)
		return 1
	}

	// Print Subject to stderr for operator log; PEM(s) to stdout for
	// pipe-to-file. The stderr line should NOT include the private key
	// (we don't have one — Private CA issues the leaf cert; the
	// customer is responsible for generating the matching CSR/keypair).
	fmt.Fprintf(os.Stderr, "subject: %s\n", bundle.Subject)
	fmt.Fprint(os.Stdout, bundle.Certificate)
	if bundle.Chain != "" {
		fmt.Fprint(os.Stdout, bundle.Chain)
	}
	return 0
}
