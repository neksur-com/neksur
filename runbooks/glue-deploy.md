# Runbook: AWS Glue Iceberg REST Deployment (Neksur Policy Gateway)

**Owner:** Customer Engineering / Platform Engineering
**Scope:** Configure AWS Glue (Iceberg REST catalog endpoint) to route through the
Neksur L1 gateway. Covers IAM role setup, SigV4 signing requirements, Lake Formation
interaction (Pitfall 3), LocalStack vs real-Glue environment shapes, and the nightly
CI PENDING_FIRST_RUN gate.
**Closes:** Phase 3 Plan 03-04 (Glue live adapter); 03-VALIDATION.md §Per-engine
SQL probe semantics (Glue leg).

---

## 1. Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **AWS account** | With Glue + S3 + IAM access | `aws sts get-caller-identity` |
| **Glue catalog** | Iceberg tables registered | `aws glue get-databases` + `aws glue get-tables --database-name ...` |
| **IAM role** | With Glue + S3 permissions (see §3) | `aws iam get-role --role-name NeksurGlueRole` |
| **Neksur gateway** | Running + reachable from AWS | `curl https://neksur-gateway:8080/v1/config` returns 200 |
| **Lake Formation** | Check if LF is enabled on the target catalog (§4) | `aws lakeformation describe-resource` |

---

## 2. Glue Iceberg REST Architecture

Per Plan 03-04 (D-3.02), the Neksur Glue adapter uses:

```
AWS Glue Iceberg REST endpoint:
  https://glue.<region>.amazonaws.com/iceberg

Authentication: AWS SigV4 with:
  signing-name=glue
  rest.sigv4-enabled=true
```

The Neksur Glue adapter (`internal/iceberg/glue/`) wraps a SigV4 `RoundTripper`
around iceberg-go's REST catalog client. From AWS Glue's perspective, the Neksur gateway
issues authenticated requests to the Glue Iceberg REST API on behalf of the customer's
query engine. Policy enforcement (row-filter, column-mask, write-coordinator) is applied
at the Neksur L1 gateway layer before requests reach Glue.

---

## 3. IAM Role Configuration

### 3.1 Create the Neksur Glue IAM role

```bash
# Create the role with a trust policy for EC2 instance profile (or EKS ServiceAccount):
aws iam create-role \
    --role-name NeksurGlueRole \
    --assume-role-policy-document '{
        "Version": "2012-10-17",
        "Statement": [{
            "Effect": "Allow",
            "Principal": {"Service": "ec2.amazonaws.com"},
            "Action": "sts:AssumeRole"
        }]
    }'
```

For EKS (IRSA), use a OIDC trust policy instead of EC2.

### 3.2 Attach Glue + S3 permissions

```bash
# Attach the Glue read policy:
aws iam put-role-policy \
    --role-name NeksurGlueRole \
    --policy-name NeksurGlueIcebergPolicy \
    --policy-document '{
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": [
                    "glue:GetDatabase", "glue:GetDatabases",
                    "glue:GetTable", "glue:GetTables",
                    "glue:GetPartition", "glue:GetPartitions",
                    "glue:GetUserDefinedFunction",
                    "glue:BatchGetPartition"
                ],
                "Resource": [
                    "arn:aws:glue:<region>:<account-id>:catalog",
                    "arn:aws:glue:<region>:<account-id>:database/<your-database>",
                    "arn:aws:glue:<region>:<account-id>:table/<your-database>/*"
                ]
            },
            {
                "Effect": "Allow",
                "Action": [
                    "s3:GetObject", "s3:ListBucket",
                    "s3:GetBucketLocation"
                ],
                "Resource": [
                    "arn:aws:s3:::<your-iceberg-bucket>",
                    "arn:aws:s3:::<your-iceberg-bucket>/*"
                ]
            }
        ]
    }'
```

### 3.3 Set the IAM role on the Neksur gateway

```bash
# For EC2 instance profile:
aws iam add-role-to-instance-profile \
    --instance-profile-name NeksurGlueInstanceProfile \
    --role-name NeksurGlueRole

# Attach the instance profile to the Neksur gateway EC2 instance:
aws ec2 associate-iam-instance-profile \
    --instance-id <neksur-gateway-instance-id> \
    --iam-instance-profile Name=NeksurGlueInstanceProfile
```

The Neksur Glue adapter uses `aws/credentials` default chain (EC2 metadata, EKS IRSA,
or environment variables). Set `AWS_REGION` on the gateway host.

---

## 4. Pitfall 3 — Lake Formation Interaction

Per 03-RESEARCH.md Pitfall 3: when AWS Lake Formation is enabled on the Glue catalog,
the Glue Iceberg REST API requires **Lake Formation permissions** in addition to IAM
permissions. IAM-only permissions return `AccessDeniedException` even with Glue read
grants.

**Symptom:**

```
level=error msg="glue adapter: AccessDeniedException" 
  action="GetTable" 
  resource="arn:aws:glue:us-east-1:123456789:table/prod_catalog/orders"
  message="User: arn:aws:sts::123456789:assumed-role/NeksurGlueRole/... 
           is not authorized to perform: lakeformation:GetDataAccess"
```

**Diagnosis:**

```bash
# Check if Lake Formation is enabled on the catalog:
aws lakeformation describe-resource \
    --resource-arn "arn:aws:s3:::<your-iceberg-bucket>"
# If this returns a resource, Lake Formation is active.

# Check current Lake Formation permissions for the Neksur role:
aws lakeformation list-permissions \
    --principal DataLakePrincipalIdentifier="arn:aws:iam::<account-id>:role/NeksurGlueRole"
```

