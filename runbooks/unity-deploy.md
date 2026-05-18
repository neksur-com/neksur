# Runbook: Databricks Unity Catalog Deployment (Neksur Policy Gateway)

**Owner:** Customer Engineering / Platform Engineering
**Scope:** Configure a Databricks workspace + Unity Catalog to route Iceberg REST
traffic through the Neksur L1 gateway. Covers OAuth client-credentials setup,
`X-Databricks-Workspace-Id` header requirements (Pitfall 2 token refresh), the Unity
adapter configuration, and the nightly CI PENDING_FIRST_RUN gate.
**Closes:** 03-VALIDATION.md Manual-Only §Three-binary upgrade path (Unity leg of
REQ-multi-engine-tiers); Phase 3 Plan 03-03 (Unity Catalog live adapter).

---

## 1. Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **Databricks workspace** | Premium+ (Unity Catalog requires Premium) | Workspace Admin UI → Settings → Unity Catalog enabled |
| **Unity Catalog** | Enabled on workspace | `databricks unity-catalog metastores list` via CLI |
| **OAuth client app** | Registered in Databricks workspace | Admin UI → Settings → OAuth → Add custom app |
| **Neksur gateway** | Running + reachable from Databricks | `curl https://neksur-gateway:8080/v1/config` returns 200 |
| **Env vars on client** | NEKSUR_UNITY_WORKSPACE_HOST, NEKSUR_UNITY_OAUTH_CLIENT_ID, NEKSUR_UNITY_OAUTH_CLIENT_SECRET, NEKSUR_UNITY_CATALOG_NAME | `echo "$NEKSUR_UNITY_WORKSPACE_HOST"` |

---

## 2. Unity Adapter Architecture

Per Plan 03-03 (D-3.02), the Neksur Unity adapter is a clone of the Polaris adapter with
three Unity-specific overrides:

| Property | Polaris | Unity Catalog |
|----------|---------|---------------|
| **Auth** | OAuth2 client-credentials (`oauth2-server-uri = Polaris endpoint`) | OAuth2 client-credentials (`oauth2-server-uri = workspace /oidc/v1/token`) |
| **Endpoint** | Polaris REST URL | `https://<workspace-host>/api/2.1/unity-catalog/iceberg` |
| **Workspace header** | Not required | `X-Databricks-Workspace-Id: <workspace-id>` required on every request |
| **Namespace depth** | Multi-level | Flat-1 (catalog.schema.table maps to single-segment Iceberg REST namespace) |

The Neksur Unity adapter handles these differences transparently. Customer-side
configuration sets the workspace host, OAuth credentials, and catalog name. The adapter
calls the Unity Iceberg REST endpoint and injects row-filter + column-mask policies at
the proxy layer (not via Unity's native masking — per D-3.02, Neksur does NOT push DDL
into Unity; Unity Horizon governance is Phase 5).

---

## 3. OAuth Client App Setup in Databricks

### 3.1 Register an OAuth custom application

1. Navigate to **Databricks Admin Console** → **Settings** → **OAuth**.
2. Click **Add custom app**.
3. Configure:
   - **App name:** `neksur-policy-proxy`
   - **Redirect URLs:** `https://neksur-gateway/oauth/callback` (placeholder; not used
     for client-credentials flow — required by Databricks UI but unused at runtime)
   - **Scopes:** `all-apis` (required for Iceberg REST catalog access per Databricks docs)
4. Click **Save**. Copy the **Client ID** and **Client Secret** immediately.
5. Store the secret in your secrets manager:
   ```bash
   aws secretsmanager put-secret-value \
       --secret-id "neksur/unity-oauth-secret" \
       --secret-string "$NEKSUR_UNITY_OAUTH_CLIENT_SECRET"
   ```

### 3.2 Grant the OAuth app access to the Unity catalog

```bash
# Grant the service principal SELECT + READ_VOLUME on the target catalog:
databricks unity-catalog permissions update catalog/prod_catalog \
    --principal service-principals/neksur-policy-proxy \
    --privilege USE_CATALOG,USE_SCHEMA,SELECT
```

---

## 4. Neksur Unity Adapter Configuration

Set the following environment variables on the neksur-server instance that serves
Unity Catalog reads:

```bash
# Required Unity adapter env vars (Plan 03-03):
NEKSUR_UNITY_WORKSPACE_HOST=https://dbc-xxx-yyy.cloud.databricks.com
NEKSUR_UNITY_OAUTH_CLIENT_ID=<client-id-from-step-3.1>
NEKSUR_UNITY_OAUTH_CLIENT_SECRET=<secret-from-secrets-manager>
NEKSUR_UNITY_CATALOG_NAME=prod_catalog      # Unity catalog name
NEKSUR_UNITY_WORKSPACE_ID=1234567890123     # Numeric workspace ID from Databricks URL
```

The Unity workspace ID is in the Databricks workspace URL:
`https://adb-<WORKSPACE_ID>.azuredatabricks.net` or visible in Admin Console.

