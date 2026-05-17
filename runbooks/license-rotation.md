# Runbook: License Key Rotation

**Owner:** SecOps / Platform Engineering
**Scope:** ECDSA P-256 keypair generation ceremony (test/staging vs production),
`cmd/license-gen` usage, fsnotify hot-reload deployment, grace-period semantics,
7-day window, and revocation flow.
**Closes:** 03-VALIDATION.md Manual-Only Verification §License-file revocation flow
on lost laptop (REQ-multi-engine-tiers).

---

## 1. Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **Go** | 1.24+ | `go version` |
| **openssl** | Any recent version | `openssl version` |
| **neksur-core** | Clean checkout | `git status` clean |
| **Access to signing-key store** | Required for production rotation | HSM / KMS / operator workstation per org policy |
| **`cmd/license-gen`** | Built or runnable | `go run ./cmd/license-gen --help` |

**CRITICAL:** The ECDSA P-256 **private** key must never appear in git, CI logs, or
environment variables in plaintext. Use a secrets manager (AWS Secrets Manager,
Vault, or HSM) in production. In staging, write to `/etc/neksur/staging-signing-key.pem`
and restrict permissions to `chmod 600`.

---

## 2. Background: License Architecture

Per D-3.04 (ADR-002), Neksur ships three binary tiers:

| Binary | License required | Features enabled |
|--------|-----------------|-----------------|
| `neksur-server` (L1, BSL Core) | No | Basic snapshot pinning, schema introspection |
| `neksur-server-commercial` (L2) | Yes — `commercial` or `enterprise` tier | L1 + RLS gateway, schema-cache broadcaster, write-conflict, verifier |
| `neksur-server-enterprise` (L3) | Yes — `enterprise` tier | L1 + L2 + partition-spec versioning, compaction coordination |

The license manifest is a JSON document signed with ECDSA P-256:

```json
{
  "license_id": "lic_2026-05-17-abc123",
  "customer_id": "acme-corp",
  "tenant_id": "tenant_uuid_or_*_for_org_wide",
  "tier": "enterprise",
  "expiry_utc": "2027-05-17T00:00:00Z",
  "allowed_features": [
    "snapshot_pinning", "schema_introspection",
    "rls_gateway", "schema_cache_broadcaster",
    "verification_probes", "write_conflict",
    "continuous_verifier", "partition_spec_versioning",
    "compaction_coordination"
  ],
  "signature": "MEUCIQDxxx...base64"
}
```

**Signature covers:** all fields except `signature` itself. ECDSA P-256 over SHA-256.
**Grace period:** 7 days post-`expiry_utc` — binary continues with all features but
logs `license_grace_period_remaining_seconds` metric and emits warning on every boot.
**Hard expiry:** after 7 days, L2/L3 features disable. L1 binary is unaffected.

See `internal/license/manifest.go` + `internal/license/verifier.go` for the canonical
implementation. The public key is compiled into higher-tier binaries via `//go:embed`
(Plan 03-02).

---

## 3. ECDSA P-256 Keypair Generation Ceremony

### 3.1 Generate a new keypair (staging or production)

```bash
# Step 1: Generate ECDSA P-256 private key (PKCS#8 PEM format)
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
    -out /etc/neksur/signing-key.pem

# Step 2: Restrict permissions immediately
chmod 600 /etc/neksur/signing-key.pem
chown root:neksur /etc/neksur/signing-key.pem  # adapt to your user model

# Step 3: Extract the public key (for embedding in binaries)
openssl pkey -in /etc/neksur/signing-key.pem -pubout \
    -out /etc/neksur/signing-pubkey.pem

# Step 4: Verify the keypair
openssl pkey -in /etc/neksur/signing-key.pem -pubout -noout && \
    echo "Key generation: PASS"
```

### 3.2 Safekeeping (production)

