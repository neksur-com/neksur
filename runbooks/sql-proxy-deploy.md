# Runbook: SQL Proxy Deployment and Operations (Phase 2)

**Owner:** Platform engineer — SQL Proxy (Plan 02-05)
**Scope:** Deploying, scaling, and operating the Phase 2 SQL proxy component.
Covers the mTLS+row-filter+column-mask proxy architecture, replica count + LB shape,
environment variables, readyz check, and manual cipher suite scan.
**Triggers:** SqlProxyP95Breach / SqlProxyP99Breach alerts (observability/rules/phase2-sql-proxy-latency.yml)
**Closes:** Phase 2 operational requirement per RESEARCH §Standard Stack line 365

---

## System Architecture

The following diagram shows the Phase 2 SQL proxy topology (from RESEARCH §System Architecture
Diagram lines 198-279). The SQL proxy is the `cmd/neksur-server` binary running on a SEPARATE
port (`NEKSUR_SQLPROXY_PORT`) from the L1 catalog gateway port.

```
┌─────────────────────────────────────────────────────────────────────┐
│                          Neksur Data Plane                           │
│                                                                     │
│  ┌──────────────┐     mTLS       ┌─────────────────────────────┐   │
│  │ Spark Exec   │────────────────► SQL Proxy (port 8443)        │   │
│  │ (Executor 1) │  client cert   │  internal/sqlproxy/          │   │
│  └──────────────┘  per-tenant   │  ┌───────────────────────┐   │   │
│                    SubCA (PCA)  │  │ TLS termination       │   │   │
│  ┌──────────────┐               │  │ Client cert → tenant  │   │   │
│  │ Spark Exec   │────────────────► │ Row-filter injection  │   │   │
│  │ (Executor 2) │               │  │ Column-mask injection  │   │   │
│  └──────────────┘               │  └───────────┬───────────┘   │   │
│                                 │              │                │   │
│  ┌──────────────┐               │  ┌───────────▼───────────┐   │   │
│  │ Trino Worker │────────────────► │ Policy lookup         │   │   │
│  │              │               │  │ CompiledPolicy cache  │   │   │
│  └──────────────┘               │  │ (in-process LRU)      │   │   │
│                                 │  └───────────┬───────────┘   │   │
│                                 └──────────────┼───────────────┘   │
│                                                │                   │
│               ┌────────────────────────────────▼──────────┐        │
│               │             Upstream SQL Engines           │        │
│               │  ┌──────────┐  ┌──────────┐  ┌─────────┐ │        │
│               │  │  Trino   │  │  Spark   │  │ Dremio  │ │        │
│               │  │ (467+)   │  │ (3.5.4)  │  │ (stub)  │ │        │
│               │  └──────────┘  └──────────┘  └─────────┘ │        │
│               └───────────────────────────────────────────┘        │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────┐        │
│  │                   Neksur Control Plane                   │        │
│  │  ┌────────────┐  ┌──────────────┐  ┌────────────────┐  │        │
│  │  │ AGE Graph  │  │ Postgres (PG)│  │ L1 Catalog GW  │  │        │
│  │  │ (Policies  │  │ (policies,   │  │ (port 8080)    │  │        │
│  │  │  compiled) │  │  compiled_   │  │ Plan 02-03     │  │        │
│  │  │            │  │  policies)   │  │                │  │        │
│  │  └────────────┘  └──────────────┘  └────────────────┘  │        │
│  └─────────────────────────────────────────────────────────┘        │
│                                                                     │
│  ┌──────────────────┐  ┌──────────────────┐                         │
│  │  LocalStack/AWS  │  │  ACM PCA         │                         │
│  │  S3+STS+KMS      │  │  Per-tenant      │                         │
│  │  (Plan 02-07)    │  │  Subordinate CA  │                         │
│  │                  │  │  (Plan 02-08)    │                         │
│  └──────────────────┘  └──────────────────┘                         │
└─────────────────────────────────────────────────────────────────────┘
```

**Key flows:**

