# Runbook: Phase 2 Dev Cold-Start

**Owner:** Developer onboarding / first-time-running-Neksur-from-clone
**Scope:** Bring up the Phase 2 cross-engine policy plane from a clean
checkout end-to-end. Postgres+AGE comes up via docker compose; migrations
apply; `neksur-server` boots into the Phase 2 stack (compiler trigger +
L1 catalog gateway + L4 credvend service; sqlproxy mTLS listener is
optional but covered here).
**Closes:** `02-UAT.md` Test 1 — "Cold Start Smoke Test" gap.

---

## 1. Prerequisites

| Item | Required | How to verify |
|------|----------|---------------|
| **Go** | 1.24+ | `go version` |
| **Docker** | 24.0+ (with `docker compose` subcommand) | `docker --version` |
| **Atlas CLI** | 0.28+ on `$PATH` (or set `NEKSUR_ATLAS_BIN`) — `cmd/migrate` shells out to `atlas migrate apply` | `atlas version` — install via `curl -sSf https://atlasgo.sh \| sh` |
| **Repo** | Clean checkout of `neksur-core` | `git status` clean |
| **Ports** | 5432 (postgres), 8080 (gateway), 8443 (sqlproxy if enabled), 9100 (metrics if enabled) free | `lsof -i :5432 -i :8080 -i :8443 -i :9100` |

If any prereq fails, **HALT** and fix it before proceeding.

---

## 2. Bring up Postgres

```bash
cd /path/to/neksur-core
docker compose up -d postgres
docker compose ps postgres
# Expected: postgres service "running (healthy)" after ~10s
```

Verify the container is accepting connections:

```bash
docker compose exec postgres pg_isready -U neksur -d neksur
# Expected: "postgres:5432 - accepting connections"
```

---

## 3. Configure env vars

```bash
cp .env.example .env
# edit .env for the WorkOS / TLS values appropriate to your environment
set -a; source .env; set +a
echo "$DATABASE_URL"
# Expected: postgres://neksur:neksur_dev@localhost:5432/neksur?sslmode=disable
```

The env-flag matrix is the canonical source of truth for what each
variable enables. `neksur-server --help` prints the same matrix; keep
them in sync if you edit the binary.

| Variable | Required when | Default | Description |
|----------|---------------|---------|-------------|
| `DATABASE_URL`           | Always (migrate + neksur-server) | (none) | pgxpool DSN; aligned to docker-compose creds |
| `NEKSUR_OBSERVABILITY=1` | Optional       | unset  | Enable OTel + Prometheus /metrics |
| `NEKSUR_METRICS_ADDR`    | When OBS=1     | :9100  | Prometheus listener address |
| `NEKSUR_SAAS_AUTH=1`     | Phase 2 demo   | unset  | Enables Phase 2 stack (compiler + gateway + sqlproxy) |
| `WORKOS_API_KEY`         | When SAAS=1    | (none) | from WorkOS dashboard |
| `WORKOS_CLIENT_ID`       | When SAAS=1    | (none) | from WorkOS dashboard |
| `WORKOS_WEBHOOK_SECRET`  | When SAAS=1    | (none) | from WorkOS dashboard → Webhooks |
| `NEKSUR_LISTEN_ADDR`     | When SAAS=1    | :8080  | L1 catalog gateway listener |
| `NEKSUR_TLS_CERT_PATH`   | sqlproxy on    | unset  | Server cert (PEM) — all four needed or proxy disables |
| `NEKSUR_TLS_KEY_PATH`    | sqlproxy on    | unset  | Server key (PEM) |
| `NEKSUR_CA_BUNDLE_PATH`  | sqlproxy on    | unset  | CA bundle for mTLS client verification |
| `NEKSUR_SQLPROXY_ADDR`   | sqlproxy on    | :8443  | SQL proxy mTLS listener |
| `NEKSUR_S3_EVENTS_QUEUE_URL` | L3 S3 path | unset  | SQS queue URL for ObjectCreated events |
| `NEKSUR_S3_EVENTS_TENANT_ID` | When S3 set | unset  | Tenant ID owning the queue |
| `NEKSUR_SLACK_WEBHOOK_URL` | Optional     | unset  | Alerts.Slack endpoint |
| `BILLING_ENABLED`         | Optional      | false  | Enables Stripe billing surface (sig still verified when false) |
| `STRIPE_API_KEY`          | When BILLING_ENABLED=true | (none) | Stripe dashboard → Developers |
| `STRIPE_WEBHOOK_SECRET`   | Security best-practice when SAAS=1 | (none) | Sig verified BEFORE BILLING_ENABLED gate to defeat spoofed webhooks (T-0.5-stripe-spoof). Binary boots without it but the /webhooks/stripe handler will reject all payloads. |
| `WORKOS_INTERNAL_ADMIN_ORG_ID` | Admin UI access | unset | WorkOS Org for Neksur staff |
| `PAGERDUTY_SERVICE_ID`   | Optional      | P000000 | Admin UI PagerDuty embed |

