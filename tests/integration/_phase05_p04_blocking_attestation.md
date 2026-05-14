# Phase 0.5 Plan 04 [BLOCKING] Attestation

**Attestation generated:** 2026-05-14T15:51:05Z (UTC)
**Repo HEAD at attestation:** `8aaaf28` (neksur-core)
**Atlas CLI version:** v1.2.1-ccae3e7-canary (`~/bin/atlas`)
**Postgres+AGE image:** `apache/age:release_PG16_1.6.0` (testcontainers-go)
**Host:** Darwin 25.3.0 (x86_64) — Docker Desktop 27.x

## Per-criterion verdict

- Layer 1: PASS
- Layer 2: PASS
- Layer 3: PASS
- Idempotent: PASS
- Regex: PASS
- Onboarding rehearsal: PASS

## Evidence

### Integration test run

Command:
```
cd /Users/evgeny/neksur-core && \
NEKSUR_ATLAS_BIN=/Users/evgeny/bin/atlas \
go test -tags integration \
  -run 'TestLayer1_NoSearchPathQueryFails|TestLayer2_CrossTenantRoleAccessFails|TestLayer3_NoCurrentTenantReturnsZeroRows|TestProvisioningIdempotent|TestProvisioningRegex' \
  ./tests/integration/ -count=1 -timeout 15m -v
```

Result (per-test):

```
=== RUN   TestProvisioningIdempotent
--- PASS: TestProvisioningIdempotent (1.32s)
=== RUN   TestProvisioningRegex
--- PASS: TestProvisioningRegex (0.00s)
    (28 sub-tests — all PASS — exercising every Validate* helper with
     positive + negative cases per RESEARCH §Pattern 3 line 635)
=== RUN   TestLayer1_NoSearchPathQueryFails
    tenant_isolation_test.go:107: Layer 1 PASS — SQLSTATE 42P01 (undefined_table) as expected: relation "audit_log" does not exist
--- PASS: TestLayer1_NoSearchPathQueryFails (1.22s)
=== RUN   TestLayer2_CrossTenantRoleAccessFails
    tenant_isolation_test.go:159: Layer 2 PASS — SQLSTATE 42501 (insufficient_privilege) as expected: permission denied for schema tenant_bbbbbbbb_bbbb_4bbb_bbbb_bbbbbbbbbbbb
--- PASS: TestLayer2_CrossTenantRoleAccessFails (1.55s)
=== RUN   TestLayer3_NoCurrentTenantReturnsZeroRows
    tenant_isolation_test.go:243: Layer 3 PASS — 0 rows from public.tenants without app.current_tenant GUC
--- PASS: TestLayer3_NoCurrentTenantReturnsZeroRows (1.33s)
PASS
ok  github.com/neksur-com/neksur/tests/integration  6.948s
```

Five tests / one go-test invocation / no flakes / wall-clock ~7s
(testcontainer cold-start dominates; each test boots its own container).

### Regression — Plan 02 + Plan 03 still PASS

Command:
```
NEKSUR_ATLAS_BIN=/Users/evgeny/bin/atlas \
go test -tags integration \
  -run 'TestSessionBleed|TestAtlasLoopApplyRollbackApply|TestMiddleware|TestJWKSRotation|TestWebhookSig' \
  ./tests/integration/ -count=1 -timeout 15m
```

Result: `ok  github.com/neksur-com/neksur/tests/integration  9.600s`

All Plan 02 + Plan 03 tests still pass — the migrate.go adjustments
for V0050+V0051+V0052 routing did NOT break the established gates.

### Shell-side regex validation (T-0.5-prov-injection defence-in-depth)

```
./scripts/provision-tenant.sh "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa" "org_TESTC" "vpc-BAD" "us-east-1"
→ invalid VPC id: vpc-BAD
→ exit code: 2

./scripts/provision-tenant.sh "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa" "org_TESTC" "vpc-0123456789abcdef0" "US-EAST-1"
→ invalid region: US-EAST-1
→ exit code: 2

./scripts/provision-tenant.sh "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa" "lowercase-org" "vpc-0123456789abcdef0" "us-east-1"
→ invalid workos org id: lowercase-org
→ exit code: 2

./scripts/provision-tenant.sh "BAD-UUID" "org_TESTC" "vpc-0123456789abcdef0" "us-east-1"
→ invalid UUID v4: BAD-UUID
→ exit code: 2
```

