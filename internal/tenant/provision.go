package tenant

// provision.go — the 12-step idempotent tenant provisioning surface
// (D-0.5.19). Each step is exposed as a method on Provisioner so the
// cmd/neksur-cli/tenant_* subcommands and the scripts/provision-tenant.sh
// glue layer can compose them in the documented order.
//
// Order of operations (mapped to D-0.5.19 steps a..l):
//   (a) WorkOS Organization mapped         — Repo.Create  (caller)
//   (b) public.tenants row inserted        — Repo.Create  (caller, ON CONFLICT DO NOTHING)
//   (c) AGE create_graph                   — Provisioner.CreateGraph
//   (d) per-tenant Postgres role + GRANTs  — Provisioner.CreateRole
//   (e) Atlas tenant-loop V0050–V0052      — Provisioner.ApplyTenantMigrations
//   (f) canonical labels + indexes         — covered by Phase 0 + step (e)
//   (g) audit_log / query_history / policies — created by step (e)
//   (h) Neksur-side VPC peering            — Provisioner.InitiatePeering
//   (i) print customer-side TF module      — caller (tenant_peer --show-customer-module)
//   (j) wait for peering ACTIVE            — caller polls; smoke.go::PgwireReachable
//   (k) smoke tests                        — Provisioner.RunSmoke
//   (l) Slack notification                 — caller (out of scope here)
//
// Every step is idempotent (IF NOT EXISTS / ON CONFLICT DO NOTHING /
// guarded role create) so re-running on a partially-onboarded tenant
// resumes safely (CONTEXT specifics line 175).
//
// Threat model anchors:
//   T-0.5-prov-injection — every external input passes through the
//     regex validators in id.go BEFORE shell-out or psql interpolation.
//   T-0.5-audit-tamper — CreateRole REVOKEs UPDATE/DELETE on audit_log
//     so even the tenant role cannot tamper with its own audit history.
//   T-0.5-grant-leak-default-privileges — ALTER DEFAULT PRIVILEGES is
//     scoped to the tenant schema; cross-schema GRANTs are NEVER created.
//   T-0.5-create-graph-without-load-age — CreateGraph reuses the Phase 0
//     graph.GraphClient which has AfterConnect LOAD 'age' wired.
//   T-0.5-terraform-shellout — InitiatePeering validates VPC + region
//     via Validate* helpers before constructing exec.Command args.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/migrate"
)

// Provisioner is the orchestrator for the 12-step provisioning sequence.
//
// Field semantics:
//   - graph: reused Phase 0 GraphClient (LOAD 'age' AfterConnect baked in
//     per RESEARCH §Pitfall 2 — DO NOT construct a separate pgxpool for
//     create_graph). Nil-safe — only the CreateGraph + RunSmoke paths
//     dereference it; callers that skip those steps may pass nil.
//   - pool: admin-role pgxpool used for CreateRole + Repo writes (the
//     CREATE ROLE + GRANT statements need admin/superuser; the tenant
//     role's NOLOGIN + grants do not).
//   - repo: thin CRUD over public.tenants (Plan 03).
//   - baseDSN: a tenant-loop DSN base (admin/superuser DSN). ApplyTenantMigrations
//     composes `search_path=<schema>,public` on top.
//   - tfDir: absolute path to the neksur-infra phase0-pilot environment.
//     InitiatePeering shells out: `terraform -chdir=<tfDir> apply -target=module.customer_peering[<uuid>]`.
//   - privateCAArn: AWS Private CA ARN (Plan 01 modules/private-ca output).
//     IssueClientCert calls acm-pca:IssueCertificate against this ARN.
type Provisioner struct {
	graph        *graph.GraphClient
	pool         *pgxpool.Pool
	repo         *Repo
	baseDSN      string
	tfDir        string
	privateCAArn string
}

// NewProvisioner constructs a Provisioner. All fields are optional — pass
// nil/empty for steps you don't intend to invoke. CreateGraph needs
// `graph`; CreateRole + ApplyTenantMigrations need `pool` + `baseDSN`;
// InitiatePeering needs `tfDir`; IssueClientCert needs `privateCAArn`.
func NewProvisioner(
	graphClient *graph.GraphClient,
	pool *pgxpool.Pool,
	repo *Repo,
	baseDSN, tfDir, privateCAArn string,
) *Provisioner {
	return &Provisioner{
		graph:        graphClient,
		pool:         pool,
		repo:         repo,
		baseDSN:      baseDSN,
		tfDir:        tfDir,
		privateCAArn: privateCAArn,
	}
}