**Fix: Grant Lake Formation permissions to NeksurGlueRole**

```bash
# Grant SELECT on the target database + tables:
aws lakeformation grant-permissions \
    --principal DataLakePrincipalIdentifier="arn:aws:iam::<account-id>:role/NeksurGlueRole" \
    --permissions SELECT DESCRIBE \
    --resource '{
        "Table": {
            "DatabaseName": "prod_catalog",
            "Name": "orders",
            "CatalogId": "<account-id>"
        }
    }'

# For all tables in a database:
aws lakeformation grant-permissions \
    --principal DataLakePrincipalIdentifier="arn:aws:iam::<account-id>:role/NeksurGlueRole" \
    --permissions SELECT DESCRIBE \
    --resource '{
        "Database": {
            "Name": "prod_catalog",
            "CatalogId": "<account-id>"
        }
    }'
```

**Disable Lake Formation for testing (only if acceptable in your environment):**

```bash
# This removes LF-controlled access from the Glue catalog — use only in dev/staging:
aws lakeformation deregister-resource \
    --resource-arn "arn:aws:s3:::<your-iceberg-bucket>"
```

---

## 5. LocalStack vs Real-Glue Environment Shapes

The Neksur Phase 3 CI testcontainer uses **LocalStack** with `SERVICES=glue,s3`
(see `tests/testfixture/glue.go`). LocalStack mimics the Glue Iceberg REST API but
has two shape differences from real AWS Glue:

| Property | LocalStack | Real AWS Glue |
|----------|-----------|---------------|
| **Endpoint URL** | `http://localhost:4566` (or container host:4566) | `https://glue.<region>.amazonaws.com` |
| **SigV4** | Required only if `LOCALSTACK_AUTH_TOKEN` set | Always required |
| **Lake Formation** | Not supported (Lake Formation API calls return 200 noop) | Required if LF enabled on account |
| **Iceberg REST path** | `/iceberg` prefix | `/iceberg` prefix (same) |

Set `LOCALSTACK_GLUE_ENDPOINT` env var on the Neksur gateway to switch between
LocalStack (dev/CI) and real Glue (staging/production). The Glue adapter reads this
at startup:

```bash
# LocalStack (CI / dev):
LOCALSTACK_GLUE_ENDPOINT=http://localhost:4566

# Real AWS (staging / production): leave unset → adapter uses AWS SDK default endpoint
```

---

## 6. SigV4 Endpoint Configuration

The Glue adapter uses the following Iceberg REST properties for SigV4:

```go
// From internal/iceberg/glue/adapter.go (Plan 03-04):
properties := icebergGo.Properties{
    "uri":                          "https://glue.us-east-1.amazonaws.com/iceberg",
    "rest.sigv4-enabled":           "true",
    "rest.signing-name":            "glue",
    "rest.signing-region":          "us-east-1",
    "warehouse":                    "s3://<your-iceberg-bucket>/warehouse",
}
```

Set via env vars on the Neksur gateway:

```bash
AWS_REGION=us-east-1
NEKSUR_GLUE_WAREHOUSE=s3://<your-iceberg-bucket>/warehouse
# NEKSUR_GLUE_ENDPOINT overrides the default https://glue.<region>.amazonaws.com/iceberg
```

---

## 7. Query Smoke Tests

```bash
# 1. List Glue databases via the Neksur gateway:
curl -s https://neksur-gateway:8080/v1/namespaces \
    -H "Authorization: Bearer $NEKSUR_GATEWAY_TOKEN"
# Expected: JSON array of Glue database names

# 2. Load a specific Glue table metadata:
curl -s "https://neksur-gateway:8080/v1/namespaces/prod_catalog/tables/orders" \
    -H "Authorization: Bearer $NEKSUR_GATEWAY_TOKEN"
# Expected: Iceberg table metadata JSON with current-snapshot-id

# 3. Issue a filtered query via Trino (Trino → Neksur gateway → Glue):
trino --execute "SELECT count(*) FROM glue.prod_catalog.orders WHERE deleted_flag=false"
# Expected: filtered row count (policy enforced at Neksur gateway)
```

---

## 8. Pass / Fail Checklist

| # | Check | Pass Criterion |
|---|-------|----------------|
| 1 | IAM role attached to gateway | `aws sts get-caller-identity` returns `NeksurGlueRole` |
| 2 | Glue `GetTable` succeeds | `aws glue get-table --database-name prod_catalog --name orders` exits 0 |
| 3 | Lake Formation (if enabled) | `lakeformation list-permissions` shows SELECT for NeksurGlueRole |
| 4 | SigV4 signing works | `curl https://glue.us-east-1.amazonaws.com/iceberg/v1/config` signed returns 200 |
| 5 | Row-filter enforced via Neksur | Count query returns filtered result |
| 6 | LocalStack shape (CI) | `LOCALSTACK_GLUE_ENDPOINT` set; CI tests use LocalStack |
| 7 | Nightly CI PENDING_FIRST_RUN flipped | 03-ACCEPTANCE.md §9 Glue row shows PASS |

---

*Phase 3 operator runbook — AWS Glue Iceberg REST deployment via Neksur gateway.
Plans: 03-04 (Glue live adapter), 03-15 (acceptance gate).
Pitfall 3: Lake Formation — IAM-only insufficient when LF is enabled on catalog.*