1. **Spark executor → SQL proxy:** mTLS with per-tenant client cert (issued by ACM PCA
   subordinate CA, vended via `/v1/credvend/sts`). The proxy reads the client cert SAN
   to identify tenant.
2. **SQL proxy → CompiledPolicy cache:** Proxy looks up the compiled row-filter + column-mask
   for the requesting principal's tenant. Cache miss → fetches from `public.compiled_policies`.
3. **SQL proxy → upstream SQL engine:** Injects `WHERE <row-filter>` into the SQL query
   before forwarding to Trino/Spark. Column-mask expressions applied to result set on return.
4. **mTLS cert chain:** Spark executor cert → per-tenant subordinate CA → shared root CA.
   Root CA certificate distributed to Spark executors as truststore.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NEKSUR_SQLPROXY_PORT` | `8443` | Port the SQL proxy listens on (mTLS) |
| `NEKSUR_TLS_CERT_PATH` | (required) | Path to TLS server certificate PEM file |
| `NEKSUR_TLS_KEY_PATH` | (required) | Path to TLS server private key PEM file |
| `NEKSUR_CA_BUNDLE_PATH` | (required) | S3 path or local path to CA bundle PEM (root + subordinate chain); consumed at startup by `sqlproxy.NewTLSConfig` |
| `NEKSUR_TLS_RELOAD_INTERVAL` | `30s` | How often cert_watcher.go polls for file changes (fsnotify + polling fallback) |
| `NEKSUR_SQLPROXY_CACHE_SIZE` | `1000` | LRU cache size for CompiledPolicy entries |
| `NEKSUR_SQLPROXY_CACHE_TTL` | `5m` | TTL for cached CompiledPolicy entries |
| `NEKSUR_SQLPROXY_TIMEOUT` | `30s` | Upstream SQL engine query timeout |
| `NEKSUR_SQLPROXY_MAX_CONNS` | `100` | Max upstream SQL connections per engine |

### Setting env vars

In Kubernetes:

```bash
kubectl set env deployment/neksur-server -n neksur-system \
  NEKSUR_SQLPROXY_PORT=8443 \
  NEKSUR_TLS_CERT_PATH=/etc/neksur/tls/tls.crt \
  NEKSUR_TLS_KEY_PATH=/etc/neksur/tls/tls.key \
  NEKSUR_CA_BUNDLE_PATH=s3://neksur-pki-phase0-pilot/tenants/<tenant_uuid>/ca-bundle.pem