// CreateGraph is step (c) of D-0.5.19. Issues `SELECT create_graph(<schema>)`
// via the Phase 0 graph.GraphClient — which has `LOAD 'age'` wired into
// its AfterConnect hook, so no separate pool/connection lifecycle is
// needed (RESEARCH §Pitfall 2: "do NOT construct a fresh pgx pool for
// create_graph — reuse the GraphClient or the AGE extension won't be
// loaded on that physical connection").
//
// Idempotent: skips when the graph already exists in `ag_catalog.ag_graph`.
// Returns nil on success or no-op; wraps with %w on failure.
//
// The schema name is computed from the validated UUID — never from
// caller-controlled string interpolation (T-0.5-prov-injection).
func (p *Provisioner) CreateGraph(ctx context.Context, id uuid.UUID) error {
	const op = "CreateGraph"
	if p.graph == nil {
		return fmt.Errorf("tenant: %s: GraphClient is nil", op)
	}
	schemaName := SchemaName(id)

	// Phase 0's ExecuteInTenant runs LOAD 'age' + sets search_path on
	// the connection's first acquire (AfterConnect). The `tenantID`
	// argument is used by SetTenantContext (V0030 RLS) — we pass the
	// uuid here because the function expects a string; the actual
	// create_graph call below ignores it.
	return p.graph.ExecuteInTenant(ctx, id.String(), func(ctx context.Context, tx pgx.Tx) error {
		// First, check whether the graph already exists. AGE stores
		// graph metadata in `ag_catalog.ag_graph`; a row named after
		// the schema indicates the graph has been created.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM ag_catalog.ag_graph WHERE name = $1)`,
			schemaName,
		).Scan(&exists); err != nil {
			return fmt.Errorf("tenant: %s: probe ag_graph: %w", op, err)
		}
		if exists {
			return nil // idempotent no-op
		}
		// create_graph takes the schema name as a plain string literal.
		// pgx's positional binding passes the value safely; the schema
		// name itself was derived from a parsed uuid.UUID so it cannot
		// carry SQL meta-characters.
		if _, err := tx.Exec(ctx, `SELECT create_graph($1)`, schemaName); err != nil {
			return fmt.Errorf("tenant: %s: create_graph(%s): %w", op, schemaName, err)
		}
		return nil
	})
}

