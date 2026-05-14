//go:build integration

// Plan 00.5-05 Task 3 — PagerDuty alarm pipeline integration tests.
//
//   1. TestAlarmToPagerDutyPipeline — mocks the PagerDuty Events API v2
//      via httptest.NewServer, captures the POST body, and asserts the
//      dedup_key / severity / event_action / payload fields the
//      alerts.DispatchAlarm function produced from an AlarmPayload.
//   2. TestDedupKeyFormat — table-driven verification of the
//      <service>:<alert>:<tenant_or_global> format (D-0.5.13). Pure
//      function; no HTTP roundtrip needed.
//   3. TestAlarmResolveAction — drives the same dispatcher with
//      NewStateValue="OK" and asserts the captured event_action is
//      "resolve" (not "trigger").

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neksur-com/neksur/internal/observability"
)

// pdMockServer constructs a stand-in for the PagerDuty Events API v2.
// It captures POST bodies into `*captured` (a slice) and always returns
// 202 Accepted with the canonical V2EventResponse shape. The returned
// closer must be deferred by the test.
func pdMockServer(t *testing.T, captured *[]map[string]interface{}) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("pdMockServer: read body: %v", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		// PagerDuty SDK posts to /v2/enqueue; record the request body shape.
		if !strings.HasSuffix(r.URL.Path, "/v2/enqueue") {
			t.Errorf("pdMockServer: unexpected path %s", r.URL.Path)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("pdMockServer: json decode: %v body=%s", err, string(body))
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		*captured = append(*captured, m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","message":"Event processed","dedup_key":"x"}`))
	}))
	return srv.URL, srv.Close
}

// TestAlarmToPagerDutyPipeline drives alerts.DispatchAlarm against a
// mocked PagerDuty Events API, then inspects the captured POST body
// to assert the dedup_key / severity / event_action / source.
func TestAlarmToPagerDutyPipeline(t *testing.T) {
	var captured []map[string]interface{}
	url, closer := pdMockServer(t, &captured)
	defer closer()

	pd := observability.NewPagerDutyWithEndpoint("rk_test", "neksur-saas", url)
	payload := observability.AlarmPayload{
		AlarmName:        "neksur-rds-pool-a-cpu-high",
		NewStateValue:    "ALARM",
		AlarmDescription: "Pool A primary CPU >80% for 2 minutes.",
		Dimensions: []observability.Dim{
			{Name: "TenantID", Value: "tenant-1"},
		},
	}
	if err := observability.DispatchAlarm(context.Background(), pd, payload); err != nil {
		t.Fatalf("DispatchAlarm: %v", err)
	}

	if got := len(captured); got != 1 {
		t.Fatalf("expected 1 captured event, got %d", got)
	}
	evt := captured[0]
	if evt["event_action"] != "trigger" {
		t.Errorf("expected event_action=trigger, got %v", evt["event_action"])
	}
	if evt["routing_key"] != "rk_test" {
		t.Errorf("expected routing_key=rk_test, got %v", evt["routing_key"])
	}
	// DedupKey format: neksur-saas:neksur-rds-pool-a-cpu-high:tenant-1
	if got := evt["dedup_key"]; got != "neksur-saas:neksur-rds-pool-a-cpu-high:tenant-1" {
		t.Errorf("unexpected dedup_key: %v", got)
	}
	pl, _ := evt["payload"].(map[string]interface{})
	if pl == nil {
		t.Fatal("missing payload object")
	}
	// Severity bumps to "warning" by default for this alarm name (does
	// not contain "storage" or "5xx").
	if pl["severity"] != "warning" {
		t.Errorf("expected severity=warning, got %v", pl["severity"])
	}
	if pl["source"] != "neksur-saas" {
		t.Errorf("expected source=neksur-saas, got %v", pl["source"])
	}
	if pl["component"] != "neksur-rds-pool-a-cpu-high" {
		t.Errorf("expected component=alarmname, got %v", pl["component"])
	}
}

// TestDedupKeyFormat is a table-driven check on the
// `<service>:<alert>:<tenant_or_global>` pattern (D-0.5.13).
// Pure function — no HTTP roundtrip required.
func TestDedupKeyFormat(t *testing.T) {
	cases := []struct {
		name      string
		service   string
		alert     string
		tenant    string
		expected  string
	}{
		{
			name:     "tenant-scoped",
			service:  "neksur-saas",
			alert:    "rds-pool-a-cpu-high",
			tenant:   "tenant-abc",
			expected: "neksur-saas:rds-pool-a-cpu-high:tenant-abc",
		},
		{
			name:     "global-scope-empty-tenant",
			service:  "neksur-saas",
			alert:    "alb-5xx-burst",
			tenant:   "",
			expected: "neksur-saas:alb-5xx-burst:global",
		},
		{
			name:     "alternate-service",
			service:  "neksur-cli",
			alert:    "tenant-provisioning-failed",
			tenant:   "tenant-xyz",
			expected: "neksur-cli:tenant-provisioning-failed:tenant-xyz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := observability.DedupKey(tc.service, tc.alert, tc.tenant)
			if got != tc.expected {
				t.Errorf("DedupKey(%q,%q,%q)=%q; expected %q",
					tc.service, tc.alert, tc.tenant, got, tc.expected)
			}
		})
	}
}

// TestAlarmResolveAction — NewStateValue="OK" flips action to "resolve".
func TestAlarmResolveAction(t *testing.T) {
	var captured []map[string]interface{}
	url, closer := pdMockServer(t, &captured)
	defer closer()

	pd := observability.NewPagerDutyWithEndpoint("rk_test", "neksur-saas", url)
	payload := observability.AlarmPayload{
		AlarmName:        "neksur-rds-pool-a-cpu-high",
		NewStateValue:    "OK",
		AlarmDescription: "Recovered.",
	}
	if err := observability.DispatchAlarm(context.Background(), pd, payload); err != nil {
		t.Fatalf("DispatchAlarm: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("expected 1 event, got %d", len(captured))
	}
	if captured[0]["event_action"] != "resolve" {
		t.Errorf("expected event_action=resolve on OK transition, got %v", captured[0]["event_action"])
	}
}

// TestAlarmSeverityCriticalBump — alarm names matching "storage" or
// "5xx" get severity=critical.
func TestAlarmSeverityCriticalBump(t *testing.T) {
	var captured []map[string]interface{}
	url, closer := pdMockServer(t, &captured)
	defer closer()

	pd := observability.NewPagerDutyWithEndpoint("rk_test", "neksur-saas", url)
	for _, name := range []string{"neksur-rds-pool-a-storage-low", "neksur-alb-5xx-burst"} {
		captured = captured[:0]
		err := observability.DispatchAlarm(context.Background(), pd, observability.AlarmPayload{
			AlarmName:        name,
			NewStateValue:    "ALARM",
			AlarmDescription: "...",
		})
		if err != nil {
			t.Fatalf("dispatch %s: %v", name, err)
		}
		if len(captured) != 1 {
			t.Fatalf("expected 1 event for %s, got %d", name, len(captured))
		}
		pl := captured[0]["payload"].(map[string]interface{})
		if pl["severity"] != "critical" {
			t.Errorf("alarm %s: expected severity=critical, got %v", name, pl["severity"])
		}
	}
}
