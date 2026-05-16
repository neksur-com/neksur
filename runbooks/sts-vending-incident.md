# Runbook: STS Credential Vending Incident Response (Phase 2)

**Owner:** Platform engineer — L4 Credential Vending (Plan 02-07)
**Scope:** Diagnosing and resolving incidents for the L4 STS credential vending service
(`/v1/credvend/sts`). Covers Pitfall 1 (session policy malformed Resource), Pitfall 7
(Polaris loadTable response config keys), Pitfall 10 (KMS rate exhaustion), and the
`L4VendingFailureSpike` + `KMSThrottlingWarn` alerts.
**Triggers:** L4VendingFailureSpike alert (observability/rules/phase2-l4-vending.yml)
**Closes:** Phase 2 operational requirement per RESEARCH §Standard Stack line 364

---

## When This Fires

The `L4VendingFailureSpike` alert fires when:

```
rate(l4_token_failures_total[5m]) > 5
```

Sustained for 5 minutes → `severity=page` → PagerDuty oncall wake.

The `KMSThrottlingWarn` alert fires when:

```
increase(kms_generate_data_key_total{cache_status="error"}[5m]) > 10
```

Sustained for 5 minutes → `severity=warn` → Slack #oncall-warn.

**Impact:** When `l4_token_failures_total` is high, Spark executors cannot obtain STS
credentials for the table they're trying to access. They receive HTTP 500 from
`/v1/credvend/sts` and fail fast (fail-secure design per D-2.09). Spark jobs fail with
`CredentialVendingException`; data pipeline is blocked.

---

## Environment Variables (L4 Vending Service)

The STS vending path in `cmd/neksur-server` is controlled by these env vars:

| Variable | Description | Source |
|----------|-------------|--------|
| `NEKSUR_KMS_ROLE_ARN` | IAM role neksur-server assumes for KMS operations | Terraform outputs (modules/kms-key) |
| `NEKSUR_PCA_ARN` | ACM PCA subordinate CA ARN for mTLS client cert issuance | Terraform outputs (modules/private-ca subordinate) |
| `NEKSUR_TLS_CERT_PATH` | Path to TLS server certificate file (watched by cert_watcher.go) | Operator-provided (see runbooks/mtls-cert-rotation.md) |
| `NEKSUR_POLARIS_VENDING_ENABLED` | Enables Polaris-backed STS vending; if false, falls back to stub | `"true"` in prod, `"false"` in local dev |
| `NEKSUR_CA_BUNDLE_PATH` | S3 path to CA bundle (root + subordinate PEM chain) | Terraform outputs (modules/private-ca ca_bundle_path) |

Verify they are set on the running neksur-server container:

```bash
kubectl exec -n neksur-system deployment/neksur-server -- env | grep NEKSUR_
```

---

## 1. Identify the Failure Type

### 1.1 Query the metric by label

```promql
topk(10, sum by (tenant_id, error_type) (
  rate(l4_token_failures_total[5m])
))
```

Common `error_type` label values:

| `error_type` | Root cause |
|--------------|------------|
| `session_policy_malformed` | Pitfall 1 — Resource string-vs-array |
| `polaris_config_key_not_found` | Pitfall 7 — Polaris version or config key mismatch |
| `kms_throttle` | Pitfall 10 — KMS GenerateDataKey quota exhausted |
| `sts_assume_role_denied` | IAM policy on `NEKSUR_KMS_ROLE_ARN` missing `sts:AssumeRole` |
| `table_not_found` | Polaris loadTable returned 404; catalog not configured |
| `mtls_cert_expired` | mTLS client cert presented by Spark executor is expired |

### 1.2 Check OTel trace for the failing request

Find the trace_id in CloudWatch Logs or the OTel collector:

```bash
# CloudWatch Insights query for L4 failures in last 30 min:
aws logs filter-log-events \
  --log-group-name "/neksur/server" \
  --filter-pattern '{ $.level = "error" && $.component = "credvend" }' \
  --start-time $(date -d '30 minutes ago' +%s)000 \
  | jq '.events[].message | fromjson | {trace_id, error, tenant_id}'
```

Record the `trace_id` and follow it through the Jaeger/OTel trace for the full call path.

---

## 2. Pitfall 1 — Session Policy Malformed Resource String

**Symptom:** `error_type=session_policy_malformed` in the metric; Spark job fails with:

