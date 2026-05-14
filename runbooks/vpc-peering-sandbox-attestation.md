# Runbook: VPC Peering Sandbox Attestation — PENDING

**Status:** PENDING — sandbox test not yet executed.
**Owner:** Founder / on-call SRE (executor) + independent reviewer
**Scope:** Operator-driven attestation that
`TestPgwireReachableFromCustomerVPC` (Plan 05
`tests/integration/pgwire_reach_test.go`) executes end-to-end against
a real AWS sandbox with BOTH Neksur-side
(`modules/customer-peering`) AND customer-side
(`customer-modules/customer-vpc-peering`) Terraform applied.
**Contract:** **D-0.5.21** mandatory pre-deploy gate +
**REQ-saas-cloud-topology** proof ("pgwire reachable from peered customer
subnet with no public-internet path"). The CI-side test is `t.Skip()`-gated
on `AWS_SANDBOX_ENABLED=true` because LocalStack cannot fully simulate
VPC peering DNS resolution; this attestation closes the un-skip gap.
**Validation tests:** `tests/integration/pgwire_reach_test.go::TestPgwireReachableFromCustomerVPC`
when `AWS_SANDBOX_ENABLED=true`.
**Closes:** `00.5-VALIDATION.md` Manual-Only Verifications line 52;
referenced by `00.5-ACCEPTANCE.md` §8 Sign-off Checklist as a hard
pre-deploy gate. REQ-saas-cloud-topology cannot flip to PASS until
this attestation is signed.
**Prerequisite:** `runbooks/dr-drill-m3-attestation.md` with PASS verdict.

> **Operator instructions.** This file is a TEMPLATE. Fill in the
> `[TBD]` markers as the sandbox drill proceeds, then commit the
> completed file with `docs(00.5-07-sandbox-vpc): attestation`. Until
> committed and reviewed, the status MUST remain `PENDING`.

---

## Sandbox setup

| Field | Value |
|-------|-------|
| AWS sandbox account ID | [TBD — must NOT be production pilot account 964775859511] |
| Sandbox region | [TBD — e.g., us-east-1] |
| Sandbox Neksur VPC ID | [TBD — from `terraform output -json | jq -r .vpc_id.value`] |
| Sandbox Neksur VPC CIDR | 10.0.0.0/16 (canonical; do NOT change) |
| Customer-side test VPC ID | [TBD — sandbox-created VPC simulating a customer] |
| Customer-side test VPC CIDR | [TBD — must not overlap 10.0.0.0/16; e.g., 172.20.0.0/16] |
| Customer-side test account ID | [TBD — can be the SAME sandbox account if simulating cross-VPC-same-account; production will be cross-account] |
| Sandbox tenant UUID | [TBD — generate via uuidgen] |
| Test instance type for customer-side VM | [TBD — e.g., t3.micro] |

---

## Pre-execution checklist

| Item | Status | Verification |
|------|--------|--------------|
| `modules/customer-peering/main.tf` exists | [TBD] | `test -f /Users/evgeny/neksur-infra/modules/customer-peering/main.tf` |
| `customer-modules/customer-vpc-peering/main.tf` exists | [TBD] | `test -f /Users/evgeny/neksur-infra/customer-modules/customer-vpc-peering/main.tf` |
| `tests/integration/pgwire_reach_test.go` exists | [TBD] | `test -f /Users/evgeny/neksur-core/tests/integration/pgwire_reach_test.go` |
| `runbooks/dr-drill-m3-attestation.md` with PASS verdict | [TBD] | `grep '^- RTO verdict: PASS' runbooks/dr-drill-m3-attestation.md` |
| Sandbox AWS profile available | [TBD] | `AWS_PROFILE=neksur-sandbox aws sts get-caller-identity` returns sandbox account ID |
| Executor identity | [TBD — operator email] | |
| Independent reviewer identity | [TBD — separate human, NOT executor] | |

---

## Test execution

### Step 1 — apply Neksur-side peering Terraform

```bash
cd /Users/evgeny/neksur-infra/environments/phase0-pilot

# Build a sandbox.tfvars with the test tenant + customer VPC info.
cat > sandbox.tfvars <<EOF
tenant_peerings = {
  "<sandbox-tenant-uuid>" = {
    customer_vpc_id     = "<customer-test-vpc-id>"
    customer_region     = "<sandbox-region>"
    customer_account    = "<customer-side-account-id>"
    customer_vpc_cidr   = "<customer-vpc-cidr>"
  }
}
EOF

AWS_PROFILE=neksur-sandbox terraform apply \
    -target='module.customer_peering["<sandbox-tenant-uuid>"]' \
    -var-file=sandbox.tfvars \
    -auto-approve
```

**Expected output:** `Apply complete! Resources: N added, 0 changed, 0 destroyed.`
**Actual output:** [TBD — paste tail of terraform apply output]
**Peering connection ID:** [TBD — from `terraform output -json | jq -r .customer_peerings.value`]

---

### Step 2 — apply customer-side accepter Terraform (simulated)

The customer-side module lives at
`/Users/evgeny/neksur-infra/customer-modules/customer-vpc-peering/`.
Apply it against the customer-side test VPC.

```bash
# Generate sandbox-customer.tfvars from the Neksur-side outputs.
NEKSUR_CLI=/Users/evgeny/neksur-core/neksur-cli
$NEKSUR_CLI tenant peer \
    --tenant-uuid <sandbox-tenant-uuid> \
    --show-customer-module \
    > /tmp/customer-module.tf

# Copy into a working directory (simulating what we'd hand the customer).
cd /tmp/sandbox-customer-side
cp /tmp/customer-module.tf main.tf

AWS_PROFILE=neksur-sandbox terraform init
AWS_PROFILE=neksur-sandbox terraform apply \
    -var='peering_connection_id=<pcx-from-step-1>' \
    -var='customer_route_table_ids=["<customer-test-rtb-id>"]' \
    -auto-approve
```

**Expected output:** `Apply complete!` + `aws_vpc_peering_connection_accepter.neksur:
Creation complete` line.
**Actual output:** [TBD]
**Customer-side accepter ID:** [TBD]

---

### Step 3 — wait for peering status = `active`

```bash
PCX_ID='<pcx-id-from-step-1>'
while true; do
  STATUS=$(AWS_PROFILE=neksur-sandbox aws ec2 describe-vpc-peering-connections \
      --vpc-peering-connection-ids "$PCX_ID" \
      --query 'VpcPeeringConnections[0].Status.Code' --output text)
  echo "peering status: $STATUS"
  [ "$STATUS" = "active" ] && break
  sleep 5
done
```

**Time to active:** [TBD — seconds from accepter apply to active]
**Status post-wait:** [TBD — must be `active`]

---

### Step 4 — DNS resolution check (RESEARCH Pitfall 6)

```bash
# From a VM in the customer-side test subnet (provisioned by the
# customer-vpc-peering module, OR a manually-provisioned t3.micro):
ssh -i ~/.ssh/sandbox.pem ec2-user@<customer-vm-public-ip>

# On the customer VM:
dig +short <neksur-rds-endpoint>
```

**Expected output:** A single A record in `10.0.0.0/16` (e.g., `10.0.1.42`).
A public IP (52.x.x.x) means RESEARCH Pitfall 6 hit — fix
`allow_remote_vpc_dns_resolution=true` on BOTH sides before continuing.

**Actual dig output:**

```
[TBD — paste exact `dig +short` output; MUST contain a 10.0.0.0/16 IP]
```

**Resolved IP within 10.0.0.0/16?** [TBD — yes/no]

---

### Step 5 — run TestPgwireReachableFromCustomerVPC

```bash
# On the customer-side VM, with the Neksur core repo checked out:
cd /opt/neksur-core
export AWS_SANDBOX_ENABLED=true
export NEKSUR_PGWIRE_HOST='<neksur-rds-endpoint>'

go test -tags integration -run TestPgwireReachableFromCustomerVPC \
    ./tests/integration/ -v -timeout 5m
```

**Expected exit code:** 0
**Expected test output:** `PASS: TestPgwireReachableFromCustomerVPC`
plus the log line
`resolved <host> → 10.0.X.X (in 10.0.0.0/16, via peering DNS — Pitfall 6 OK)`
and `TCP dial <host>:5432 succeeded`.

**Actual exit code:** [TBD]
**Actual output:** [TBD — paste full `go test -v` output]
**Wall-clock time:** [TBD — from test output]

---

### Step 6 — capture pcap evidence of pgwire roundtrip

In a second terminal on the customer VM, run tcpdump during the test
to capture proof that pgwire packets traverse the peering connection
(NOT the NAT gateway):

```bash
# Terminal 2 (run BEFORE Step 5):
sudo tcpdump -i any -w /tmp/pgwire-sandbox.pcap port 5432 &
TCPDUMP_PID=$!

# (Run Step 5 in Terminal 1.)

# After Step 5 completes:
sudo kill $TCPDUMP_PID

# Analyze the pcap.
tcpdump -r /tmp/pgwire-sandbox.pcap -nn -c 5
```

**Expected pcap content:** packets show source IP in customer VPC CIDR
(e.g., 172.20.X.X), destination IP in 10.0.0.0/16 (Neksur VPC). NO
packets to a public IP (no 52.x.x.x destination).

**Actual pcap excerpt (first 5 packets):**

```
[TBD — paste tcpdump -r output; MUST show customer-VPC → 10.0.0.0/16 packets]
```

**Pcap file SHA256:** [TBD — `sha256sum /tmp/pgwire-sandbox.pcap`]

---

### Step 7 — verify no public-internet path

From a NON-peered VPC (e.g., the default VPC in the sandbox account):

```bash
# From a VM in the default (non-peered) sandbox VPC:
dig +short <neksur-rds-endpoint>
# Expected: no answer OR a public IP that is NOT routable (Neksur RDS
# is private-only; even the public DNS A record points at 10.x.x.x).
```

**Actual dig output:** [TBD — paste output; expect empty OR un-routable]
**Counter-test:** [TBD — `nc -zv <neksur-rds-endpoint> 5432` from non-peered
VPC MUST time out or fail]

---

## Findings

[TBD — operator describes any deviation from the runbook. Examples:]

> "Step 4 dig output initially returned a public IP because the
> customer VPC's `enable_dns_hostnames` was false. Toggled it to true
> via `aws ec2 modify-vpc-attribute --enable-dns-hostnames` (no peering
> rebuild required), waited 60s for DNS cache to clear, re-ran dig —
> got 10.0.1.42 as expected. Updated runbooks/vpc-peering.md §4.b
> troubleshooting matrix to add a 'wait 60s for DNS cache' note."

---

## Teardown

```bash
# Step 1 — destroy customer-side accepter first (Neksur-side destroy
# will fail if accepter still exists; customer-side has the dependent
# routes).
cd /tmp/sandbox-customer-side
AWS_PROFILE=neksur-sandbox terraform destroy -auto-approve

# Step 2 — destroy Neksur-side peering.
cd /Users/evgeny/neksur-infra/environments/phase0-pilot
AWS_PROFILE=neksur-sandbox terraform destroy \
    -target='module.customer_peering["<sandbox-tenant-uuid>"]' \
    -var-file=sandbox.tfvars \
    -auto-approve

# Step 3 — confirm no stranded peering.
AWS_PROFILE=neksur-sandbox aws ec2 describe-vpc-peering-connections \
    --filters 'Name=status-code,Values=active' \
    --query 'VpcPeeringConnections[?Tags[?Key==`Name` && contains(Value, `sandbox-test`)]]'
# Expected: empty array.

# Step 4 — confirm no stranded EC2 / RDS instances (sandbox cost guard).
AWS_PROFILE=neksur-sandbox aws ec2 describe-instances \
    --filters 'Name=tag:Name,Values=neksur-sandbox-*' \
              'Name=instance-state-name,Values=running'
# Expected: empty.
```

**Teardown wall-clock:** [TBD]
**Stranded-cost check:** [TBD — confirm empty queries above; if not
empty, manually delete + record cost burn]

---

## Verdict

**Test passed (Steps 1–7 all expected):** [TBD — yes / no]
**Pcap proves peering traversal (NO NAT-gateway IP):** [TBD — yes / no]
**Counter-test from non-peered VPC fails as expected:** [TBD — yes / no]

**Overall verdict:** [TBD — PASS / PARTIAL / FAIL]

If PASS: REQ-saas-cloud-topology Sign-off Checklist row in
`00.5-ACCEPTANCE.md` §8 flips to PASS.
If PARTIAL or FAIL: file follow-up ticket; production DNS cutover
BLOCKED until resolved.

---

## Sign-off

The VPC peering sandbox attestation is closed only when BOTH executor
and reviewer have signed. D-0.5.21 mandates dual sign-off — independent
reviewer re-runs Step 4 (dig output) and re-verifies the peering
connection is `active` in the AWS console.

Attested: [TBD — executor name + date]

## Executor:

[TBD — executor email]  Date: [TBD]

## Reviewer:

[TBD — independent reviewer email (NOT executor)]  Date: [TBD]

---

## Cross-references

- `tests/integration/pgwire_reach_test.go` — the t.Skip()-gated test
  this attestation un-gates (Plan 05).
- `modules/customer-peering/main.tf` — Neksur-side requester module
  (Plan 05).
- `customer-modules/customer-vpc-peering/main.tf` — customer-side
  accepter module (Plan 05).
- `runbooks/vpc-peering.md` — operator setup + troubleshooting (Plan 07
  Task 2).
- `runbooks/dr-drill-m3-attestation.md` — UPSTREAM prerequisite.
- `runbooks/pen-test-phase-0.5.md` — parallel attestation (D-0.5.21
  Attempt 6 stresses the same network topology as this attestation).
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-RESEARCH.md`
  §Common Pitfall 6 lines 1129-1133 (DNS resolution) + §Pattern 4
  lines 695-807 (peering shape).
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-VALIDATION.md`
  Manual-Only Verifications line 52.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-ACCEPTANCE.md`
  §8 — Sign-off Checklist row for this attestation.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-REQUIREMENTS.md`
  REQ-saas-cloud-topology coverage row.