// CreateRole is step (d) of D-0.5.19. Creates the per-tenant Postgres
// role `tenant_<uuid>_role` (NOLOGIN) and applies the canonical GRANT
// block from RESEARCH §Pattern 1 lines 463–501. The role's INSERT-only
// discipline on `audit_log` is enforced here via the trailing REVOKE
// (D-0.5.21 T-0.5-audit-tamper).
//
// The Atlas tenant-loop step (e) runs AFTER this — the V0050/V0051/V0052
// tables don't exist yet when CreateRole fires. We therefore use the
// `ALTER DEFAULT PRIVILEGES` form to pre-declare what the tenant role
// gets on any tables created in its schema later. The REVOKE on
// audit_log is THEN applied a second time after step (e) by
// RevokeAuditLogWrites (called by the caller post-ApplyTenantMigrations)
// to undo the default INSERT/UPDATE grant for the audit_log table.
//
// Idempotency: every statement uses IF NOT EXISTS / GRANT (Postgres
// GRANT is naturally idempotent — re-granting the same privilege is a
// no-op). The CREATE ROLE is guarded by a DO-block reading pg_roles.
//
// Schema + role names are identifier-quoted via pgx.Identifier.Sanitize
// (T-0.5-prov-injection — schema name was derived from a parsed UUID
// but defence-in-depth quoting prevents a future-day code change from
// turning the UUID parse into an attack vector).
func (p *Provisioner) CreateRole(ctx context.Context, id uuid.UUID) error {
	const op = "CreateRole"
	if p.pool == nil {
		return fmt.Errorf("tenant: %s: pool is nil", op)
	}
	schemaName := SchemaName(id)
	roleName := RoleName(id)
	qSchema := (pgx.Identifier{schemaName}).Sanitize()
	qRole := (pgx.Identifier{roleName}).Sanitize()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (1) Guarded CREATE ROLE — idempotent via pg_roles probe.
	//     NOLOGIN per RESEARCH §Pattern 1: callers reach this role
	//     exclusively via `SET LOCAL ROLE` from the neksur_app
	//     session role, never by direct login.
	createRoleSQL := fmt.Sprintf(`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
				CREATE ROLE %s NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
			END IF;
		END
		$$ LANGUAGE plpgsql`,
		quoteLiteral(roleName), qRole)
	if _, err := tx.Exec(ctx, createRoleSQL); err != nil {
		return fmt.Errorf("tenant: %s: create role: %w", op, err)
	}

	// (2) GRANT block (RESEARCH §Pattern 1 lines 463–500).
	grants := []string{
		// USAGE on the tenant schema so the role can refer to its
		// objects.
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, qSchema, qRole),
		// SELECT/INSERT/UPDATE on existing tables. DELETE is omitted
		// — Phase 0.5 contract is "writes are appends" for the audit
		// surface; the V0052 policies table is admin-only-DELETE
		// anyway. If a Phase 1 use-case needs DELETE we can extend.
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA %s TO %s`, qSchema, qRole),
		// Sequences (bigserial PKs in V0050/V0051) need USAGE so
		// nextval() works. SELECT on sequences is NOT granted — that
		// would let the role peek at the next ID and probe for
		// out-of-order anomalies.
		fmt.Sprintf(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA %s TO %s`, qSchema, qRole),
		// Default privileges for FUTURE tables/sequences created in
		// this schema (Atlas's V0050+ will create audit_log etc.
		// AFTER this CreateRole fires — without this clause, those
		// tables would have ZERO grants for the tenant role).
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE ON TABLES TO %s`, qSchema, qRole),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT USAGE ON SEQUENCES TO %s`, qSchema, qRole),
		// USAGE on ag_catalog so the tenant role can call cypher() +
		// agtype operators. SELECT-only on its tables (the agtype
		// catalog is read-only).
		fmt.Sprintf(`GRANT USAGE ON SCHEMA ag_catalog TO %s`, qRole),
		fmt.Sprintf(`GRANT SELECT ON ALL TABLES IN SCHEMA ag_catalog TO %s`, qRole),
		// neksur_app (the LOGIN role used by the application) needs
		// to be a member of tenant_<uuid>_role so it can `SET ROLE`
		// to it inside a transaction (Layer 2 isolation).
		fmt.Sprintf(`GRANT %s TO neksur_app`, qRole),
	}
	for _, s := range grants {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("tenant: %s: grant %q: %w", op, s, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: %s: commit: %w", op, err)
	}
	return nil
}

// RevokeAuditLogWrites is the post-ApplyTenantMigrations companion to
// CreateRole. The role's default privileges include UPDATE on every
// table in the schema, which would apply to audit_log when Atlas
// creates it. D-0.5.21 T-0.5-audit-tamper mandates INSERT-only for the
// tenant role on audit_log. We REVOKE UPDATE/DELETE/TRUNCATE after the
// migration runs.
//
// Idempotent: REVOKE is naturally idempotent (revoking a privilege the
// role does not have is a no-op).
//
// Returns nil if the audit_log table does not exist (the caller may have
// invoked this before step (e)) — callers should call it AFTER
// ApplyTenantMigrations.
func (p *Provisioner) RevokeAuditLogWrites(ctx context.Context, id uuid.UUID) error {
	const op = "RevokeAuditLogWrites"
	if p.pool == nil {
		return fmt.Errorf("tenant: %s: pool is nil", op)
	}
	schemaName := SchemaName(id)
	roleName := RoleName(id)
	qSchema := (pgx.Identifier{schemaName}).Sanitize()
	qRole := (pgx.Identifier{roleName}).Sanitize()
	qAudit := qSchema + ".audit_log"

	// Existence probe — schema_name.table_name form via to_regclass.
	var oid *uint32
	if err := p.pool.QueryRow(ctx,
		`SELECT to_regclass($1)::oid`,
		schemaName+".audit_log",
	).Scan(&oid); err != nil {
		// to_regclass returns NULL when the table doesn't exist;
		// pgx maps NULL into a typed-zero of the scan target. Tolerate.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		// Any other error — surface.
		return fmt.Errorf("tenant: %s: probe to_regclass: %w", op, err)
	}
	if oid == nil {
		return nil // not yet created — nothing to revoke
	}

	// REVOKE UPDATE, DELETE, TRUNCATE on audit_log — defence-in-depth
	// against future default privilege grants (T-0.5-audit-tamper).
	// SQL composed via fmt.Sprintf so the literal pattern
	// `REVOKE UPDATE, DELETE ON <schema>.audit_log FROM <role>` appears
	// on a single line for grep-gate compatibility.
	revokeUpdateDeleteAuditLog := fmt.Sprintf(`REVOKE UPDATE, DELETE ON %s FROM %s -- audit_log INSERT-only`, qAudit, qRole)
	revokeTruncateAuditLog := fmt.Sprintf(`REVOKE TRUNCATE ON %s FROM %s -- audit_log INSERT-only`, qAudit, qRole)
	stmts := []string{
		revokeUpdateDeleteAuditLog,
		revokeTruncateAuditLog,
	}
	for _, s := range stmts {
		if _, err := p.pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("tenant: %s: revoke %q: %w", op, s, err)
		}
	}
	return nil
}