| Asset | Storage | Access |
|-------|---------|--------|
| ECDSA private key | AWS Secrets Manager (secret name: `neksur/license-signing-key`) or HSM | SecOps only; all access audited |
| ECDSA public key | Embedded in `neksur-server-commercial` + `neksur-server-enterprise` binaries at build time via `//go:embed internal/license/pubkey.pem` | Public — anyone with the binary can extract it |
| License JSON manifests | Per-customer secret (Secrets Manager, encrypted email, or customer Vault) | Customer + Neksur sales/ops |

**Do not store the private key on the Neksur server filesystem.** The signing ceremony
happens offline (operator workstation or HSM); only the signed manifest is deployed.

### 3.3 Embedding the public key in new binaries

After a keypair rotation, the new public key must be embedded in the next binary release:

```bash
# Place the new public key at the embed path:
cp /etc/neksur/signing-pubkey.pem \
    /Users/evgeny/neksur-core/internal/license/pubkey.pem

# Rebuild commercial + enterprise binaries:
cd /Users/evgeny/neksur-core
make build-commercial   # embeds the new pubkey
make build-enterprise   # embeds the new pubkey

# Verify the embedded key matches:
go test -tags commercial -run TestPublicKeyEmbedded ./tests/integration/...
```

**Pitfall 4 (03-RESEARCH.md):** Old binary + new public key = broken signature verification.
Binaries built before the key rotation will reject manifests signed with the new key.
Issue the new manifests with the OLD private key for existing customers until all binaries
are rolled. Coordinate with customers on binary upgrade timeline.

---

## 4. Issuing Licenses via `cmd/license-gen`

### 4.1 Issue a new license manifest

```bash
# Build the CLI:
go build -o ./bin/license-gen ./cmd/license-gen

# Issue a commercial license (L2):
./bin/license-gen \
    --customer-id acme-corp \
    --tenant-id tenant_uuid_or_star \
    --tier commercial \
    --expiry 2027-05-17T00:00:00Z \
    --features rls_gateway,schema_cache_broadcaster,write_conflict,continuous_verifier \
    --private-key /etc/neksur/signing-key.pem \
    --out /tmp/acme-corp-commercial.json

# Issue an enterprise license (L3):
./bin/license-gen \
    --customer-id acme-corp \
    --tenant-id tenant_uuid_or_star \
    --tier enterprise \
    --expiry 2027-05-17T00:00:00Z \
    --features rls_gateway,schema_cache_broadcaster,write_conflict,continuous_verifier,partition_spec_versioning,compaction_coordination \
    --private-key /etc/neksur/signing-key.pem \
    --out /tmp/acme-corp-enterprise.json

# Verify the generated manifest:
./bin/license-gen --verify \
    --manifest /tmp/acme-corp-enterprise.json \
    --pubkey /etc/neksur/signing-pubkey.pem
# Expected: "Manifest signature: VALID. Expiry: 2027-05-17T00:00:00Z (365 days remaining)."
```

### 4.2 Staging vs production

For staging and test environments, use a dedicated staging keypair (not the production key).
Store the staging private key in CI secrets (`NEKSUR_LICENSE_STAGING_KEY`) as a
base64-encoded PEM. The nightly-cross-engine.yml workflow uses the staging key for
integration test license manifests.

---

## 5. Deploying a New License File

### 5.1 Place the manifest on the server

```bash
# Transfer the signed manifest to the server:
scp /tmp/acme-corp-enterprise.json \
    operator@neksur-server:/etc/neksur/license.json.new

# Atomic replace (mirrors fsnotify rename-event pattern per Plan 03-02):
ssh operator@neksur-server \
    'mv /etc/neksur/license.json.new /etc/neksur/license.json'
```

### 5.2 Hot-reload via fsnotify (no restart required)

The `neksur-server-commercial` and `neksur-server-enterprise` binaries watch
`/etc/neksur/license.json` (or `$NEKSUR_LICENSE_PATH`) via fsnotify (Plan 03-13).
An atomic rename (write to `.new`, then `mv`) triggers the watcher reliably.

Expected log line within ~2 seconds of the `mv`:

