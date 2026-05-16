# Runbook: mTLS Certificate Rotation (Phase 2 — Manual)

**Owner:** Platform engineer — mTLS + ACM PCA (Plan 02-08)
**Scope:** Manual mTLS certificate rotation for the Phase 2 SQL proxy. Covers ACM PCA
subordinate CA renewal, server cert replacement, CA bundle republish to S3, and
hot-reload via cert_watcher.go (no server restart needed). Phase 6 will automate this
procedure (D-2.08 deferral).
**Triggers:** Certificate expiration approaching (< 30 days remaining), cert replacement
after a security incident, or `mtls_cert_expired` incident (see runbooks/sts-vending-incident.md).
**Closes:** Phase 2 operational requirement per RESEARCH §Standard Stack line 366 + D-2.08

---

## Overview

The Phase 2 mTLS infrastructure uses a two-tier PKI:

```
Root CA (RSA 2048, 10-year validity)
│  Provisioned by: modules/private-ca/main.tf (Phase 0.5)
│  Managed by: neksur-infra Terraform
│
└── Subordinate CA (RSA 4096, 5-year validity)
       Provisioned by: modules/private-ca/subordinate.tf (Phase 2 Plan 02-08)
       One per tenant (per-tenant isolation)
       │
       └── Client certs (RSA 2048 or ECDSA, 1-hour validity)
              Issued by: neksur-server /v1/credvend/sts → AWS IssueCertificate
              Used by: Spark executors to authenticate to SQL proxy
```

**Phase 6 automation:** CRL/OCSP and automated cert rotation are deferred to Phase 6
(D-2.08). For Phase 2, all rotation steps in this runbook are performed manually by a
platform engineer with IAM access to ACM PCA.

---

## Environment Variables (Cert Paths)

| Variable | Description | Source |
|----------|-------------|--------|
| `NEKSUR_TLS_CERT_PATH` | Path to TLS server certificate PEM file | Operator-placed (see §3) |
| `NEKSUR_TLS_KEY_PATH` | Path to TLS server private key PEM file | Operator-placed (see §3) |
| `NEKSUR_CA_BUNDLE_PATH` | S3 path to CA bundle (root + subordinate PEM chain) | Terraform outputs (see §4) |
| `NEKSUR_PCA_ARN` | ACM PCA subordinate CA ARN for IssueCertificate | Terraform outputs |

---

## 1. Pre-Rotation Checklist

Before starting, verify:

```bash
# 1. Identify the subordinate CA ARN for the tenant:
terraform output -raw design_partner_1_subordinate_ca_arn
# → arn:aws:acm-pca:us-east-1:<account>:certificate-authority/<uuid>

# 2. Check current CA status:
aws acm-pca describe-certificate-authority \
  --certificate-authority-arn "$(terraform output -raw design_partner_1_subordinate_ca_arn)" \
  | jq '{Status: .CertificateAuthority.Status, ExpiresAt: .CertificateAuthority.NotAfter}'
# Expected: Status = ACTIVE, ExpiresAt = <5 years from creation>

# 3. Check server cert expiry:
openssl x509 -in "$NEKSUR_TLS_CERT_PATH" -noout -enddate
# Expected: notAfter=<date more than 30 days from today>

# 4. Confirm cert_watcher is running:
kubectl exec -n neksur-system deployment/neksur-server -- \
  pgrep -fa cert_watcher
# OR check logs:
kubectl logs -n neksur-system deployment/neksur-server --since=1m | grep cert_watcher
```

---

## 2. ACM PCA Subordinate CA Renewal

Use this procedure when the subordinate CA itself is expiring (every 5 years by default,
or `var.subordinate_validity_years` as configured).

**Note:** Client certs (1-hour validity) renew automatically via neksur-server IssueCertificate
calls — no operator action needed for client cert rotation.

### 2.1 Request CA certificate renewal

```bash
CA_ARN="$(terraform output -raw design_partner_1_subordinate_ca_arn)"

# Issue the renewal request (creates a new cert signed by the root CA):
aws acm-pca issue-certificate \
  --certificate-authority-arn "$CA_ARN" \
  --csr "$(aws acm-pca get-certificate-authority-csr \
    --certificate-authority-arn "$CA_ARN" \
    --output text)" \
  --signing-algorithm SHA512WITHRSA \
  --template-arn arn:aws:acm-pca:::template/SubordinateCACertificate_PathLen0/V1 \
  --validity Value=5,Type=YEARS \
  | jq -r '.CertificateArn'
```

Record the `CertificateArn` returned.

### 2.2 Install the renewed cert on the CA