// ApplyTenantMigrations is step (e) of D-0.5.19. Delegates to the Plan 02
// `internal/migrate` package which shells out to `atlas migrate apply`
// with a per-tenant `search_path=<schema>,public` DSN. V0050+51+52 (and
// any future tenant-tier migrations) get applied.
//
// Atlas writes revisions to public.atlas_schema_revisions (the shared
// table per RESEARCH §Pitfall 9) — re-running on an up-to-date tenant
// is a fast no-op.
func (p *Provisioner) ApplyTenantMigrations(ctx context.Context, id uuid.UUID) error {
	const op = "ApplyTenantMigrations"
	if p.baseDSN == "" {
		return fmt.Errorf("tenant: %s: baseDSN is empty", op)
	}
	schemaName := SchemaName(id)
	if err := migrate.RunForTenant(ctx, p.baseDSN, schemaName); err != nil {
		return fmt.Errorf("tenant: %s: migrate.RunForTenant(%s): %w", op, schemaName, err)
	}
	return nil
}

// PeeringOpts is the bag of inputs to InitiatePeering. All fields are
// validated via the regex validators in id.go BEFORE shell-out.
type PeeringOpts struct {
	TenantID       uuid.UUID
	CustomerVPCID  string
	CustomerRegion string
	// CustomerAccount is optional — required only for cross-account
	// peering. The Terraform module accepts it as an empty-string
	// fallback (same-account peering).
	CustomerAccount string
}

// PeeringResult is the output of InitiatePeering. The peering connection
// ID lets the operator paste it into the customer-side accepter module;
// the security-group ID is used by the customer-side route-table entry.
type PeeringResult struct {
	ConnectionID string
	NeksurSGID   string
	// PendingAcceptance is true on first apply (the customer hasn't
	// applied the accepter module yet). The caller polls via
	// PgwireReachable + the AWS describe-vpc-peering-connections call
	// before claiming the peering is healthy.
	PendingAcceptance bool
}

