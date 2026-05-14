# Runbook: Customer VPC Peering (Setup + Troubleshooting)

**Owner:** Founder / on-call SRE (Phase 0.5); Customer Success + Solutions
Engineering (Phase 1+)
**Scope:** Operator runbook for the customer-facing VPC peering connection
that gives each SaaS tenant a private path from their Spark/data-plane
VPC to Neksur's pgwire NLB at `sql.neksur.com:5432`.
**Contract:** **D-0.5.06** (VPC peering — accepter-on-customer-side; no
public internet path) + **D-0.5.21** (T-0.5-vpc-peer-misconfig).
**Validation tests:** `tests/integration/pgwire_reach_test.go::TestPgwireReachableFromCustomerVPC`
(skip-gated on `AWS_SANDBOX_ENABLED=true`); operator-attested via
`runbooks/vpc-peering-sandbox-attestation.md` (Plan 07 Task 4 — REQ-saas-cloud-topology
pre-deploy gate).
**Closes:** REQ-saas-cloud-topology (peering connectivity invariant).

> **Note on the contract.** Neksur is the **requester** of the peering;
> the customer must apply an **accepter** module on their AWS account to
> complete the connection. Both sides need `allow_remote_vpc_dns_resolution=true`
> AND `enable_dns_hostnames=true` on their VPC for the Neksur RDS endpoint
> to resolve to a 10.0.0.0/16 private IP via peering (RESEARCH Pitfall 6).

---

## 1. Topology

```text
   ┌─────────────────────┐      VPC peering        ┌─────────────────────┐
   │  Customer VPC       │ ◄───────────────────► │  Neksur VPC         │
   │  (CIDR: customer-   │   (pcx-xxxxxxxxxxxx)  │  (CIDR: 10.0.0.0/16) │
   │   supplied)         │                       │                     │
   │                     │                       │  ┌──────────────┐  │
   │  ┌──────────────┐  │                       │  │ Pool A RDS   │  │
   │  │ Customer's   │  │                       │  │ (port 5432)  │  │
   │  │ Spark / data │──┼──peering route───────►│  │              │  │
   │  │ plane        │  │                       │  └──────────────┘  │
   │  └──────────────┘  │                       │                     │
   └─────────────────────┘                       └─────────────────────┘
                            ▲  Neksur SG ingress rule allows
                            │  TCP/5432 from {customer-cidr}
                            │  ONLY (per-peering scoped)
                            ▼
   No public internet path — sql.neksur.com is a private DNS record
   resolved via peering DNS (`allow_remote_vpc_dns_resolution=true`).
```

**Pre-requisites on BOTH sides:**

- `enable_dns_hostnames = true` (required for the private DNS name to resolve)
- `enable_dns_support = true`
- `allow_remote_vpc_dns_resolution = true` on the peering connection itself

If any of these are missing, the customer's `dig sql.neksur.com` will
return a **public** IP (the default), which has no route via peering —
queries appear to "fall through" to the public internet (or, more
commonly, the NAT gateway) and time out at the Neksur side because
Pool A is private-only.

---

## 2. Apply customer-side accepter (operator hands customer a template)

After `./scripts/provision-tenant.sh` step (h) completes, run step (i)
to print the customer-side Terraform module:

```bash
./neksur-cli tenant peer --tenant-uuid <uuid> --show-customer-module
```

The output is a complete Terraform module (~30 lines) that the customer
SRE can apply on their AWS account. It contains:

- `aws_vpc_peering_connection_accepter` — accepts the peering Neksur
  initiated (the `peering_connection_id` is Neksur-supplied).
- `aws_route` — adds a route from customer's route tables to
  `neksur_vpc_cidr = 10.0.0.0/16` via the peering connection.
- Tags marking the resources as `ManagedBy = "neksur-customer-peering"`.

**Operator hands the customer:**

1. The module text (copy-paste).
2. The `peering_connection_id` (e.g., `pcx-0123456789abcdef0`) printed
   by step (h).
3. The list of customer route-table IDs they need to update (the customer
   decides which of their route tables should learn the Neksur route;
   typically all subnets that host the Spark / data-plane workload).

**Customer-side apply:**

```bash
# Customer runs this on their AWS account; not Neksur.
terraform init
terraform apply \
    -var='peering_connection_id=pcx-0123456789abcdef0' \
    -var='customer_route_table_ids=["rtb-aaa", "rtb-bbb"]'
```

---

## 3. Verify peering is healthy

From a customer-side VM in a peered subnet:

```bash
# (1) DNS resolution — MUST return a 10.0.0.0/16 IP (NOT a public IP).
dig +short sql.neksur.com
# Expected output: a single A record like "10.0.1.42"
# If you see a public IP (e.g., 52.x.x.x) — RESEARCH Pitfall 6 hit;
# fix `allow_remote_vpc_dns_resolution=true` on BOTH sides.

# (2) TCP reachability — pgwire dial.
nc -zv sql.neksur.com 5432
# Expected: "Connection to sql.neksur.com 5432 port [tcp/postgres] succeeded!"

# (3) Postgres-level smoke test (if you have a tenant cert + role).
psql "postgres://tenant_<uuid>_role@sql.neksur.com:5432/postgres?sslmode=verify-full&sslrootcert=neksur-ca.crt&sslcert=client.crt&sslkey=client.key" \
    -c "SELECT 1"
```

