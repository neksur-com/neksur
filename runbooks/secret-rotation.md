# Runbook: Secret Rotation (AWS Secrets Manager)

**Owner:** Founder / Security on-call (Phase 0.5); Security + DevOps
(Phase 1+)
**Scope:** Operator runbook for rotating each of the Phase 0.5 SaaS
secrets stored in AWS Secrets Manager. Quarterly cadence for
externally-issued API keys; annual for routing/page keys; ad-hoc for
known-compromise scenarios.
**Contract:** RESEARCH §Runtime State Inventory lines 1036–1044 (the
eight Phase 0.5 secrets) + D-0.5.21 T-0.5-secret-rotation-downtime
(zero-downtime rotation pattern: read-from-Secrets-Manager-at-request-time
OR caches with TTL).
**Validation tests:** N/A (operator-driven; rotation correctness verified
by post-rotation smoke tests + the issuer's "last used N min ago" UI).

> **Note on the contract.** Every rotation writes an audit-log row via
> `./neksur-cli admin secret-rotation-log` (CLI placeholder for Phase 1+;
> for Phase 0.5 the operator types the audit row manually — procedure
> in §3).

---

## 1. Rotation cadence + criticality

The Phase 0.5 secrets, by criticality:

| Secret | AWS Secrets Manager path | Cadence | Compromise risk |
|--------|--------------------------|---------|-----------------|
| `WORKOS_API_KEY` | `neksur/workos/api_key` | Quarterly | Critical — controls all auth |
| `WORKOS_CLIENT_ID` | `neksur/workos/client_id` | (not secret; informational) | Low |
| `WORKOS_WEBHOOK_SECRET` | `neksur/workos/webhook_secret` | Quarterly | Critical — controls webhook auth |
| `STRIPE_API_KEY` | `neksur/stripe/api_key` | Quarterly | Critical (when BILLING_ENABLED=true) |
| `STRIPE_WEBHOOK_SECRET` | `neksur/stripe/webhook_secret` | Quarterly | Critical — controls webhook auth |
| `PAGERDUTY_ROUTING_KEY` | `neksur/pagerduty/routing_key` | Annually | Medium — alert flooding risk |
| `STATUSPAGE_API_KEY` | `neksur/statuspage/api_key` | Annually | Medium — page defacement risk |
| `SLACK_OPS_WEBHOOK_URL` | `neksur/slack/ops_webhook` | Annually | Low — internal notification only |

**Compromise-driven rotation (any time):**
- Key visible in a public source (git, logs, screenshot)
- Key visible in a chat transcript or terminal screen-share
- Employee/contractor departure who had access
- Any "did I just paste that in the wrong window?" moment

For all of the above: drop everything and rotate immediately.

---

## 2. Procedure per secret

Each rotation follows the same five-step pattern:
1. Create a new key in the issuer's UI (parallel to the existing key).
2. Update AWS Secrets Manager (`put-secret-value` to the same path —
   creates a new version while old version remains accessible).
3. Roll the consumer (EC2 ASG rolling restart picks up new value at
   boot — Phase 0.5 model; Phase 1+ caches with TTL for zero-downtime).
4. Confirm the new key is being used (issuer's "last used N min ago"
   UI; or a smoke test).
5. Revoke the old key in the issuer's UI.

Total time per secret: ~15–30 min (depending on ASG rolling-restart
duration).

### 2.a `WORKOS_API_KEY` (quarterly)

**Why critical:** All WorkOS Organizations API + AuthKit session
validation flows through this key. Compromise gives an attacker the
ability to mint sessions, list orgs, and create/delete organizations.

**Procedure:**

```bash
# Step 1 — create new key in WorkOS Dashboard.
# Navigate: WorkOS Dashboard → API Keys → "Create new key".
# Copy the new key (sk_live_... format). Label it
# "neksur-prod-rotation-YYYY-MM-DD".

# Step 2 — write to AWS Secrets Manager.
NEW_KEY='sk_live_...'  # paste from WorkOS dashboard
aws secretsmanager put-secret-value \
    --secret-id neksur/workos/api_key \
    --secret-string "$NEW_KEY" \
    --version-stages AWSCURRENT

# Step 3 — roll the consumer. Phase 0.5: rolling ASG restart.
# The neksur-app EC2 ASG reads Secrets Manager via boot-time IMDS
# instance role; SIGTERM each instance one at a time and wait for the
# new one to come up green.
aws autoscaling start-instance-refresh \
    --auto-scaling-group-name neksur-app-asg \
    --preferences '{"MinHealthyPercentage":100,"InstanceWarmup":120}'

# Wait for the refresh to complete (check every 30s).
aws autoscaling describe-instance-refreshes \
    --auto-scaling-group-name neksur-app-asg \
    --query 'InstanceRefreshes[0].{Status:Status,Percentage:PercentageComplete}'

# Step 4 — confirm new key is in use.
# WorkOS Dashboard → API Keys → "Last used N min ago" column should show
# the NEW key active and the OLD key inactive for at least 5 min.

# Step 5 — revoke old key.
# WorkOS Dashboard → API Keys → click the old key → "Revoke".
# This is irreversible — once revoked the key cannot be re-enabled.

# Step 6 — log the rotation (Phase 0.5 manual; Phase 1+ via CLI).
psql "$DATABASE_URL" -c "
    INSERT INTO public.system_audit_log
        (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
    VALUES
        (now(), 'rotation@neksur.com', NULL, 'secret.rotated',
         '{\"secret\":\"neksur/workos/api_key\",\"rotated_at\":\"$(date -u -Iseconds)\"}'::jsonb);
"
```

**Zero-downtime note:** WorkOS allows multiple active API keys per
account, so the new key works BEFORE the old one is revoked. The ASG
rolling restart can complete entirely before step 5 — there is no
flap window if the procedure is followed.

### 2.b `STRIPE_API_KEY` (quarterly, gated on BILLING_ENABLED)

Same five-step pattern as WORKOS_API_KEY. Stripe additionally supports
"Restricted keys" with narrower scope — prefer those over full API
keys when available.

```bash
# Stripe Dashboard → Developers → API Keys → "Create restricted key"
# Scope to: subscriptions:read+write, invoices:read+write, customers:read+write
# Avoid: dashboard, account, charges (we don't need those for Phase 0.5)

NEW_KEY='rk_live_...'  # restricted key, not sk_live_
aws secretsmanager put-secret-value \
    --secret-id neksur/stripe/api_key \
    --secret-string "$NEW_KEY"

# Roll ASG (same as 2.a). Stripe's webhook signing key is separate
# (rotated in 2.e below) — these two CAN be rotated independently.

# Stripe's "Last used" indicator: Dashboard → Developers → API Keys
# → click key → "Last used N hours ago"
```

**Activation gate:** Phase 0.5 keeps `BILLING_ENABLED=false`; the
internal/billing webhook handler verifies signatures even when the flag
is false (defence in depth — D-0.5.21 T-0.5-stripe-spoof). Rotation is
required even pre-activation because the signature verification code
path is live.

### 2.c `PAGERDUTY_ROUTING_KEY` (annually)

**Why medium-risk:** A leaked routing key allows an attacker to flood
the on-call rotation with fake alerts (DoS via alert fatigue), but does
NOT give access to read incident details or modify the rotation.

**Procedure:**

```bash
# Step 1 — PagerDuty Service → Integrations → Events API v2 → "Regenerate".
# Copy the new routing key (32 chars).

NEW_KEY='<32 char key>'
aws secretsmanager put-secret-value \
    --secret-id neksur/pagerduty/routing_key \
    --secret-string "$NEW_KEY"

# Step 3 — roll the alertmanager pod / EC2.
# Phase 0.5: alertmanager runs on the same EC2 hosts; the rolling
# restart from 2.a refreshes alertmanager too.

# Step 4 — send a test alert to confirm.
curl -X POST https://events.pagerduty.com/v2/enqueue \
    -d '{
        "routing_key": "<NEW_KEY>",
        "event_action": "trigger",
        "payload": {
            "summary": "POST-ROTATION TEST — confirm in PagerDuty UI and resolve",
            "severity": "info",
            "source": "rotation@neksur.com"
        }
    }'

# Confirm in PagerDuty UI that the incident appears. Resolve it.

# Step 5 — there is no "old key" to revoke — Regenerate replaces the
# active key atomically. Done.
```

### 2.d `STATUSPAGE_API_KEY` (annually)

**Why medium-risk:** A leaked statuspage.io API key allows an attacker
to deface the status page (post fake incidents) but does not affect
production data.

**Procedure:**

```bash
# Step 1 — statuspage.io → User Settings → API → "Generate New Key".
# Copy the new key.

NEW_KEY='<key>'
aws secretsmanager put-secret-value \
    --secret-id neksur/statuspage/api_key \
    --secret-string "$NEW_KEY"

# Step 3 — roll ASG (same as 2.a).

# Step 4 — confirm via a no-op API call (list components — read-only,
# no UI side-effect).
STATUSPAGE_API_KEY="$NEW_KEY" STATUSPAGE_PAGE_ID='<page-id>' \
    curl -H "Authorization: OAuth $NEW_KEY" \
    "https://api.statuspage.io/v1/pages/$STATUSPAGE_PAGE_ID/components" \
    | jq '.[0].name'

# Step 5 — revoke old key via statuspage.io UI → API → click old key → Revoke.
```

### 2.e `WORKOS_WEBHOOK_SECRET` + `STRIPE_WEBHOOK_SECRET` (quarterly)

**Why critical:** Webhook secrets are used to verify INCOMING webhooks
(WorkOS Org events; Stripe subscription events). A compromised webhook
secret lets an attacker forge events that look like they came from the
issuer — e.g., forge a "tenant suspended" event or a "subscription
canceled" event.

The signature-verify-before-flag-check pattern (Plan 03 + Plan 05) means
the handler verifies the signature BEFORE reading any payload data —
forged events return HTTP 400 with empty body.

**Procedure:**

```bash
# WORKOS — Dashboard → Endpoints → click endpoint → "Rotate signing secret".
# IMPORTANT: WorkOS supports TWO active signing secrets during rotation
# window (default 7 days). Set both old + new in Secrets Manager during
# the rotation window, OR shorten the window and tolerate <1 min of
# verified-fail events.

NEW_SECRET='<webhook signing secret>'
aws secretsmanager put-secret-value \
    --secret-id neksur/workos/webhook_secret \
    --secret-string "$NEW_SECRET"

# Roll ASG (same as 2.a). The handler now verifies with the NEW secret.
# Events signed with the OLD secret during the overlap window will fail
# signature verification — usually acceptable for a 1-min ASG roll
# during low-traffic hours.

# STRIPE — Dashboard → Developers → Webhooks → click endpoint → "Roll secret".
# Stripe supports overlapping signing secrets for 24 hours by default.
# Procedure identical to WORKOS.
```

**Audit log every rotation:**

```sql
INSERT INTO public.system_audit_log
    (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
VALUES
    (now(), 'rotation@neksur.com', NULL, 'secret.rotated',
     '{"secret":"<secrets-manager-path>","rotated_at":"<ISO-8601>","cadence":"quarterly|annual|adhoc","reason":"scheduled|compromise"}'::jsonb);
```

### 2.f `SLACK_OPS_WEBHOOK_URL` (annually)

Slack incoming webhooks are bound to a channel + workspace; a compromised
webhook lets an attacker post messages to the ops channel (low risk).

```bash
# Slack → Apps → click "Incoming Webhooks" → "Add New Webhook to Workspace"
# → select #neksur-ops channel → copy URL.
NEW_URL='https://hooks.slack.com/services/...'
aws secretsmanager put-secret-value \
    --secret-id neksur/slack/ops_webhook \
    --secret-string "$NEW_URL"

# Step 3 — roll ASG (same as 2.a) OR for Phase 0.5 just update the env
# var in the scripts/* shell wrappers (they read SLACK_OPS_WEBHOOK_URL
# at invocation time).

# Step 4 — test:
curl -X POST -H 'Content-Type: application/json' \
    -d '{"text":"Post-rotation test — please confirm in #neksur-ops"}' \
    "$NEW_URL"

# Step 5 — Slack → Apps → Incoming Webhooks → revoke old URL.
```

---

## 3. Audit log every rotation

Every rotation MUST leave an audit-log row in `public.system_audit_log`
so the rotation history is queryable. The Phase 1+ CLI will be:

```bash
# Phase 1+ — not yet shipped.
./neksur-cli admin secret-rotation-log \
    --secret neksur/workos/api_key \
    --cadence quarterly \
    --reason scheduled
```

For Phase 0.5, the operator types the row manually after every rotation
(SQL template at the top of §2.e). The audit log is queryable for
compliance review:

```sql
SELECT occurred_at, payload->>'secret' AS secret, payload->>'cadence' AS cadence
  FROM public.system_audit_log
 WHERE event_type = 'secret.rotated'
 ORDER BY occurred_at DESC
 LIMIT 20;
```

---

## 4. Post-bootstrap AWS access key rotation reminder

**Phase 0.5 special case.** The AWS bootstrap access key used to set up
the initial Terraform state was visible in early planning-session terminal
output. Per `00.5-ACCEPTANCE.md` §8 Sign-off Checklist, this key MUST be
rotated AFTER Phase 0.5 bootstrap completes and BEFORE Phase 1 begins.

**Procedure:**

```bash
# Step 1 — confirm bootstrap is complete (terraform state lives in S3,
# IAM roles are provisioned, ASG is running).
aws --profile <bootstrap-profile> s3 ls s3://neksur-tf-state/
aws --profile <bootstrap-profile> sts get-caller-identity

# Step 2 — IAM → Users → <bootstrap-user> → Security credentials →
# Create access key (the new one).
NEW_AWS_ACCESS_KEY_ID='AKIA...'
NEW_AWS_SECRET_ACCESS_KEY='...'

# Step 3 — update the operator's local AWS profile.
aws configure --profile neksur-ops set aws_access_key_id "$NEW_AWS_ACCESS_KEY_ID"
aws configure --profile neksur-ops set aws_secret_access_key "$NEW_AWS_SECRET_ACCESS_KEY"

# Step 4 — confirm the new key works by re-running a no-op terraform
# command.
cd /Users/evgeny/neksur-infra/environments/phase0-pilot
AWS_PROFILE=neksur-ops terraform plan -refresh=false

# Step 5 — deactivate (then delete) the old key:
# IAM → Users → <bootstrap-user> → Security credentials → click old key
# → "Make inactive" → wait 24h to confirm nothing broke → "Delete".

# Step 6 — audit log.
psql "$DATABASE_URL" -c "
    INSERT INTO public.system_audit_log
        (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
    VALUES
        (now(), 'rotation@neksur.com', NULL, 'secret.rotated',
         '{\"secret\":\"aws-bootstrap-access-key\",\"rotated_at\":\"$(date -u -Iseconds)\",\"cadence\":\"adhoc\",\"reason\":\"post-bootstrap-hygiene-per-00.5-ACCEPTANCE-sec8\"}'::jsonb);
"
```

The Phase 0.5 ACCEPTANCE.md §8 sign-off row for this rotation flips to
"PASS" once the audit row exists and the old key is deleted.

---

## 5. Cross-references

- AWS Secrets Manager paths: defined in `modules/secrets-manager/main.tf`
  (Plan 01) and consumed by Plan 03 (WorkOS), Plan 05 (Stripe / PagerDuty
  / statuspage / Slack).
- `internal/auth/workos/client.go` — WORKOS_API_KEY consumer.
- `internal/auth/workos/webhook.go` — WORKOS_WEBHOOK_SECRET consumer
  (verify-before-flag-check pattern).
- `internal/billing/webhook.go` — STRIPE_WEBHOOK_SECRET consumer.
- `internal/observability/pagerduty.go` — PAGERDUTY_ROUTING_KEY consumer.
- `internal/observability/statuspage.go` — STATUSPAGE_API_KEY consumer.
- `runbooks/status-page-incident.md` §4.b — statuspage.io key rotation
  cross-reference.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-ACCEPTANCE.md`
  §8 — AWS bootstrap access key rotation sign-off row.
- `.planning/phases/00.5-saas-pilot-infrastructure/00.5-RESEARCH.md`
  §Runtime State Inventory lines 1036–1044.