```bash
# Wait for cert to be issued (~30 seconds):
aws acm-pca wait certificate-issued \
  --certificate-authority-arn "$CA_ARN" \
  --certificate-arn "$CERT_ARN"

# Download the new cert:
aws acm-pca get-certificate \
  --certificate-authority-arn "$CA_ARN" \
  --certificate-arn "$CERT_ARN" \
  --query Certificate --output text > /tmp/subordinate-ca-new.pem

# Download the chain (subordinate + root):
aws acm-pca get-certificate \
  --certificate-authority-arn "$CA_ARN" \
  --certificate-arn "$CERT_ARN" \
  --query CertificateChain --output text > /tmp/subordinate-ca-chain-new.pem

# Install the renewed cert on the subordinate CA:
aws acm-pca import-certificate-authority-certificate \
  --certificate-authority-arn "$CA_ARN" \
  --certificate file:///tmp/subordinate-ca-new.pem \
  --certificate-chain file:///tmp/subordinate-ca-chain-new.pem
```

### 2.3 Verify CA is ACTIVE after renewal

```bash
aws acm-pca describe-certificate-authority \
  --certificate-authority-arn "$CA_ARN" \
  | jq '.CertificateAuthority.Status'
# Expected: "ACTIVE"
```

---

## 3. Server Certificate Replacement

The SQL proxy server certificate is the TLS server cert presented to Spark executors.
Rotate when:
- Cert expiry < 30 days
- Server private key compromise
- Cipher suite upgrade required

### 3.1 Generate a new server cert from the subordinate CA

```bash
CA_ARN="$(terraform output -raw design_partner_1_subordinate_ca_arn)"

# Generate a new private key:
openssl genrsa -out /tmp/neksur-server-new.key 2048

# Generate a CSR:
openssl req -new \
  -key /tmp/neksur-server-new.key \
  -subj "/CN=neksur-server/O=Neksur/C=US" \
  -out /tmp/neksur-server-new.csr

# Issue the server cert from the subordinate CA:
aws acm-pca issue-certificate \
  --certificate-authority-arn "$CA_ARN" \
  --csr fileb:///tmp/neksur-server-new.csr \
  --signing-algorithm SHA256WITHRSA \
  --template-arn arn:aws:acm-pca:::template/EndEntityServerAuthCertificate/V1 \
  --validity Value=1,Type=YEARS \
  | jq -r '.CertificateArn'
```

Record the `CertificateArn`.

### 3.2 Download and install the new server cert

```bash
# Wait for issuance:
aws acm-pca wait certificate-issued \
  --certificate-authority-arn "$CA_ARN" \
  --certificate-arn "$CERT_ARN"

# Download:
aws acm-pca get-certificate \
  --certificate-authority-arn "$CA_ARN" \
  --certificate-arn "$CERT_ARN" \
  --query Certificate --output text > /tmp/neksur-server-new.pem

# Replace the cert on the server (path from NEKSUR_TLS_CERT_PATH):
kubectl cp /tmp/neksur-server-new.pem \
  neksur-system/$(kubectl get pods -n neksur-system -l app=neksur-server -o name | head -1):"$NEKSUR_TLS_CERT_PATH"

kubectl cp /tmp/neksur-server-new.key \
  neksur-system/$(kubectl get pods -n neksur-system -l app=neksur-server -o name | head -1):"$NEKSUR_TLS_KEY_PATH"
```

**Or** via Kubernetes Secret update (recommended for production):

```bash
kubectl create secret tls neksur-tls \
  --cert=/tmp/neksur-server-new.pem \
  --key=/tmp/neksur-server-new.key \
  --namespace neksur-system \
  --dry-run=client -o yaml | kubectl apply -f -
```

The cert_watcher.go in `internal/sqlproxy/cert_watcher.go` watches the cert + key files
via fsnotify. When the Secret is updated and the file changes on-disk, cert_watcher triggers
an in-process TLS config reload within `NEKSUR_TLS_RELOAD_INTERVAL` (default 30s).

**No server restart is needed.**

### 3.3 Verify cert reload without restart

```bash
# Watch for the reload event in logs:
kubectl logs -n neksur-system deployment/neksur-server -f | grep cert_reloaded

# Expected log line (within NEKSUR_TLS_RELOAD_INTERVAL seconds):
# {"level":"info","component":"cert_watcher","event":"cert_reloaded","path":"/etc/neksur/tls/tls.crt","new_expiry":"2027-05-16T00:00:00Z"}

# Verify new cert is presented:
openssl s_client \
  -connect <host>:8443 \
  -cert /tmp/spark-executor-client.pem \
  -key /tmp/spark-executor-client.key \
  2>/dev/null | openssl x509 -noout -enddate
# Expected: notAfter = 1 year from today (new cert)
```

---

## 4. CA Bundle Republish to S3

After renewing the subordinate CA, the CA bundle (root + subordinate PEM chain) must be
republished to S3 so neksur-server can reload it at next startup.

```bash
# Build the new CA bundle (subordinate cert + root cert):
cat /tmp/subordinate-ca-new.pem > /tmp/ca-bundle-new.pem

# Append the root CA cert:
aws acm-pca get-certificate-authority-certificate \
  --certificate-authority-arn "$(terraform output -raw certificate_authority_arn)" \
  --query Certificate --output text >> /tmp/ca-bundle-new.pem

# Upload to S3:
CA_BUNDLE_PATH="$(terraform output -raw design_partner_1_ca_bundle_path)"
aws s3 cp /tmp/ca-bundle-new.pem "$CA_BUNDLE_PATH"
# Example: s3://neksur-pki-phase0-pilot/tenants/<tenant_uuid>/ca-bundle.pem

# Verify:
aws s3 cp "$CA_BUNDLE_PATH" - | openssl x509 -noout -subject -issuer
# Expected: subject and issuer show subordinate CA and root CA respectively
```

