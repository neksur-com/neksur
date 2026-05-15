// Slack alert dispatcher for Phase 1 L3 detection findings (Plan 01-07).
//
// Mirrors the PagerDuty pattern from internal/observability/pagerduty.go
// (PATTERNS Group G lines 113 — exact analog) but POSTs to a Slack
// incoming webhook URL instead of the PagerDuty Events API. Slack is
// chosen over PagerDuty for Phase 1 detection alerts (D-1.12 + D-OQ.07)
// because L3 findings are dashboard-trend signal, not paging-grade
// incidents (Phase 6 Tier 2/3 may add PagerDuty fan-out for
// confidence-1.0 findings).
//
// Threading: Slack is safe for concurrent use (the http.Client is
// goroutine-safe and we don't mutate per-call state).
//
// Test endpoint override: NewSlackWithClient accepts an *http.Client
// override (e.g., httptest.NewServer.Client()) so the integration test
// (tests/integration/slack_alert_test.go) can capture and assert the
// POST body without hitting the real Slack API.
//
// Failure mode: degrade silently if WebhookURL is empty (operators may
// not have wired up Slack in early Phase 1 deployments). Phase 1 fail-
// closed semantic is `detection_runs` row + DetectionRun graph node;
// Slack is an optional-extra observability surface.

package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/neksur-com/neksur/internal/detect"
)

// Slack is the Phase 1 L3 detection alert dispatcher. Construct ONCE
// per process at neksur-server startup; share across all detection
// scanners.
type Slack struct {
	webhookURL string
	serviceTag string
	httpClient *http.Client
}

// NewSlack constructs a Slack dispatcher against the given incoming-
// webhook URL + per-service tag. The default http.Client has a 10s
// timeout (Slack incoming-webhook responses are typically <100ms; 10s
// guards against network stalls).
//
// webhookURL is the Slack incoming-webhook URL (e.g.,
// https://hooks.slack.com/services/T0/B0/XXX) sourced from
// NEKSUR_SLACK_WEBHOOK_URL env in production. serviceTag is the short
// service name (e.g., `neksur-server`) used in alert payload + future
// dashboard filters.
//
// If webhookURL is empty, every Post call returns nil (silent no-op).
// This lets operators ship without configuring Slack in early Phase 1
// deployments while keeping the call-sites unconditional.
func NewSlack(webhookURL, serviceTag string) *Slack {
	return NewSlackWithClient(webhookURL, serviceTag, &http.Client{Timeout: 10 * time.Second})
}

// NewSlackWithClient constructs a Slack dispatcher with a caller-supplied
// http.Client. Production callers use NewSlack; integration tests pass
// httptest.NewServer.Client() to capture POST bodies.
func NewSlackWithClient(webhookURL, serviceTag string, client *http.Client) *Slack {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Slack{
		webhookURL: webhookURL,
		serviceTag: serviceTag,
		httpClient: client,
	}
}

// slackPayload is the wire-shape JSON body for the Slack incoming-webhook
// POST. Slack's incoming-webhook accepts a `text` field plus an
// `attachments` array of typed blocks. Phase 1 uses a simple shape with
// `text` for the headline + an attachments block carrying the per-finding
// details — sufficient for dashboard alerts; Phase 6 may switch to Block
// Kit for richer formatting.
type slackPayload struct {
	Text        string             `json:"text"`
	Attachments []slackAttachment  `json:"attachments,omitempty"`
}

type slackAttachment struct {
	Color  string            `json:"color,omitempty"`
	Title  string            `json:"title,omitempty"`
	Fields []slackField      `json:"fields,omitempty"`
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Post dispatches a Slack alert. severity is one of "info" / "warning" /
// "error" (maps to attachment color); summary is the one-line headline;
// tenantID is the tenant UUID (for cross-tenant alert disambiguation);
// details is a string-string map rendered as Slack attachment fields.
//
// Returns:
//   - nil on success (Slack returned 2xx) OR when webhookURL is empty
//     (silent no-op).
//   - wrapped detect.ErrSlackPostFailed on non-2xx.
//   - wrapped transport error on network failure.
func (s *Slack) Post(ctx context.Context, severity, summary, tenantID string, details map[string]string) error {
	// Silent no-op when not configured — operators may ship before
	// configuring NEKSUR_SLACK_WEBHOOK_URL.
	if s.webhookURL == "" {
		return nil
	}

	color := severityColor(severity)
	fields := make([]slackField, 0, len(details)+1)
	fields = append(fields, slackField{Title: "tenant", Value: tenantID, Short: true})
	for k, v := range details {
		fields = append(fields, slackField{Title: k, Value: v, Short: true})
	}

	payload := slackPayload{
		Text: fmt.Sprintf("[%s] %s — %s", s.serviceTag, severity, summary),
		Attachments: []slackAttachment{
			{
				Color:  color,
				Title:  summary,
				Fields: fields,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("alerts/slack: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alerts/slack: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("alerts/slack: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alerts/slack: post: status=%d: %w", resp.StatusCode, detect.ErrSlackPostFailed)
	}
	return nil
}

// severityColor maps severity strings to the Slack attachment color
// convention (good=green / warning=yellow / danger=red).
func severityColor(severity string) string {
	switch severity {
	case "error", "critical":
		return "danger"
	case "warning":
		return "warning"
	default:
		return "good"
	}
}
