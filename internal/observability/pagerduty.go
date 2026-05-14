package observability

import (
	"context"
	"fmt"

	"github.com/PagerDuty/go-pagerduty"
)

// PagerDuty is the Neksur SaaS wrapper around the PagerDuty Events API
// v2 (per D-0.5.13 + CONTEXT specifics line 173). One instance per
// running service; instantiated once at startup with the routing key
// from Secrets Manager and a per-service tag used in dedup keys.
//
// Threading: PagerDuty itself is safe for concurrent use; the embedded
// *http.Client created by the go-pagerduty SDK is goroutine-safe.
type PagerDuty struct {
	routingKey string
	serviceTag string
}

// NewPagerDuty constructs a PagerDuty client.
//
// routingKey is the PagerDuty Events API v2 integration key
// (`PAGERDUTY_ROUTING_KEY` from Secrets Manager). serviceTag is the
// short name of THIS service (e.g. `neksur-server`, `neksur-cli`) —
// it appears as the `Source` in the V2Event payload and as the leading
// segment of the dedup key.
func NewPagerDuty(routingKey, serviceTag string) *PagerDuty {
	return &PagerDuty{routingKey: routingKey, serviceTag: serviceTag}
}

// DedupKey returns the canonical PagerDuty dedup key per D-0.5.13:
//
//	<service>:<alertName>:<tenantOrGlobal>
//
// where tenantOrGlobal is the tenant UUID for tenant-scoped alarms or
// the literal string "global" for system-wide alarms.
//
// This is a pure function — exported so callers (incl. the Lambda
// PagerDuty bridge and the in-process pipeline test) can derive the
// same key without instantiating a PagerDuty client.
func DedupKey(service, alertName, tenantOrGlobal string) string {
	if tenantOrGlobal == "" {
		tenantOrGlobal = "global"
	}
	return fmt.Sprintf("%s:%s:%s", service, alertName, tenantOrGlobal)
}

// Trigger enqueues a PagerDuty Events API v2 `trigger` event.
//
// severity must be one of "info", "warning", "error", "critical".
// summary is a one-line human-readable description. alertName is the
// CloudWatch alarm name (or any stable identifier) — it appears in the
// V2Payload.Component AND in the dedup key. tenant is the tenant UUID
// (or "" for system-wide alarms).
func (p *PagerDuty) Trigger(ctx context.Context, severity, summary, alertName, tenant string, details map[string]string) error {
	return p.triggerOrResolve(ctx, "trigger", severity, summary, alertName, tenant, details)
}

// TriggerOrResolve dispatches a trigger or resolve action against the
// PagerDuty Events API. Used by DispatchAlarm (alerts.go) when mapping
// CloudWatch alarm state transitions to PagerDuty incident lifecycle:
// ALARM → trigger; OK → resolve.
func (p *PagerDuty) TriggerOrResolve(ctx context.Context, action, severity, summary, alertName, tenant string, details map[string]string) error {
	return p.triggerOrResolve(ctx, action, severity, summary, alertName, tenant, details)
}

func (p *PagerDuty) triggerOrResolve(ctx context.Context, action, severity, summary, alertName, tenant string, details map[string]string) error {
	dedup := DedupKey(p.serviceTag, alertName, tenant)

	// pagerduty.V2Event.Details is map[string]interface{}; cast our
	// string-only details map element-by-element.
	d := map[string]interface{}{}
	for k, v := range details {
		d[k] = v
	}

	event := pagerduty.V2Event{
		RoutingKey: p.routingKey,
		Action:     action,
		DedupKey:   dedup,
		Payload: &pagerduty.V2Payload{
			Summary:   summary,
			Source:    p.serviceTag,
			Severity:  severity,
			Component: alertName,
			Details:   d,
		},
	}

	_, err := pagerduty.ManageEventWithContext(ctx, event)
	if err != nil {
		return fmt.Errorf("observability: pagerduty %s: %w", action, err)
	}
	return nil
}