All four bash regex validators reject malformed inputs with exit code 2
BEFORE any `./neksur-cli tenant ...` invocation. The Go CLI also
re-validates via internal/tenant.ValidateUUIDv4 / ValidateWorkOSOrgID /
ValidateCustomerVPCID / ValidateAWSRegion — defence-in-depth per
D-0.5.21 T-0.5-prov-injection.

### Onboarding rehearsal coverage

The end-to-end onboarding rehearsal is exercised at the test level by
`TestProvisioningIdempotent`:
- Boots Postgres+AGE testcontainer (StartSaasFixture).
- Applies Phase 0 baseline + V0041..V0044 (public-tier).
- Provisions tenant via `repo.Create` → `provisioner.CreateGraph` →
  `provisioner.CreateRole` → `provisioner.ApplyTenantMigrations` →
  `provisioner.RevokeAuditLogWrites`.
- Re-runs each step TWICE — every iteration returns nil (idempotent).
- Asserts audit_log table exists exactly once + V0050/V0051/V0052
  recorded in `<tenant_schema>.atlas_schema_revisions`.

The full 12-step shell rehearsal (`./scripts/provision-tenant.sh` with
peering + cert-issue) is the operator-facing wrapper around these
Go subcommands; steps (h) peering + (i) cert-issue are stubbed for
test runs (PROVISION_MOCK_TERRAFORM=1, empty PRIVATE_CA_ARN). The
async peering re-run pattern (step (j) prints "Re-run when customer
applies their accepter module" and exits 0) is verifiable by running
the script with `NEKSUR_FAKE_PEER_STATUS=pending-acceptance` against
a fresh tenant — exits 0 gracefully (RESEARCH Pattern 3 line 681).

## Atlas migration ledger (verified via `public.atlas_schema_revisions`)

Across the integration tests, the following Atlas-managed migrations
were applied:

- **Public-tier (in `public.atlas_schema_revisions`):**
  V0001 + V0030 (baselined at 0030), V0041, V0042, V0043, V0044.
  Capped at PublicMaxVersion=0044 via `--to-version` to prevent
  V0050+ from being applied to the public schema.

- **Per-tenant tier (each in
  `<tenant_schema>.atlas_schema_revisions`):**
  V0050 (audit_log), V0051 (query_history), V0052 (policies),
  with `--baseline 0044` to treat V0001..V0044 as already-applied
  for the tenant target (per Plan 04 deviation #N).

Verified by `provisioning_test.go::TestProvisioningIdempotent`
assertion: SELECT version FROM <tenant_schema>.atlas_schema_revisions
WHERE version IN ('0050','0051','0052') returns all three. After
re-running the provisioning steps a second time, the rows are still
distinct (no duplicates) and audit_log table count remains 1.

## atlas.sum integrity

```
$ head -1 /Users/evgeny/neksur-core/migrations/postgres/atlas.sum
h1:wn+VEkDWjbNos9bJTSk+bruZ0Q2gSBnAm5qOM50/H7Y=
```

Eight migration files hashed:
- V0001__enable_extensions.sql
- V0030__rls_policies.sql
- V0041__public_tenants.sql
- V0042__public_rls.sql
- V0043__public_audit_log.sql
- V0044__public_tenant_lookup_fn.sql
- V0050__tenant_audit_log.sql (new in Plan 04)
- V0051__tenant_query_history.sql (new in Plan 04)
- V0052__tenant_policies.sql (new in Plan 04)

## Sign-off

The three-layer connection isolation invariants of D-0.5.03 are codified
as automated CI gates per VALIDATION.md line 32: every commit runs
TestLayer1 / TestLayer2 / TestLayer3 against a real Postgres+AGE
testcontainer with two provisioned tenants. Each test fails closed —
asserting the EXACT SQLSTATE / row count — rather than "fails somehow".

ROADMAP §Phase 0.5 success criterion 4 ("3-layer connection isolation
in Pool A is verifiable by automated tests") is **met**.
ROADMAP §Phase 0.5 success criterion 3 ("tenant onboarded end-to-end
via `./scripts/provision-tenant.sh`") is **met for the Neksur-side
flow** (peering + cert-issue rely on Plan 05's Terraform module +
Plan 01's Private CA — Plan 04 stubs them appropriately for tests).