If all three checks pass, the peering is healthy and the customer can
proceed with their integration.

The Neksur-side integration test `TestPgwireReachableFromCustomerVPC`
(Plan 05) automates steps (1)+(2) in a sandbox AWS environment; see
`runbooks/vpc-peering-sandbox-attestation.md` for the operator-attested
proof against real AWS (REQ-saas-cloud-topology pre-deploy gate).

---

## 4. Troubleshooting matrix

### 4.a Connection timed out from customer's Spark to Neksur pgwire

```
ERROR: org.postgresql.util.PSQLException: Connection to sql.neksur.com:5432 refused.
ERROR: Connection timeout: connect timed out
```

| Cause | Diagnostic | Fix |
|-------|-----------|-----|
| **Security group missing ingress** | On Neksur side: `aws ec2 describe-security-groups --group-ids <neksur-pgwire-sg>` — confirm CustomerVPCCidr in IpPermissions[].IpRanges | Re-run `terraform apply` on Neksur side; the `customer_peering` module adds an SG ingress rule per peering. |
| **Route not yet propagated** | `aws ec2 describe-route-tables --route-table-ids <customer-rtb>` — confirm 10.0.0.0/16 route via peering exists; State should be "active" | Wait 1–5 minutes for route propagation; if still failing after 5 min, re-apply customer-side module. |
| **Peering connection not yet active** | `aws ec2 describe-vpc-peering-connections --vpc-peering-connection-ids <pcx>` — Status.Code should be "active" | If Status="pending-acceptance" the customer hasn't applied their accepter; if "expired" the connection has aged out and must be re-created. |
| **Customer's NACL blocks** | Customer-side NACL on the Spark subnet | Customer adds NACL egress rule to 10.0.0.0/16; not a Neksur-side fix. |

### 4.b Queries appear in customer's NAT gateway logs (NOT via peering)

```
NAT gateway flow log shows traffic to a public IP for sql.neksur.com
```

This is the classic Pitfall 6 symptom — DNS resolution isn't going via
peering, so traffic gets routed through the NAT gateway to the public IP
of the Neksur NLB (which is not publicly accessible — connection refused).

| Cause | Diagnostic | Fix |
|-------|-----------|-----|
| `allow_remote_vpc_dns_resolution=false` on Neksur side | `aws ec2 describe-vpc-peering-connections ... --query 'VpcPeeringConnections[].AccepterVpcInfo.PeeringOptions'` | On Neksur side: `terraform apply` after setting `requester_peering_options { allow_remote_vpc_dns_resolution = true }` on the peering resource. The `modules/customer-peering` module sets this by default. |
| `allow_remote_vpc_dns_resolution=false` on customer side | Customer's peering connection options | Customer modifies their `aws_vpc_peering_connection_accepter` block to include `accepter { allow_remote_vpc_dns_resolution = true }`. |
| `enable_dns_hostnames=false` on customer VPC | `aws ec2 describe-vpc-attribute --vpc-id <customer-vpc> --attribute enableDnsHostnames` | Customer modifies their VPC: `aws ec2 modify-vpc-attribute --vpc-id <vpc> --enable-dns-hostnames`. Toggling this post-peering-apply DOES NOT require destroying the peering; safe to apply mid-flight. |

### 4.c `dig +short sql.neksur.com` returns no answer

```
$ dig +short sql.neksur.com
(empty output)
```

| Cause | Diagnostic | Fix |
|-------|-----------|-----|
| Neksur Route 53 private hosted zone not associated with customer VPC | `aws route53 list-hosted-zones-by-vpc --vpc-id <customer-vpc> --vpc-region <region>` | Neksur ops: add the customer VPC to the `aws_route53_zone_association` for the `neksur.com` private zone — this is done automatically by the `customer_peering` module via `aws_route53_zone_association`. Re-run `terraform apply` on Neksur side. |
| Customer's DNS resolver doesn't forward to AWS Route 53 | Custom DNS resolver (e.g., dnsmasq, Pi-hole) in customer VPC | Customer adds a forwarder rule for `neksur.com` zone pointing to AWS's `.2` resolver address (`<customer-vpc-cidr-first-octet+2>`). |

### 4.d VPC peering quota exceeded

```
Error: error creating VPC Peering Connection: VpcPeeringConnectionLimitExceeded
```

AWS soft quota: 50 active VPC peering connections per VPC. Plan 05 ships
`./scripts/request-vpc-peering-quota-increase.sh` for the quota increase.

---

## 5. Cross-references

- `customer-modules/customer-vpc-peering/main.tf` — customer-side
  Terraform module (the one `--show-customer-module` prints).
- `modules/customer-peering/main.tf` — Neksur-side requester module
  (Plan 05).
- `scripts/request-vpc-peering-quota-increase.sh` — quota increase
  request automation.
- `tests/integration/pgwire_reach_test.go` — automated regression test
  (skip-gated on `AWS_SANDBOX_ENABLED=true`).
- `runbooks/vpc-peering-sandbox-attestation.md` — pre-deploy operator
  attestation (REQ-saas-cloud-topology proof).
- `runbooks/saas-onboarding.md` — upstream: the 12-step onboarding
  flow that initiates peering at step (h)–(j).
- `runbooks/tenant-lifecycle.md` §3 — downstream: peering destroy
  during tenant delete.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-RESEARCH.md`
  §Common Pitfall 6 (DNS resolution via peering — lines 1129–1133).
