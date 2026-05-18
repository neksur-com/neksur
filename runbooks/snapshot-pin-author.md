# Runbook: Snapshot Pin Authoring Guide

**Owner:** Data Engineering / SecOps
**Scope:** Authoring snapshot pins for consistent cross-engine reads — named vs session
pins, `expiry_utc` TTL semantics, orphan pin sweep cadence, and Plan 03-06 pin_sweep
metric monitoring. Includes Plan 03-12 compaction interaction guidance.
**Closes:** Phase 3 Plans 03-06 (SnapshotPin store) + 03-12 (compaction coordinator);
REQ-snapshot-pinning acceptance.

---

## 1. What Are Snapshot Pins?

When multiple Iceberg query engines read the same table concurrently, a writer may
commit a new snapshot mid-read. Without a pin, different engines may see different
snapshot versions — engine A reads snapshot v1, engine B reads snapshot v2 after a
compaction, producing inconsistent results.

Neksur's Snapshot Pinning Service (Plan 03-06, REQ-snapshot-pinning, L1 BSL Core feature)
records a pin in the AGE graph:

```
(SnapshotPin {pin_name, pinned_by_principal, at_snapshot_id, pinned_at, expiry_utc})
(SnapshotPin)-[:PINS]->(Table)
(Query)-[:READ {pinned: true, pinned_by: pin_name, at_snapshot: snapshot_id}]->(Table)
```

All cross-engine reads for the pinned table while the pin is active are forced to read
`at_snapshot_id`, regardless of more recent snapshots. This satisfies REQ-snapshot-pinning's
"consistent reads across heterogeneous write/read engines" guarantee.

---

## 2. Pin Types

| Type | Scope | TTL | Use case |
|------|-------|-----|----------|
| **Named pin** | Persistent; explicit creation + deletion | Must set `expiry_utc`; swept when expired | Long-running ETL jobs; cross-engine consistency windows; audit queries |
| **Session pin** | Tied to the authenticated query session | Implicit; expires when session ends OR after default TTL (4h) | Interactive BI queries; short read windows |

**Named pins are preferred** for ETL jobs and compliance windows where the consistency
window must outlast a single session. Session pins are convenient for interactive
analytics.

---

## 3. Creating a Named Pin

### 3.1 Via `neksur-cli`

```bash
# Create a named pin on the orders table at the current snapshot:
neksur-cli pin create \
    --name "etl-window-2026-Q2" \
    --table prod.orders \
    --expiry 2026-06-30T23:59:59Z \
    --principal "svc-etl-pipeline@neksur.local"

# Expected output:
# {
#   "pin_name": "etl-window-2026-Q2",
#   "at_snapshot_id": 1234567890,
#   "pinned_at": "2026-05-17T12:00:00Z",
#   "expiry_utc": "2026-06-30T23:59:59Z",
#   "table": "prod.orders"
# }
```

### 3.2 Via Neksur REST API

```bash
curl -X POST https://neksur-gateway:8080/api/pins \
    -H "Authorization: Bearer $NEKSUR_GATEWAY_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
        "pin_name": "etl-window-2026-Q2",
        "table_namespace": "prod",
        "table_name": "orders",
        "expiry_utc": "2026-06-30T23:59:59Z",
        "principal": "svc-etl-pipeline@neksur.local"
    }'
```

### 3.3 Via Cypher (advanced — direct graph write)

```cypher
-- AGE 1.6 SnapshotPin MERGE via ExecuteInTenant:
MERGE (sp:SnapshotPin { pin_name: $pin_name })
ON CREATE SET sp.tenant_id = $tenant_id,
              sp.pinned_by_principal = $principal,
              sp.at_snapshot_id = $snapshot_id,
              sp.pinned_at = now(),
              sp.expiry_utc = $expiry_utc
WITH sp
MATCH (t:Table { name: $table_name, namespace: $namespace })
MERGE (sp)-[:PINS]->(t)
RETURN sp.pin_name, sp.at_snapshot_id, sp.expiry_utc
```

**AGE 1.6 quirk:** Use the COALESCE-on-WITH-SET emulation pattern if `ON CREATE SET` is
rejected; see Plan 01-04 SUMMARY for the canonical workaround.

---

## 4. Listing and Inspecting Active Pins

```bash
# List all active pins for a table:
neksur-cli pin list --table prod.orders

# Expected output:
# pin_name                expiry_utc                  at_snapshot_id  pinned_by
# etl-window-2026-Q2      2026-06-30T23:59:59Z        1234567890      svc-etl-pipeline

# Inspect a specific pin:
neksur-cli pin get --name etl-window-2026-Q2
```

---

## 5. `expiry_utc` TTL Semantics

**Setting `expiry_utc` is mandatory for named pins.** The Neksur orphan-pin sweep
(Plan 03-06 `pin_sweep.go`) runs daily and deletes expired SnapshotPin nodes where:
`expiry_utc < now()` AND no active Query is referencing the pin.

