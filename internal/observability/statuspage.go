package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Statuspage is the programmatic-update client for atlassian-statuspage.io
// (D-0.5.14). For the happy path (incident open → resolve), Neksur SaaS
// relies on PagerDuty's NATIVE statuspage integration (configured in the
// PagerDuty dashboard, not via Terraform) which auto-updates component
// status on incident state transitions.
//
// This client exists for the scheduled-maintenance-window use case where
// ops wants to manually flip a component to `under_maintenance` ahead of
// a deployment window. The Atlassian Statuspage REST API is `PATCH
// /v1/pages/{page_id}/components/{component_id}` with
// `Authorization: OAuth <api_key>`.
//
// API key: stored in AWS Secrets Manager under `neksur/statuspage/api_key`
// (Plan 01 module/billing-secrets); read at startup via env / Secrets
// Manager SDK.
type Statuspage struct {
	apiKey  string
	pageID  string
	baseURL string
	http    *http.Client
}

// NewStatuspage constructs a Statuspage client.
//
// baseURL defaults to "https://api.statuspage.io" if empty.
func NewStatuspage(apiKey, pageID, baseURL string, httpClient *http.Client) *Statuspage {
	if baseURL == "" {
		baseURL = "https://api.statuspage.io"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Statuspage{apiKey: apiKey, pageID: pageID, baseURL: baseURL, http: httpClient}
}

// UpdateComponent sets the status of a statuspage component.
//
// status must be one of:
//   - "operational"
//   - "under_maintenance"
//   - "degraded_performance"
//   - "partial_outage"
//   - "major_outage"
//
// On non-2xx HTTP responses, returns an error containing the response
// status code and a truncated body.
func (s *Statuspage) UpdateComponent(ctx context.Context, componentID, status string) error {
	url := fmt.Sprintf("%s/v1/pages/%s/components/%s", s.baseURL, s.pageID, componentID)

	bodyJSON, err := json.Marshal(map[string]map[string]string{
		"component": {"status": status},
	})
	if err != nil {
		return fmt.Errorf("observability: statuspage marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return fmt.Errorf("observability: statuspage new request: %w", err)
	}
	req.Header.Set("Authorization", "OAuth "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("observability: statuspage do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("observability: statuspage HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