```

In EC2 user-data (Phase 0.5 pilot):

```bash
# These env vars are wired via the EC2 launch template from Terraform outputs:
# module.acm_pca_design_partner_1.ca_bundle_path → NEKSUR_CA_BUNDLE_PATH
export NEKSUR_CA_BUNDLE_PATH="$(terraform output -raw design_partner_1_ca_bundle_path)"
export NEKSUR_PCA_ARN="$(terraform output -raw design_partner_1_subordinate_ca_arn)"
```

---

## Deployment

### Replica Count and Load Balancer Shape

The SQL proxy is co-located with the L1 catalog gateway (`cmd/neksur-server`) but on a
different port. They share the same Kubernetes Deployment and HPA:

| Parameter | Value | Notes |
|-----------|-------|-------|
| Minimum replicas | 2 | HA — no single-point-of-failure |
| Maximum replicas | 10 | HPA scales on CPU + `sql_proxy_overhead_ms_p95` |
| Target CPU | 60% | Conservative; SQL proxy is I/O-bound |
| LB type | Network Load Balancer (Layer 4) | mTLS passthrough — NLB does NOT terminate TLS; the proxy does |
| Health check | TCP port 8443 | NLB TCP health check (not HTTP — mTLS requires client cert) |
| Session affinity | None | SQL queries are stateless per request |

**NLB vs. ALB:** The SQL proxy uses an NLB (Layer 4) because mTLS client certificates are
presented at the TLS layer — an ALB (Layer 7) would terminate TLS before the proxy sees the
client cert. Always use NLB for this deployment.

### Readyz Check

The SQL proxy exposes `/readyz` on the MGMT port (`NEKSUR_MGMT_PORT`, default 9090):

```bash
curl -s http://<host>:9090/readyz | jq
```

Expected response when ready:

```json
{
  "status": "ready",
  "checks": {
    "tls_cert_loaded": true,
    "ca_bundle_loaded": true,
    "compiled_policy_cache_warmed": true,
    "compiled_policy_cache_entries": 42
  }
}
```

The proxy is NOT ready until:
1. `tls_cert_loaded: true` — TLS cert file was read and parsed successfully.
2. `ca_bundle_loaded: true` — CA bundle was fetched from `NEKSUR_CA_BUNDLE_PATH` and parsed.
3. `compiled_policy_cache_warmed: true` — At least 1 CompiledPolicy entry loaded from Postgres.

If `/readyz` returns `"status": "not_ready"`, the NLB health check fails and the replica is
removed from rotation. Follow the diagnosis below.

**Common not-ready causes:**

```bash
# Check pod logs for startup errors:
kubectl logs -n neksur-system deployment/neksur-server --since=5m | grep -E "ERROR|FATAL|not_ready"
```

| Log pattern | Cause | Fix |
|-------------|-------|-----|
| `"ca_bundle_loaded": false` | S3 fetch of CA bundle failed (wrong path or permissions) | Verify `NEKSUR_CA_BUNDLE_PATH`; check S3 bucket policy |
| `"tls_cert_loaded": false` | TLS cert file missing or corrupt | Run mtls-cert-rotation.md procedure |
| `"compiled_policy_cache_warmed": false` | Postgres connection failure or no PUBLISHED policies | Check DB connectivity; seed at least 1 policy |

---

## SqlProxyP95Breach / SqlProxyP99Breach Alert Response

**Alert fires when:**

```
histogram_quantile(0.95, sum(rate(sql_proxy_overhead_ms_bucket[5m])) by (le)) > 50   # P95 > 50ms
histogram_quantile(0.99, sum(rate(sql_proxy_overhead_ms_bucket[5m])) by (le)) > 150  # P99 > 150ms
```

The `sql_proxy_overhead_ms` histogram measures ONLY the overhead the SQL proxy adds to a
query (time in proxy - upstream engine time). Does NOT include the upstream SQL engine
execution time.

**Diagnosis steps:**

```promql
# 1. Identify which phase is slow (row-filter injection vs. policy lookup vs. upstream):
histogram_quantile(0.95, sum by (phase, le) (rate(sql_proxy_overhead_ms_bucket[5m])))
```

Expected overhead breakdown per phase:
- `policy_lookup`: < 5ms (LRU cache hit — cache miss can be 50-100ms)
- `row_filter_inject`: < 2ms (SQL string manipulation)
- `column_mask_apply`: < 10ms (result set transform)
- `total`: < 30ms P95 target

**High `policy_lookup` latency:** Cache miss rate too high. Increase `NEKSUR_SQLPROXY_CACHE_SIZE`
or check if policies are being updated frequently (each update invalidates cache entries).

**High `column_mask_apply` latency:** Result set is too large. Column masking happens in-memory
on the proxy — large result sets (> 10,000 rows) can cause OOM or high latency. Enforce
LIMIT on SQL queries routed through the proxy (policy enforcement can add a LIMIT clause).

**High `row_filter_inject` latency:** Complex row-filter CEL expressions generating large SQL
WHERE clauses. Review the CEL expressions for that tenant (use `neksur-cli policy compiled-status`
to inspect the transpiled SQL).

---

## Manual Cipher Suite Scan

Run this against staging or a test endpoint before promoting to production. Required by
02-VALIDATION.md Manual-Only verification #3.

```bash
# Install nmap if not present:
brew install nmap  # macOS
apt-get install nmap  # Ubuntu

