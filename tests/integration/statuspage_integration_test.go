//go:build integration

// Plan 00.5-05 Task 3 — Statuspage integration test.
//
// Mocks the Atlassian Statuspage REST API via httptest.NewServer; drives
// observability.Statuspage.UpdateComponent and asserts the captured
// PATCH body contains the right component.status field.
//
// The happy-path (incident open/resolve) goes through PagerDuty's
// native Statuspage integration and does NOT route through this Go
// client (D-0.5.14 + RESEARCH §Pattern 7 line 1005). This test covers
// the programmatic-update path used for scheduled maintenance windows.

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

// TestStatuspageIntegration mocks the Statuspage REST API and asserts
// the PATCH body shape sent by UpdateComponent.
func TestStatuspageIntegration(t *testing.T) {
	var captured struct {
		Method      string
		Path        string
		AuthHeader  string
		ContentType string
		Body        map[string]interface{}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.Path
		captured.AuthHeader = r.Header.Get("Authorization")
		captured.ContentType = r.Header.Get("Content-Type")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured.Body); err != nil {
			t.Errorf("decode body: %v body=%s", err, string(body))
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"comp_123","status":"degraded_performance"}`))
	}))
	defer srv.Close()

	client := observability.NewStatuspage("apikey_test", "page_xyz", srv.URL, srv.Client())
	if err := client.UpdateComponent(context.Background(), "comp_123", "degraded_performance"); err != nil {
		t.Fatalf("UpdateComponent: %v", err)
	}

	if captured.Method != http.MethodPatch {
		t.Errorf("expected PATCH method, got %s", captured.Method)
	}
	if !strings.HasSuffix(captured.Path, "/v1/pages/page_xyz/components/comp_123") {
		t.Errorf("unexpected path: %s", captured.Path)
	}
	if !strings.HasPrefix(captured.AuthHeader, "OAuth ") {
		t.Errorf("expected OAuth auth header, got %q", captured.AuthHeader)
	}
	if captured.ContentType != "application/json" {
		t.Errorf("expected application/json content-type, got %q", captured.ContentType)
	}
	componentObj, ok := captured.Body["component"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected component object, body=%+v", captured.Body)
	}
	if componentObj["status"] != "degraded_performance" {
		t.Errorf("expected component.status=degraded_performance, got %v", componentObj["status"])
	}
}

// TestStatuspageNon2xxReturnsError — server 4xx response surfaces as an
// error containing the status code.
func TestStatuspageNon2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	client := observability.NewStatuspage("apikey", "page", srv.URL, srv.Client())
	err := client.UpdateComponent(context.Background(), "comp_x", "operational")
	if err == nil {
		t.Fatal("expected error on 403 response, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected error mentioning HTTP 403, got %v", err)
	}
}