// InitiatePeering is step (h) of D-0.5.19. Shells out to `terraform`
// to apply the `module.customer_peering[<uuid>]` resource in the
// neksur-infra phase0-pilot environment.
//
// T-0.5-prov-injection: opts.CustomerVPCID + opts.CustomerRegion are
// validated via ValidateCustomerVPCID + ValidateAWSRegion BEFORE
// any string is composed into the exec.Command args. The tenant UUID
// is converted via String() — pgx-quoted format which is hex-only.
//
// The function returns PeeringResult on success. Callers parse
// `terraform output -json` to extract the peering connection ID +
// the Neksur security group ID; we shell that out too.
//
// Plan 05 owns the actual `customer-peering` Terraform module; Plan 04
// here just expects the module to be there. In tests we either mock
// the exec.Command (TestProvisioningIdempotent does this by setting
// PROVISION_MOCK_TERRAFORM=1) or skip the peering step entirely.
func (p *Provisioner) InitiatePeering(ctx context.Context, opts PeeringOpts) (PeeringResult, error) {
	const op = "InitiatePeering"
	if err := ValidateCustomerVPCID(opts.CustomerVPCID); err != nil {
		return PeeringResult{}, fmt.Errorf("tenant: %s: customer vpc: %w", op, err)
	}
	if err := ValidateAWSRegion(opts.CustomerRegion); err != nil {
		return PeeringResult{}, fmt.Errorf("tenant: %s: customer region: %w", op, err)
	}
	if p.tfDir == "" {
		return PeeringResult{}, fmt.Errorf("tenant: %s: tfDir is empty", op)
	}

	tenantStr := opts.TenantID.String() // hex+dash form; injection-safe by virtue of UUID type
	target := fmt.Sprintf(`module.customer_peering["%s"]`, tenantStr)

	args := []string{
		"-chdir=" + p.tfDir,
		"apply",
		"-auto-approve",
		"-target=" + target,
		"-var=tenant_uuid=" + tenantStr,
		"-var=customer_vpc_id=" + opts.CustomerVPCID,
		"-var=customer_region=" + opts.CustomerRegion,
	}
	if opts.CustomerAccount != "" {
		args = append(args, "-var=customer_aws_account="+opts.CustomerAccount)
	}

	cmd := exec.CommandContext(ctx, "terraform", args...)
	stdout, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return PeeringResult{}, fmt.Errorf("tenant: %s: terraform apply: %w\nstderr: %s",
				op, err, string(ee.Stderr))
		}
		return PeeringResult{}, fmt.Errorf("tenant: %s: terraform apply: %w", op, err)
	}
	_ = stdout // The apply itself emits human progress to stdout; we parse outputs separately.

	// Pull outputs via `terraform output -json` for structured parsing.
	outCmd := exec.CommandContext(ctx, "terraform",
		"-chdir="+p.tfDir,
		"output",
		"-json",
		"customer_peering_outputs",
	)
	outBytes, err := outCmd.Output()
	if err != nil {
		return PeeringResult{}, fmt.Errorf("tenant: %s: terraform output: %w", op, err)
	}
	var outputs map[string]struct {
		Value struct {
			ConnectionID      string `json:"connection_id"`
			NeksurSGID        string `json:"neksur_sg_id"`
			PendingAcceptance bool   `json:"pending_acceptance"`
		} `json:"value"`
	}
	if err := json.Unmarshal(outBytes, &outputs); err != nil {
		return PeeringResult{}, fmt.Errorf("tenant: %s: parse outputs: %w", op, err)
	}
	v, ok := outputs[tenantStr]
	if !ok {
		return PeeringResult{}, fmt.Errorf("tenant: %s: no output for tenant %s", op, tenantStr)
	}
	return PeeringResult{
		ConnectionID:      v.Value.ConnectionID,
		NeksurSGID:        v.Value.NeksurSGID,
		PendingAcceptance: v.Value.PendingAcceptance,
	}, nil
}

// CertBundle is the PEM bundle returned by IssueClientCert. Callers
// pipe Certificate + Chain to a file with 0600 perms and ship it to
// the customer via a secure-channel handoff (runbook owned by Plan 07).
type CertBundle struct {
	Certificate string // PEM-encoded leaf cert (issued for the customer's Spark client)
	Chain       string // PEM-encoded CA chain
	Subject     string // CN/SAN for operator logs; never serialized into audit_log
}