```
com.neksur.credvend.CredentialVendingException: STS AssumeRole returned 400
Invalid session policy: JSON parse error: array expected at Resource
```

**Root cause:** AWS STS session policy `Resource` field MUST be a JSON array, not a string:

```json
// BROKEN — Resource is a scalar string:
{
  "Effect": "Allow",
  "Action": ["s3:GetObject"],
  "Resource": "arn:aws:s3:::my-bucket/warehouse/customers/*"
}

// CORRECT — Resource is an array:
{
  "Effect": "Allow",
  "Action": ["s3:GetObject"],
  "Resource": ["arn:aws:s3:::my-bucket/warehouse/customers/*"]
}
```

**Diagnostic:**

```bash
# Dump the session policy for a table to inspect it:
neksur-cli credvend dump-session-policy \
  --tenant-id <tenant_uuid> \
  --table warehouse.customers \
  --region us-east-1
```

Check the output `Resource` field: must be a JSON array `[...]`, not a string `"..."`.

**Fix:**

The session policy builder is in `internal/credvend/session_policy.go`. Search for the
`resourceArn` variable and ensure it's wrapped in `[]string{...}` before JSON marshaling.

After fixing, restart neksur-server (`kubectl rollout restart deployment/neksur-server -n neksur-system`).

**Verification:**

```bash
# Call the endpoint directly with curl (from inside the cluster):
curl -X POST http://localhost:8443/v1/credvend/sts \
  -H "Content-Type: application/json" \
  -d '{"tenant_id": "<uuid>", "table": "warehouse.customers", "region": "us-east-1"}' | jq
# Expected: 200 with { "access_key_id": "...", "secret_access_key": "...", "session_token": "..." }
```

---

## 3. Pitfall 7 — Polaris loadTable Config Key Mismatch

**Symptom:** `error_type=polaris_config_key_not_found`; `l4_token_failures_total` spike
correlates with a Polaris version upgrade or config change.

**Root cause:** The Phase 2 Polaris STS vending path parses `loadTable` response config
to extract S3 endpoint info. Polaris 1.4+ changed the config key names:

| Key (Polaris < 1.4) | Key (Polaris >= 1.4) | Value |
|---------------------|----------------------|-------|
| `s3.access-key-id` | `s3.access-key-id` | AWS access key (unchanged) |
| `s3.secret-access-key` | `s3.secret-access-key` | AWS secret (unchanged) |
| `s3.endpoint` | `s3.endpoint-override` | S3-compatible endpoint URL (**changed**) |
| `s3.region` | `s3.region` | AWS region (unchanged) |

**Diagnostic:**

```bash
# Check the Polaris version:
neksur-cli polaris version --catalog <catalog_name>
# Expected: Polaris 1.4.0 or later.

# Dump the raw loadTable response config:
neksur-cli credvend polaris-config \
  --tenant-id <tenant_uuid> \
  --table warehouse.customers
# Look for 's3.endpoint' vs 's3.endpoint-override'.
```

**Fix option A (Polaris >= 1.4):** Update `internal/credvend/polaris_client.go` to read
`s3.endpoint-override` instead of `s3.endpoint`. Update the config key constant.

**Fix option B (Polaris not upgraded):** Pin Polaris to the version that matches the config
keys the code expects. File a Polaris ticket for the endpoint key rename if unexpected.

**Fix option C (fallback to direct STS):** Set `NEKSUR_POLARIS_VENDING_ENABLED=false` to
fall back to direct AWS STS AssumeRole. This bypasses Polaris entirely and uses the role ARN
in `NEKSUR_KMS_ROLE_ARN` for table-scoped session policies. Performance impact: no Polaris
caching, slightly higher latency (~50ms).

**Verification after fix:**

```bash
neksur-cli credvend test --tenant-id <tenant_uuid> --table warehouse.customers
# Expected: "STS credentials vended successfully; expires in 3600s"
```

---

## 4. Pitfall 10 — KMS Rate Exhaustion

**Symptom:** `KMSThrottlingWarn` alert fires (`severity=warn`); metric shows
`kms_generate_data_key_total{cache_status="error"}` increasing; Spark jobs fail with:

```
com.neksur.credvend.CredentialVendingException: KMS GenerateDataKey throttled: ThrottlingException
```

**Root cause:** AWS KMS GenerateDataKey has a default quota of 5,000 requests per second per
region (applies to the entire AWS account). Heavy Spark workloads with many concurrent
executors can exhaust this quota.

