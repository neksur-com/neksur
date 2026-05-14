# Runbook: Status Page Incident Lifecycle (statuspage.io)

**Owner:** Founder / on-call SRE (Phase 0.5); Customer Communications +
on-call (Phase 1+)
**Scope:** Operator runbook for declaring, updating, and resolving
incidents on the public status page at `status.neksur.com`
(statuspage.io account `neksur`).
**Contract:** **D-0.5.14** (statuspage.io for customer-facing incident
communication; PagerDuty native integration auto-publishes component
state changes; programmatic updates via `internal/observability/statuspage.go`
for scheduled maintenance).
**Validation tests:** `tests/integration/statuspage_integration_test.go::TestStatuspageIntegration`
(component update round-trip — skipped without `STATUSPAGE_API_KEY` and
`STATUSPAGE_PAGE_ID` env vars).

> **Note on the contract.** There are TWO paths to update the status
> page: (a) PagerDuty's native integration auto-updates components on
> incident open / resolve (preferred for unplanned events); (b)
> programmatic updates via `internal/observability/statuspage.go` for
> scheduled maintenance, multi-component incidents, or post-mortem
> updates after PagerDuty has resolved.

---

## 1. Two paths to update the status page

### Path A — PagerDuty auto-update (unplanned incidents)

When PagerDuty fires an alert for a Neksur service (e.g., Pool A primary
down), the PagerDuty native statuspage.io integration publishes a
component status change automatically:

- PagerDuty incident opened → `Pool A Postgres` component → `partial_outage`
  or `major_outage` (rule-mapped per service severity).
- PagerDuty incident acknowledged → no change.
- PagerDuty incident resolved → `Pool A Postgres` → `operational`.

This path requires zero operator action. The PagerDuty service-to-component
mapping is configured in the PagerDuty UI:

```
PagerDuty → Services → "neksur-pool-a-primary" → Integrations
    → Statuspage → Component: "Pool A Postgres"
    → Severity mapping:
        critical → major_outage
        error    → partial_outage
        warning  → degraded_performance
```

**Operator action:** none, unless you need to add detail or comms
beyond what PagerDuty auto-publishes — in which case fall through to
Path B for the incident update.

### Path B — Programmatic update (maintenance windows, multi-component, post-mortem)

For events that need explicit operator control (scheduled maintenance,
multi-component incidents that span PagerDuty service boundaries,
post-mortem communication after PagerDuty has resolved):

```bash
# Example: declare a scheduled maintenance.
./neksur-cli admin statuspage-incident \
    --component-id <component-id> \
    --status under_maintenance \
    --message "Routine database maintenance — read-only window 14:00–15:00 UTC"
```

The CLI surface above is a placeholder for Phase 1+; for Phase 0.5 use
the Go client at `internal/observability/statuspage.go` directly via a
small driver, OR `curl` the statuspage.io REST API (procedure in §2).

---

## 2. Scheduled maintenance window (full lifecycle)

### 2.a 24-hour pre-notice

24 hours before the maintenance window, post a banner on statuspage.io:

```bash
# Replace <PAGE_ID> and <COMPONENT_ID> with real values; fetch via
# `curl https://api.statuspage.io/v1/pages` with the API key set.
STATUSPAGE_API_KEY=$(aws secretsmanager get-secret-value \
    --secret-id neksur/statuspage/api_key --query SecretString --output text)

curl -X POST "https://api.statuspage.io/v1/pages/$PAGE_ID/incidents" \
  -H "Authorization: OAuth $STATUSPAGE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "incident": {
      "name": "Scheduled maintenance — Pool A database",
      "status": "scheduled",
      "scheduled_for": "2026-05-15T14:00:00Z",
      "scheduled_until": "2026-05-15T15:00:00Z",
      "scheduled_remind_prior": true,
      "components": {"<COMPONENT_ID>": "under_maintenance"},
      "component_ids": ["<COMPONENT_ID>"],
      "body": "We will be performing routine database maintenance on Pool A from 14:00 UTC to 15:00 UTC. During this window, the Neksur API will be read-only — your existing data remains accessible, but writes will be queued or return HTTP 503."
    }
  }'
```

### 2.b At maintenance start (T-0)

Transition the incident to `in_progress` and set the component to
`under_maintenance`:

```bash
curl -X PATCH "https://api.statuspage.io/v1/pages/$PAGE_ID/incidents/$INCIDENT_ID" \
  -H "Authorization: OAuth $STATUSPAGE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "incident": {
      "status": "in_progress",
      "body": "Maintenance has started. Pool A is now read-only."
    }
  }'