**Do not set `NEKSUR_UNITY_WORKSPACE_ID` via plaintext in `.env` files committed to git.**
Use your secrets manager or Kubernetes secrets.

---

## 5. Pitfall 2 — Token Refresh Troubleshooting

Per 03-RESEARCH.md Pitfall 2: Unity Catalog OAuth tokens have a short TTL (~1 hour).
If the Neksur gateway does not refresh the token before it expires, Iceberg REST calls
begin returning `401 Unauthorized`.

**Symptom:**

```
level=error msg="unity adapter: HTTP 401" endpoint="https://dbc-xxx.cloud.databricks.com/api/2.1/unity-catalog/iceberg/v1/config"
```

**Diagnosis:**

```bash
# Check the Unity token expiry metric:
curl -s http://neksur-server:9100/metrics | grep unity_token_expiry_seconds
# Expected: unity_token_expiry_seconds > 0 (positive = time until expiry)
# Problem indicator: unity_token_expiry_seconds <= 0 (token expired or absent)
```

**Fix:**

1. The Unity adapter's token cache (Plan 03-03) should auto-refresh 60 seconds before
   expiry. If it is not refreshing, check that the OAuth client secret is correct and
   accessible from the gateway host.

2. Force a token refresh by restarting the gateway (temporary fix):
   ```bash
   systemctl restart neksur-server-commercial
   # Or in Kubernetes: kubectl rollout restart deployment/neksur-server-commercial
   ```

3. Permanent fix: verify the OAuth client app secret has not been rotated in Databricks.
   If it was rotated, update the secret in your secrets manager and redeploy the gateway.

**Root cause (Pitfall 2 — D-3.02):** Databricks rotates OAuth client secrets every 90
days by default (workspace policy-dependent). Set up a 60-day rotation alert in your
secrets manager to proactively rotate before expiry.

---

## 6. Neksur Unity Endpoint as Iceberg REST Catalog

Databricks Unity Catalog documentation (April 2026) explicitly states:
> "You cannot use Iceberg REST catalog or Unity REST APIs to access tables with row
> filters or column masks."

Neksur's Unity adapter exploits this gap — the Neksur gateway sits in front of Unity's
Iceberg REST endpoint and injects row-filter + column-mask enforcement at the proxy layer.
Unity itself does not apply governance; Neksur intercepts before Unity sees the query.

For customers using Databricks as a **query engine** (not as a catalog source), configure
Databricks to use the **Neksur Polaris endpoint** as the Iceberg REST catalog instead of
Unity directly:

```python
# In Databricks notebook / spark-defaults.conf:
spark.conf.set("spark.sql.catalog.neksur", "org.apache.iceberg.spark.SparkCatalog")
spark.conf.set("spark.sql.catalog.neksur.type", "rest")
spark.conf.set("spark.sql.catalog.neksur.uri", "https://neksur-gateway:8080/v1")
spark.conf.set("spark.sql.catalog.neksur.credential", "$NEKSUR_DATABRICKS_TOKEN")
# Result: Databricks Spark queries the Neksur Polaris proxy; policies enforced.
```

This is distinct from using Unity Catalog as the _catalog authority_ — see Plan 03-03
for the full Unity adapter architecture.

---

## 7. PENDING_FIRST_RUN Gate

Per 03-ACCEPTANCE.md §9, the Unity Catalog acceptance row is `PENDING_FIRST_RUN`
until the nightly-cross-engine.yml workflow exits 0 with `NEKSUR_UNITY_WORKSPACE_HOST`
set and the Unity integration tests passing.

Steps to flip the Unity PENDING_FIRST_RUN row:
1. Set the Unity env vars in the GitHub Actions secret store.
2. Confirm `nightly-cross-engine.yml` exits 0 with the `adapter_unity_test.go` suite
   running against a live Unity workspace.
3. Record the result in 03-ACCEPTANCE.md §9 Unity row: `PASS — nightly CI exit-0 <date>`.

---

## 8. Pass / Fail Checklist

| # | Check | Pass Criterion |
|---|-------|----------------|
| 1 | OAuth app registered | Client ID + secret available |
| 2 | Catalog access granted | `SELECT` privilege on target catalog for service principal |
| 3 | Env vars set on gateway | `echo "$NEKSUR_UNITY_WORKSPACE_HOST"` non-empty |
| 4 | Token refresh working | `unity_token_expiry_seconds > 0` in Prometheus |
| 5 | Row-filter enforced | Query returns filtered count (not total) |
| 6 | Column-mask enforced | PII column returns masked values |
| 7 | `X-Databricks-Workspace-Id` header present | Server logs show header injection (debug level) |
| 8 | Nightly CI PENDING_FIRST_RUN flipped | 03-ACCEPTANCE.md §9 Unity row shows PASS |

---

*Phase 3 operator runbook — Databricks Unity Catalog deployment via Neksur gateway.
Plans: 03-03 (Unity live adapter), 03-15 (acceptance gate).
Pitfall 2: token refresh — set 60-day rotation alert for OAuth client secret.*