The minimal Phase 2 boot needs: `DATABASE_URL`, `NEKSUR_SAAS_AUTH=1`,
`WORKOS_API_KEY`, `WORKOS_CLIENT_ID`, `WORKOS_WEBHOOK_SECRET`. Add
`STRIPE_WEBHOOK_SECRET` as a security best-practice (it's not enforced
on boot but the /webhooks/stripe route will reject every payload
without it). The sqlproxy listener is OFF by default (an info log line
"sqlproxy: mTLS material missing, listener disabled" confirms this).

---

## 4. Run migrations

```bash
go run ./cmd/migrate
# Expected: V0001..V0073 apply cleanly; no errors; stdout reports each version.
```

Common failure modes:

- `DATABASE_URL is required` → re-run `set -a; source .env; set +a`
  (env vars don't survive across new shells).
- `atlas: exec: "atlas": executable file not found in $PATH` → install
  the Atlas CLI (`curl -sSf https://atlasgo.sh | sh`) or export
  `NEKSUR_ATLAS_BIN=/path/to/atlas`. The migrate binary shells out to
  `atlas migrate apply` for the public + per-tenant tier passes.

---

## 5. Boot neksur-server

```bash
go run ./cmd/neksur-server
```

With **no env flags set**, the binary prints the usage banner to stderr
and exits 0. This is the affordance fix from `02-UAT.md` Test 1 — see
Section 1.

```bash
go run ./cmd/neksur-server --help
# Same banner, to stdout this time. Exits 0.
```

With **NEKSUR_SAAS_AUTH=1** + the required WORKOS_* values + DATABASE_URL,
the binary boots into the Phase 2 stack. Expected slog lines on stdout
(sample, abridged — wall-clock order may vary):

```json
{"level":"info","msg":"sqlproxy: mTLS material missing, listener disabled","cert_path_set":false,"key_path_set":false,"ca_bundle_path_set":false}
```

(the sqlproxy info line is normal in dev when TLS env vars are unset)

The compiler trigger + L1 catalog gateway + L4 credvend service are
wired without explicit slog lines at boot — confirm reachability via
the verification step below.

---

## 6. Verify Phase 2 reachability

In a second terminal:

```bash
# L1 catalog gateway tenant middleware is active — 401 is the correct
# fail-closed signal that the gateway is wired and demanding a tenant
# session cookie. A 404 would mean main() didn't dispatch into
# runWithSaasAuth(); a connection refused would mean the binary
# didn't bind to NEKSUR_LISTEN_ADDR.
curl -i http://localhost:8080/api/
# Expected: HTTP/1.1 401 Unauthorized
```

Optional checks:

```bash
# Webhook handler reachable (sig will fail without a real Stripe payload,
# which is fine — we're only confirming the route is mounted):
curl -i -X POST http://localhost:8080/webhooks/stripe -d '{}'
# Expected: HTTP/1.1 400 (sig verification failed) — route IS mounted.

# WorkOS webhook handler reachable:
curl -i -X POST http://localhost:8080/webhooks/workos -d '{}'
# Expected: HTTP/1.1 400 (sig verification failed) — route IS mounted.
```

---

## 7. Tear-down

```bash
# Stop neksur-server (Ctrl-C in the run terminal — graceful shutdown
# via SIGTERM + 5s grace per main.go:619-625).

docker compose down                     # stop postgres
docker compose down -v                  # AND remove the pgdata volume (clean slate)
```

---

## 8. PASS / FAIL Checklist

| # | Check | Pass criterion | Section |
|---|-------|----------------|---------|
| 1 | Postgres healthy        | `docker compose ps postgres` reports "healthy"  | 2 |
| 2 | DATABASE_URL exported   | `echo "$DATABASE_URL"` prints non-empty DSN     | 3 |
| 3 | Migrations applied      | `go run ./cmd/migrate` exits 0                   | 4 |
| 4 | Usage banner            | `neksur-server --help` prints env-flag matrix    | 5 |
| 5 | Gateway reachable       | `curl http://localhost:8080/api/` returns 401   | 6 |

If all 5 pass, Phase 2 functionality is reachable from a clean checkout.
Closes 02-UAT.md Test 1.

---

*Phase 2 dev cold-start runbook — closes 02-UAT.md Test 1 (operator
affordance gap). Companion docs: runbooks/phase0-deploy.md (production-ish
deploy), runbooks/sql-proxy-deploy.md (sqlproxy operations).*