```

Or use the Go client:

```go
import "github.com/neksur-com/neksur/internal/observability"

client := observability.NewStatuspageClient(apiKey, pageID)
err := client.UpdateComponent(ctx, componentID, "under_maintenance")
```

### 2.c At maintenance end (T-end)

Set component back to `operational` and resolve the incident:

```bash
curl -X PATCH "https://api.statuspage.io/v1/pages/$PAGE_ID/incidents/$INCIDENT_ID" \
  -H "Authorization: OAuth $STATUSPAGE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "incident": {
      "status": "resolved",
      "components": {"<COMPONENT_ID>": "operational"},
      "body": "Maintenance is complete. Pool A is now fully operational."
    }
  }'
```

### 2.d Post-mortem (within 5 business days)

For maintenance windows that overran their scheduled time OR uncovered
unexpected issues, post a follow-up incident with the post-mortem:

```bash
# Open a "completed" incident retroactively documenting what happened.
curl -X POST "https://api.statuspage.io/v1/pages/$PAGE_ID/incidents" \
  -H "Authorization: OAuth $STATUSPAGE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "incident": {
      "name": "Post-mortem — 2026-05-15 maintenance overrun",
      "status": "postmortem",
      "body": "..."
    }
  }'
```

---

## 3. Communication template (incident announcement)

Adjust to fit the incident shape. Ported from `runbooks/restore-pitr.md`
§6 communication template (PATTERNS.md line 836 — keep one canonical
template across runbooks).

> **DRAFT (do not publish without sign-off):**
>
> **{ISO-8601 timestamp} — {Service} {severity verb}**
>
> We detected an issue with {service} starting at approximately
> {time UTC}. {What we know about scope: e.g., "Customers in
> {region} may see elevated error rates on writes; read operations
> are unaffected."} We are actively investigating. Updates every
> {15 minutes for major, 30 minutes for partial}.
>
> ---
>
> **{ISO-8601 timestamp} — UPDATE**
>
> {What we did: e.g., "We identified the cause as {root cause} and
> have {mitigation applied}."} {Current state: e.g., "All
> tenants are now back to normal operation."} A full post-mortem
> will follow within 5 business days.
>
> ---
>
> **{ISO-8601 timestamp} — RESOLVED**
>
> All services are operating normally. Customers can resume normal
> operations. We are committed to publishing a post-mortem within 5
> business days at https://status.neksur.com/incidents/<id>.

---

## 4. Operational notes

### 4.a Components inventory (Phase 0.5)

| Component | statuspage.io ID | PagerDuty service link | Auto-managed by PagerDuty? |
|-----------|------------------|------------------------|----------------------------|
| Pool A Postgres | [TBD — fetch via `GET /v1/pages/<id>/components`] | `neksur-pool-a-primary` | yes |
| Pool A AGE | [TBD] | (covered by Pool A Postgres) | yes |
| WorkOS authentication | [TBD] | `neksur-workos-middleware` | yes |
| API gateway | [TBD] | `neksur-gateway` | yes |
| Per-Pool-B (first Enterprise) | [TBD; provisioned on demand] | `neksur-pool-b-<customer-uuid>` | yes |

The `neksur-cli admin statuspage-list-components` placeholder is Phase 1+;
for Phase 0.5, list via curl:

```bash
curl -H "Authorization: OAuth $STATUSPAGE_API_KEY" \
    "https://api.statuspage.io/v1/pages/$PAGE_ID/components" | jq '.[] | {id, name, status}'
```

### 4.b Rotating the statuspage.io API key

See `runbooks/secret-rotation.md` §2.d (the STATUSPAGE_API_KEY rotation
procedure). Annual rotation cadence unless compromised.

---

## 5. Cross-references

- `internal/observability/statuspage.go` — Go client used by Path B
  (programmatic updates).
- `tests/integration/statuspage_integration_test.go` — round-trip test
  for the component update API.
- `runbooks/secret-rotation.md` §2.d — STATUSPAGE_API_KEY rotation.
- `runbooks/restore-pitr.md` §6 — canonical communication template
  this runbook ports.
- `runbooks/failover.md` — Patroni HA failover (Phase 0 runbook;
  triggers PagerDuty → statuspage.io Path A automatically).
- statuspage.io REST API docs: https://developer.statuspage.io
- PagerDuty native statuspage.io integration docs:
  https://www.pagerduty.com/docs/guides/statuspage-integration-guide/