**Note:** neksur-server reads `NEKSUR_CA_BUNDLE_PATH` from S3 at startup. For a running
server, the CA bundle is loaded once at startup. To pick up a new bundle without restart:

```bash
# Hot-reload the CA bundle (Phase 6 will automate; for now, rolling restart):
kubectl rollout restart deployment/neksur-server -n neksur-system
# Zero-downtime rolling restart; NLB routes traffic to remaining replicas during restart.
```

---

## 5. Client Cert Revocation (Emergency)

In Phase 2, revocation is manual. There is no CRL or OCSP endpoint (D-2.08 Phase 6 deferral).

**Emergency revocation procedure:**

```bash
# Revoke a specific client cert by serial number:
aws acm-pca revoke-certificate \
  --certificate-authority-arn "$(terraform output -raw design_partner_1_subordinate_ca_arn)" \
  --certificate-serial "$(openssl x509 -in /tmp/compromised-client.pem -noout -serial | cut -d= -f2)" \
  --revocation-reason "KEY_COMPROMISE"
```

**Impact:** In Phase 2, revocation has NO immediate effect because there is no CRL/OCSP
distribution. The revoked cert can still be used until it expires (max 1 hour for
auto-issued certs). For a compromised cert with a long validity period:

1. Revoke the cert in ACM PCA (above).
2. Block the cert's CN (tenant UUID + executor ID) at the SQL proxy via OPA/admission control
   (Phase 6 capability).
3. For immediate blocking: add the cert serial to the `blocked_certs` table (Phase 2 escape
   hatch — documented in `internal/sqlproxy/auth.go`).

---

## 6. Post-Rotation Validation

```bash
# Full end-to-end validation after cert rotation:

# 1. SQL proxy serves new cert:
openssl s_client -connect <host>:8443 -CAfile /tmp/ca-bundle-new.pem 2>/dev/null \
  | openssl x509 -noout -enddate
# Expected: notAfter = new expiry date

# 2. Client cert issuance still works:
neksur-cli credvend test \
  --tenant-id <tenant_uuid> \
  --table warehouse.customers
# Expected: "STS credentials vended successfully; expires in 3600s"

# 3. Spark can connect:
# Run a minimal Spark job that reads from a governed table via the SQL proxy.
# Expected: job completes without mTLS handshake errors.

# 4. Row-filter + column-mask still applied:
curl -k --cert /tmp/spark-executor-client.pem --key /tmp/spark-executor-client.key \
  -X POST "https://<host>:8443/v1/sql/trino/warehouse/customers" \
  -d '{"query": "SELECT ssn FROM customers LIMIT 1"}' | jq
# Expected: ssn shows as "XXX-XX-LAST4" (masked), not plaintext
```

---

## 7. Phase 6 Automation Deferral (D-2.08)

The following automation is planned for Phase 6 and is **not available in Phase 2**:

| Automation | Phase | Notes |
|------------|-------|-------|
| CRL distribution (S3 + CloudFront CDN) | 6 | Enables real-time revocation for client certs |
| OCSP responder (Lambda-backed) | 6 | Provides real-time revocation status to clients |
| Automated server cert renewal (ACM + Lambda trigger) | 6 | Eliminates manual rotation for server certs |
| Automated CA bundle republish (EventBridge + Lambda) | 6 | Triggers on CA renewal event |
| Cert expiry alerting (CloudWatch alarms on ACM PCA) | 6 | 30-day and 7-day expiry warnings |

Until Phase 6, operators must:
- Monitor cert expiry manually via `aws acm-pca describe-certificate-authority`.
- Set calendar reminders for subordinate CA expiry (default: 5 years from provisioning date).
- Follow this runbook for all rotation steps.

---

## References

- **D-2.08** — Per-tenant mTLS client cert issuance + Phase 6 automation deferral.
- **ACM PCA documentation** — https://docs.aws.amazon.com/acm-pca/latest/userguide/
- **modules/private-ca/README.md** — ACM PCA Terraform module + CA bundle upload procedure.
- **environments/phase0-pilot/private-ca-subordinate.tf** — Terraform outputs for CA ARNs.
- **runbooks/sts-vending-incident.md** — STS vending incident response (includes mTLS cert expiry path).
- **runbooks/sql-proxy-deploy.md** — SQL proxy deployment + readyz check.
- **Plan 02-05 SUMMARY** — `internal/sqlproxy/cert_watcher.go` hot-reload implementation.
- **Plan 02-08 Task 1** — ACM PCA Terraform module implementation.

---

*Phase 2 mTLS cert rotation runbook — Phase 2 Plan 02-08 Task 2. Phase 6 will automate.*