**Immediate mitigation — raise the batch cache TTL:**

The KMS call result is cached in `internal/credvend/kms_cache.go` (BatchCache). Raising the
TTL from the default 10 minutes to 30 minutes reduces KMS call frequency by 3×:

```bash
# Override via env var:
kubectl set env deployment/neksur-server -n neksur-system \
  NEKSUR_KMS_CACHE_TTL_SECONDS=1800

# Rolling restart (zero-downtime):
kubectl rollout restart deployment/neksur-server -n neksur-system
```

Observe `rate(kms_generate_data_key_total[5m])` — should drop within 5 minutes.

**Medium-term mitigation — request quota increase:**

```bash
# Check current quota:
aws service-quotas get-service-quota \
  --service-code kms \
  --quota-code L-2AC2B0B4 \
  --region us-east-1 | jq '.Quota.Value'

# Request increase (example: to 10,000/sec):
aws service-quotas request-service-quota-increase \
  --service-code kms \
  --quota-code L-2AC2B0B4 \
  --desired-value 10000 \
  --region us-east-1
```

AWS typically approves KMS quota increases within 24 hours for production workloads.

**Check key rotation status (housekeeping, not blocking):**

```bash
aws kms describe-key-rotation \
  --key-id $(kubectl get secret neksur-kms-key-id -n neksur-system -o jsonpath='{.data.key_id}' | base64 -d)
```

Ensure `KeyRotationEnabled: true` (Phase 0.5 requirement). Non-blocking for this incident.

**Verification:**

```promql
# KMS throttle errors should drop to 0:
increase(kms_generate_data_key_total{cache_status="error"}[5m])
```

---

## 5. IAM / STS AssumeRole Failures

**Symptom:** `error_type=sts_assume_role_denied` with AWS error `AccessDenied: User is not authorized to assume role`.

**Diagnostic:**

```bash
# Verify the IAM role ARN is correct:
echo $NEKSUR_KMS_ROLE_ARN
# Expected: arn:aws:iam::<account_id>:role/neksur-server-role

# Test assumption manually:
aws sts assume-role \
  --role-arn "$NEKSUR_KMS_ROLE_ARN" \
  --role-session-name debug-session \
  --duration-seconds 900 | jq .Credentials
```

If `assume-role` fails, check:
1. The EC2 instance profile attached to the neksur-server host has `sts:AssumeRole` on `NEKSUR_KMS_ROLE_ARN`.
2. The `NEKSUR_KMS_ROLE_ARN` trust policy allows the instance profile ARN as a principal.
3. CloudTrail events for `AssumeRole` denied (search for `ErrorCode=AccessDenied`).

---

## 6. mTLS Client Cert Expired

**Symptom:** `error_type=mtls_cert_expired`; Spark executor logs show
`x509: certificate has expired or is not yet valid`.

**Action:** Follow `runbooks/mtls-cert-rotation.md` for the certificate rotation procedure.
This is an out-of-band operational action — the cert must be renewed before Spark jobs can
reconnect.

---

## 7. Escalation

| Condition | Action |
|-----------|--------|
| L4VendingFailureSpike > 15m, all tables affected | Incident bridge; check `NEKSUR_POLARIS_VENDING_ENABLED` toggle |
| Pitfall 7 config key mismatch after Polaris upgrade | Rollback Polaris OR set fallback flag; PR to fix config key |
| KMS throttle + cache TTL raise didn't resolve in 30m | Request quota increase (§4); consider horizontal scaling |
| IAM AssumeRole denied | Platform team + AWS Support if quota/policy dispute |
| mTLS cert expired | Follow runbooks/mtls-cert-rotation.md immediately |

---

## References

- **D-2.09** — L4 credential vending architecture (Polaris STS live; Unity stub).
- **Pitfall 1** — Resource string-vs-array session policy bug.
- **Pitfall 7** — Polaris loadTable config key names.
- **Pitfall 10** — KMS rate limits.
- **observability/rules/phase2-l4-vending.yml** — L4VendingFailureSpike + KMSThrottlingWarn alert definitions.
- **runbooks/mtls-cert-rotation.md** — mTLS cert rotation procedure.
- **Plan 02-07 SUMMARY** — L4 credential vending implementation details.

---

*Phase 2 STS vending incident runbook — Phase 2 Plan 02-08 Task 2.*