| Scenario | TTL recommendation |
|----------|--------------------|
| ETL job with known finish time | Set `expiry_utc` = job expected finish + 1h buffer |
| Compliance audit window (30-day) | `expiry_utc` = today + 30 days |
| Interactive BI query | Use session pin (implicit 4h TTL) |
| Long-running Spark job | Named pin with `expiry_utc` = Spark job timeout |

**Do not set `expiry_utc` more than 90 days in the future** without a compaction
coordinator override (see §7). Long-lived pins prevent Iceberg snapshot expiration
on the pinned table, causing unbounded storage growth.

---

## 6. Deleting a Pin

```bash
# Delete a named pin (explicit release — does not wait for TTL expiry):
neksur-cli pin delete --name etl-window-2026-Q2

# Via REST:
curl -X DELETE https://neksur-gateway:8080/api/pins/etl-window-2026-Q2 \
    -H "Authorization: Bearer $NEKSUR_GATEWAY_TOKEN"
```

After deletion, the pin is gone from the graph and the orphan sweep will no longer see it.
Queries that were referencing the pin will see the latest snapshot on next access.

---

## 7. Orphan Pin Sweep Monitoring

The daily orphan pin sweep (Plan 03-06 `pin_sweep.go`) removes expired SnapshotPin nodes
that no longer have active Query references. Monitor via the Plan 03-06 metric:

```bash
# Check orphan sweep metrics:
curl -s http://neksur-server:9100/metrics | grep snapshot_pin_
# Key metrics:
# snapshot_pin_sweep_total          — total expired pins removed in last sweep
# snapshot_pin_sweep_duration_seconds — duration of last sweep
# snapshot_pin_active_count          — current active (non-expired) pin count
# snapshot_pin_orphan_count          — pins expired but not yet swept (sweep lag indicator)
```

**Alert threshold:** If `snapshot_pin_orphan_count > 1000`, the sweep is falling behind.
This typically indicates very high pin creation rate without corresponding deletion.
Investigate whether ETL jobs are creating pins but not deleting them on completion.

```promql
# PromQL rule for orphan pin backlog alert:
# Fires when orphan count exceeds 500 for 30 minutes.
snapshot_pin_orphan_count > 500
```

---

## 8. Plan 03-12 Compaction Interaction

When an Iceberg compaction job runs on a pinned table, the Neksur compaction coordinator
(Plan 03-12, L3 enterprise feature) extends Iceberg's `ExpireSnapshots` deadline:

- If `snapshot_pin.at_snapshot_id` is referenced by an active pin, the compaction
  coordinator prevents `ExpireSnapshots` from deleting that snapshot.
- The compaction waits until the pin expires or is explicitly deleted.
- The compaction coordinator emits `compaction_blocked_by_pin_total` metric when blocked.

**Operator guidance for long compaction blocks:**

```bash
# Check if compaction is blocked by a specific pin:
curl -s http://neksur-server:9100/metrics | \
    grep compaction_blocked_by_pin_total
# If this counter is incrementing, check which pins are blocking:

neksur-cli pin list --table prod.orders --blocking-compaction
# Expected: shows pins whose at_snapshot_id overlaps with the compaction target
```

To unblock a compaction, either:
1. Wait for the blocking pin's `expiry_utc` to pass (recommended — protects data consistency).
2. Explicitly delete the pin if the ETL job is finished:
   ```bash
   neksur-cli pin delete --name etl-window-2026-Q2
   ```

**Do NOT extend the pin TTL indefinitely to avoid compaction.** Long-lived pins cause
unbounded S3 storage growth. If ETL jobs regularly block compaction, review the job
schedule and set appropriate `expiry_utc` values.

---

## 9. Session Pin Behavior

Session pins are managed automatically by the Neksur SQL proxy (Plan 03-06). When a
query session begins, the proxy records a session pin in the AGE graph. When the session
ends (connection close or timeout), the pin is automatically released.

**Default session pin TTL:** 4 hours. Configurable via `NEKSUR_SESSION_PIN_TTL` env var.
**No operator action required** for session pins under normal operation.

---

## 10. Pass / Fail Checklist

| # | Check | Pass Criterion |
|---|-------|----------------|
| 1 | Named pin created | `neksur-cli pin get --name <name>` returns pin with `at_snapshot_id` |
| 2 | Cross-engine reads consistent | All engines return identical row counts while pin active |
| 3 | Pin expired on schedule | `snapshot_pin_orphan_count` includes expired pin; sweep removes it daily |
| 4 | Orphan sweep metric visible | `snapshot_pin_sweep_total` increments after 00:00 UTC daily |
| 5 | Compaction blocked (if applicable) | `compaction_blocked_by_pin_total` increments; unblocked after pin deleted |
| 6 | Session pin released | `snapshot_pin_active_count` decrements after session close |

---

*Phase 3 operator runbook — snapshot pin authoring + TTL semantics + orphan sweep +
compaction interaction.
Plans: 03-06 (SnapshotPin store), 03-12 (compaction coordinator), 03-15 (acceptance gate).
`expiry_utc` field is the canonical TTL; pin_sweep runs daily at 00:00 UTC.*
