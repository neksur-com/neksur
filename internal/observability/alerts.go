package observability

import (
	"context"
	"strings"
)

// AlarmPayload mirrors the relevant subset of the CloudWatch alarm JSON
// payload that SNS delivers to the pagerduty-bridge Lambda. We expose
// the same struct for in-process testing of the dispatch logic without
// having to drag in a full SNS event shape.
type AlarmPayload struct {
	AlarmName        string
	NewStateValue    string // "ALARM" | "OK" | "INSUFFICIENT_DATA"
	AlarmDescription string
	Dimensions       []Dim
}

// Dim is a CloudWatch alarm dimension (Name/Value pair).
type Dim struct {
	Name  string
	Value string
}

// DispatchAlarm maps a CloudWatch alarm payload to a PagerDuty Events
// API v2 trigger or resolve.
//
// Routing logic:
//   - tenant extraction: if the alarm payload carries a `TenantID`
//     dimension, that's the tenant identifier; else "global".
//   - severity: default "warning"; bumped to "critical" if the alarm
//     name contains "storage" or "5xx".
//   - action: "trigger" by default; "resolve" if NewStateValue == "OK".
//
// The dedup key derivation lives in `pagerduty.go::DedupKey` so this
// function and the Lambda bridge produce identical keys for the same
// (service, alert, tenant) tuple.
func DispatchAlarm(ctx context.Context, pd *PagerDuty, payload AlarmPayload) error {
	tenant := ""
	for _, d := range payload.Dimensions {
		if d.Name == "TenantID" {
			tenant = d.Value
			break
		}
	}

	severity := "warning"
	if strings.Contains(payload.AlarmName, "storage") || strings.Contains(payload.AlarmName, "5xx") {
		severity = "critical"
	}

	action := "trigger"
	if payload.NewStateValue == "OK" {
		action = "resolve"
		severity = "info"
	}

	return pd.TriggerOrResolve(
		ctx,
		action,
		severity,
		payload.AlarmDescription,
		payload.AlarmName,
		tenant,
		nil,
	)
}