# Run the cipher scan against the SQL proxy port:
nmap --script ssl-enum-ciphers -p 8443 <staging-host>
```

**Expected output (TLS 1.3 only):**

```
PORT     STATE SERVICE
8443/tcp open  https
| ssl-enum-ciphers:
|   TLSv1.3:
|     ciphers:
|       TLS_AES_128_GCM_SHA256 (secp256r1) - A
|       TLS_AES_256_GCM_SHA384 (secp256r1) - A
|       TLS_CHACHA20_POLY1305_SHA256 (secp256r1) - A
|     cipher preference: server
|_  least strength: A
```

**What to look for:**
- TLSv1.3 ONLY — no TLSv1.2 / TLSv1.1 / TLSv1.0 sections.
- All ciphers graded A (no B, C, or F grades).
- `TLS_AES_128_GCM_SHA256`, `TLS_AES_256_GCM_SHA384`, `TLS_CHACHA20_POLY1305_SHA256` present.

**If TLS 1.2 appears:** The `MinVersion: tls.VersionTLS13` constraint in `sqlproxy.NewTLSConfig`
was not applied. Check the TLS config build in `internal/sqlproxy/tls.go`.

**Expected cipher list for ACCEPTANCE.md sign-off:**

Record the actual output of the nmap scan in the ACCEPTANCE.md §Sign-off Checklist row
for "Manual cipher scan — TLS 1.3 only":

```markdown
| Manual cipher scan | PASS | `nmap --script ssl-enum-ciphers -p 8443 staging.neksur.internal` — TLS 1.3 only; ciphers: TLS_AES_128_GCM_SHA256, TLS_AES_256_GCM_SHA384, TLS_CHACHA20_POLY1305_SHA256 |
```

---

## Scaling Triggers

| Trigger | Action |
|---------|--------|
| `sql_proxy_overhead_ms` P95 > 30ms (warning threshold) | HPA adds replica; investigate cache miss rate |
| `sql_proxy_overhead_ms` P95 > 50ms (page threshold) | SqlProxyP95Breach alert fires; follow §Alert Response |
| `sql_proxy_overhead_ms` P99 > 150ms | SqlProxyP99Breach alert fires; same response |
| CPU > 60% on any replica | HPA scales out automatically |
| `compiled_policy_cache_entries` near `NEKSUR_SQLPROXY_CACHE_SIZE` | Increase cache size via env var; rolling restart |

---

## Cert Hot-Reload (fsnotify)

The SQL proxy uses `cert_watcher.go` (Plan 02-05) to watch `NEKSUR_TLS_CERT_PATH` and
`NEKSUR_TLS_KEY_PATH` via fsnotify. When either file changes, the TLS config is reloaded
in-process without a server restart.

**Test that cert reload works:**

```bash
# Trigger a reload by touching the cert file:
kubectl exec -n neksur-system deployment/neksur-server -- touch "$NEKSUR_TLS_CERT_PATH"

# Watch for the reload log event:
kubectl logs -n neksur-system deployment/neksur-server -f | grep cert_reloaded
# Expected: '{"level":"info","component":"cert_watcher","event":"cert_reloaded","path":"..."}'
```

For full cert rotation procedures, see `runbooks/mtls-cert-rotation.md`.

---

## References

- **D-2.05** — SQL proxy architecture (Plan 02-05).
- **D-2.08** — ACM PCA mTLS per-tenant cert issuance.
- **REQ-NFR-latency-sql-proxy** — P95 < 50ms, P99 < 150ms.
- **observability/rules/phase2-sql-proxy-latency.yml** — SqlProxyP95Breach + SqlProxyP99Breach alerts.
- **runbooks/mtls-cert-rotation.md** — cert rotation procedure.
- **runbooks/gateway-deploy.md** — L1 catalog gateway topology (sister runbook, same binary different port).
- **Plan 02-05 SUMMARY** — SQL proxy + cert watcher implementation.
- **Plan 02-08 Task 1** — ACM PCA Terraform module + CA bundle path wiring.

---

*Phase 2 SQL proxy deploy runbook — Phase 2 Plan 02-08 Task 2.*