```json
{"level":"info","msg":"license: manifest hot-reloaded","license_id":"lic_2027-...","tier":"enterprise","expiry_utc":"2027-05-17T00:00:00Z"}
```

Verify the reload:

```bash
# Check the server metric:
curl -s http://neksur-server:9100/metrics | grep license_grace_period_remaining_seconds
# Expected: license_grace_period_remaining_seconds{license_id="lic_2027-..."} > 0
```

If the server is NOT running the commercial/enterprise binary, or if `NEKSUR_LICENSE_PATH`
points to a different file, the watcher does not fire — verify the binary tier and env var.

---

## 6. Grace Period Semantics

Per D-3.04, the grace period is exactly **7 calendar days** after `expiry_utc`:

| State | Binary behavior | Metric |
|-------|----------------|--------|
| Valid (expiry > now) | Normal operation | `license_grace_period_remaining_seconds` = seconds until expiry |
| Grace (0 ≤ expiry - now ≤ 7 days) | All features enabled; warning on every boot + every 24h recheck | `license_grace_period_remaining_seconds` = 0 (not negative) |
| Expired (now > expiry + 7 days) | L2/L3 features disabled; L1 continues; `license.IsFeatureAllowed()` returns false | `license_expired_total` incremented; alert fires |

**Fail-closed on signature failure:** if the manifest signature is invalid (corrupt,
wrong key, or tampered manifest), the binary refuses to boot regardless of grace period.
Grace period only applies to legitimate time-expiry, not signature failures.

---

## 7. Revocation Flow

Neksur uses **offline revocation only** (no license-server / phone-home in Phase 3).
Revocation is achieved by rotating the keypair and re-issuing all non-revoked licenses
with the new private key. The old manifest is rendered invalid by the new binary's
embedded public key.

### 7.1 Lost laptop / compromised signing key

1. Generate a new keypair (§3.1 above).
2. Re-issue all active customer license manifests signed with the new key.
3. Rebuild `neksur-server-commercial` + `neksur-server-enterprise` with the new public key embedded (§3.3).
4. Deploy the new binaries to all staging + production environments.
5. Deploy the new license manifests to all customer instances.
6. Confirm customers have upgraded their binaries before the old manifests expire.

**Note:** Customers running binaries built with the old public key will continue to
accept manifests signed with either the old or new private key ONLY until they upgrade
the binary. Coordinate the binary upgrade schedule with customer success.

### 7.2 Specific license revocation (single customer)

1. Do NOT re-issue a new manifest for the revoked customer.
2. Allow the existing manifest to expire naturally (within `expiry_utc + 7 days`).
3. If immediate revocation is needed before `expiry_utc`: issue a new manifest with
   `expiry_utc` set to `now() + 1h` (minimum grace period before features disable is 7 days
   from the new expiry). This gives the customer 7 days + 1h to notice and contact support.
4. Document the revocation in the operator attestation log (internal incident record).

---

## 8. Pass / Fail Checklist

| # | Check | Pass Criterion |
|---|-------|----------------|
| 1 | New keypair generated | `openssl pkey -in signing-key.pem -pubout -noout` exits 0 |
| 2 | Private key restricted | `ls -la /etc/neksur/signing-key.pem` shows `chmod 600` |
| 3 | Manifest issued | `./bin/license-gen --verify` exits 0 with "VALID" |
| 4 | Manifest deployed | `/etc/neksur/license.json` exists on target server |
| 5 | Hot-reload triggered | License hot-reload log line appears within 2s |
| 6 | Metric visible | `license_grace_period_remaining_seconds > 0` in Prometheus |
| 7 | Revoked license rejected | Old manifest no longer accepted after binary upgrade with new public key |

---

*Phase 3 operator runbook — license key rotation ceremony + fsnotify hot-reload + grace-period
semantics + revocation flow. Closes 03-VALIDATION.md Manual-Only §License-file revocation flow.
Pitfall reference: Pitfall 4 (03-RESEARCH.md) — old binary + new public key breakage.*