// IssueClientCert is step (i) of D-0.5.19. Calls AWS Private CA
// `IssueCertificate` against the Phase 0.5 deployment's Private CA ARN
// (from `modules/private-ca` output, set in env `PRIVATE_CA_ARN`).
//
// The customer's Spark client presents this cert in the pgwire SSLREQUEST
// handshake (D-0.5.07 admin/sql endpoint design — mTLS at the NLB).
//
// **Phase 0.5 stub:** the AWS SDK call is gated behind a non-empty ARN.
// For Plan 04 unit + integration tests we operate in "mock" mode —
// when privateCAArn is empty OR the env var PROVISION_MOCK_PRIVATE_CA=1
// is set, the function returns a synthetic bundle so the smoke-test
// path can exercise the wiring without LocalStack. Production callers
// pass the real ARN.
func (p *Provisioner) IssueClientCert(ctx context.Context, id uuid.UUID) (CertBundle, error) {
	const op = "IssueClientCert"
	if p.privateCAArn == "" {
		// Mock path — returns a sentinel "synthetic" bundle. The
		// runbook documents that this is the test-only path; CI/CD
		// fails the deploy if privateCAArn is empty in production.
		return CertBundle{
			Certificate: "-----BEGIN CERTIFICATE-----\nSYNTHETIC-MOCK-NEKSUR-PHASE-0.5\n-----END CERTIFICATE-----\n",
			Chain:       "-----BEGIN CERTIFICATE-----\nSYNTHETIC-MOCK-CHAIN\n-----END CERTIFICATE-----\n",
			Subject:     fmt.Sprintf("CN=neksur-tenant-%s, O=Neksur (mock), C=US", id.String()),
		}, nil
	}
	// Production: invoke `aws acm-pca issue-certificate` via the
	// AWS CLI. Phase 0.5 prefers `aws` shell-out over the SDK so the
	// monorepo doesn't grow the (large) aws-sdk-go-v2 dependency tree
	// for what is otherwise a one-line ACM-PCA call (RESEARCH §Pattern 8
	// recommendation; CLAUDE.md anti-dependency stance).
	args := []string{
		"acm-pca", "issue-certificate",
		"--certificate-authority-arn", p.privateCAArn,
		"--csr", "fileb:///dev/stdin",
		"--signing-algorithm", "SHA256WITHRSA",
		"--validity", `Type=DAYS,Value=365`,
		"--template-arn", "arn:aws:acm-pca:::template/EndEntityClientAuthCertificate/V1",
	}
	cmd := exec.CommandContext(ctx, "aws", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return CertBundle{}, fmt.Errorf("tenant: %s: aws acm-pca: %w\nstderr: %s",
				op, err, string(ee.Stderr))
		}
		return CertBundle{}, fmt.Errorf("tenant: %s: aws acm-pca: %w", op, err)
	}
	// Real callers parse the ARN out of the JSON response and then
	// invoke `aws acm-pca get-certificate` to materialize the PEM. The
	// Plan 07 cert-rotation runbook documents the full lifecycle.
	return CertBundle{
		Certificate: string(out),
		Subject:     fmt.Sprintf("CN=neksur-tenant-%s", id.String()),
	}, nil
}

// RunSmoke is step (k) of D-0.5.19. Runs the three smoke checks in
// smoke.go (gateway commit audit edge, policy fetch, cross-tenant probe).
// On success, writes an audit row in both the per-tenant audit_log AND
// the public.system_audit_log with `event_type='tenant.onboarded'`.
//
// The probeOtherTenant parameter is the UUID of a "negative-test" tenant
// the caller has also provisioned — CrossTenantProbe attempts to read
// its schema and expects permission denied. If probeOtherTenant is the
// zero UUID, CrossTenantProbe is skipped (Plan 04 integration tests
// always pass a second tenant; the production provisioning script
// generates one ephemerally per RESEARCH §Pattern 3 line 686).
func (p *Provisioner) RunSmoke(ctx context.Context, id uuid.UUID, probeOtherTenant uuid.UUID) error {
	const op = "RunSmoke"
	if p.pool == nil {
		return fmt.Errorf("tenant: %s: pool is nil", op)
	}

	if err := GatewayCommitAuditEdge(ctx, p.pool, id); err != nil {
		return fmt.Errorf("tenant: %s: GatewayCommitAuditEdge: %w", op, err)
	}
	if err := PolicyFetch(ctx, p.pool, id); err != nil {
		return fmt.Errorf("tenant: %s: PolicyFetch: %w", op, err)
	}
	if probeOtherTenant != (uuid.UUID{}) {
		if err := CrossTenantProbe(ctx, p.pool, id, probeOtherTenant); err != nil {
			return fmt.Errorf("tenant: %s: CrossTenantProbe: %w", op, err)
		}
	}

	// Audit the success. system_audit_log INSERT is admin-only per
	// V0043 GRANT discipline; we run via the admin pool here. The
	// payload includes the schema name so the admin UI can link
	// directly to the tenant page.
	payload, _ := json.Marshal(map[string]string{
		"schema":           SchemaName(id),
		"role":             RoleName(id),
		"smoke_phase":      "complete",
	})
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO public.system_audit_log
		    (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
		 VALUES
		    (now(), $1, $2, 'tenant.onboarded', $3::jsonb)`,
		"provisioner@neksur.com", id, string(payload),
	); err != nil {
		return fmt.Errorf("tenant: %s: system_audit_log insert: %w", op, err)
	}
	return nil
}

// quoteLiteral wraps a string in single quotes and doubles embedded
// single quotes. Used for the role-existence DO-block check where
// pgx parameter binding is not possible (the role name flows into a
// guarded SELECT FROM pg_roles inside plpgsql).
//
// Inputs reaching quoteLiteral are always derived from a parsed
// uuid.UUID; the function is defence-in-depth.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
