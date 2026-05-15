package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neksur-com/neksur/internal/detect"
)

// TestSlackPostsToWebhookURL — assert the Post call sends a POST to
// the configured webhook URL with a JSON body containing the summary
// + tenantID + details fields.
func TestSlackPostsToWebhookURL(t *testing.T) {
	var captured struct {
		method      string
		contentType string
		body        []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.contentType = r.Header.Get("Content-Type")
		captured.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewSlackWithClient(srv.URL, "neksur-server", srv.Client())
	err := s.Post(context.Background(), "warning", "PII detected: customer_email",
		"tenant-uuid-123", map[string]string{
			"column":     "customer_email",
			"tag":        "pii-email",
			"confidence": "0.92",
		})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %s; want POST", captured.method)
	}
	if captured.contentType != "application/json" {
		t.Errorf("content-type = %s; want application/json", captured.contentType)
	}

	bodyStr := string(captured.body)
	wantSubstrings := []string{
		"PII detected: customer_email",
		"tenant-uuid-123",
		"customer_email",
		"pii-email",
		"0.92",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(bodyStr, sub) {
			t.Errorf("body missing %q: %s", sub, bodyStr)
		}
	}

	// Confirm body parses as valid JSON with `text` + `attachments` keys.
	var payload map[string]any
	if err := json.Unmarshal(captured.body, &payload); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, ok := payload["text"]; !ok {
		t.Errorf("payload missing `text` key: %+v", payload)
	}
	if _, ok := payload["attachments"]; !ok {
		t.Errorf("payload missing `attachments` key: %+v", payload)
	}
}

// TestSlackReturnsErrSlackPostFailedOn500 — server returns 500 →
// errors.Is(err, detect.ErrSlackPostFailed).
func TestSlackReturnsErrSlackPostFailedOn500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewSlackWithClient(srv.URL, "neksur-server", srv.Client())
	err := s.Post(context.Background(), "warning", "boom test", "tenant-x", nil)
	if err == nil {
		t.Fatalf("Post: expected error on 500; got nil")
	}
	if !errors.Is(err, detect.ErrSlackPostFailed) {
		t.Errorf("expected errors.Is(err, detect.ErrSlackPostFailed); got %v", err)
	}
}

// TestSlackEmptyWebhookURLNoOps — when webhookURL is empty the Post
// returns nil silently (operators may ship before wiring SLACK_WEBHOOK_URL).
func TestSlackEmptyWebhookURLNoOps(t *testing.T) {
	s := NewSlack("", "neksur-server")
	err := s.Post(context.Background(), "warning", "should be no-op", "tenant-x", nil)
	if err != nil {
		t.Errorf("empty webhookURL Post: expected nil; got %v", err)
	}
}

// TestNewSlackDefaultsClient — NewSlack with empty client falls back
// to a default 10s-timeout http.Client (no panic on Post).
func TestNewSlackDefaultsClient(t *testing.T) {
	s := NewSlack("https://invalid.example.invalid/no-such-path", "tag")
	// We don't actually expect Post to succeed here (DNS will fail);
	// just confirm the constructor produces a usable struct.
	if s == nil {
		t.Fatalf("NewSlack returned nil")
	}
	if s.httpClient == nil {
		t.Errorf("NewSlack did not initialize httpClient")
	}
}

// TestSlackSeverityColors — danger / warning / good color map.
func TestSlackSeverityColors(t *testing.T) {
	cases := []struct {
		severity string
		want     string
	}{
		{"info", "good"},
		{"warning", "warning"},
		{"error", "danger"},
		{"critical", "danger"},
		{"unknown", "good"},
	}
	for _, c := range cases {
		got := severityColor(c.severity)
		if got != c.want {
			t.Errorf("severityColor(%s) = %s; want %s", c.severity, got, c.want)
		}
	}
}
