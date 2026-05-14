package billing

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// testEventBody returns a valid Stripe-event JSON shape that carries the
// SDK's pinned API version (so webhook.ConstructEvent doesn't reject on
// api_version mismatch). Format must match the canonical
// `<yyyy-MM-dd>.<release-train>` shape; the SDK only compares release
// trains, so any date in the same train passes isCompatibleAPIVersion.
func testEventBody(id, typ string) string {
	return `{"id":"` + id + `","type":"` + typ + `","api_version":"` + stripe.APIVersion + `"}`
}

// signedPayload returns a Stripe-Signature header + body pair that
// `webhook.ConstructEvent` will accept with the given secret. Used by
// both the noop-billing valid-sig and stripe-billing valid-sig cases.
func signedPayload(secret string, body string) (string, []byte) {
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   []byte(body),
		Secret:    secret,
		Timestamp: time.Now(),
	})
	return sp.Header, sp.Payload
}

// TestWebhookHandlerValidSignatureNoopOK: NoopBilling, valid signature →
// 200. The event itself is ignored (BILLING_ENABLED=false) but the
// signature verification passed.
func TestWebhookHandlerValidSignatureNoopOK(t *testing.T) {
	const secret = "whsec_test_secret"
	b := NewBilling(Config{Enabled: false, WebhookSecret: secret})
	handler := WebhookHandler(b, secret)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	sigHeader, body := signedPayload(secret, testEventBody("evt_test", "customer.subscription.created"))

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Stripe-Signature", sigHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}
}

// TestWebhookHandlerInvalidSignature400Empty: NoopBilling, corrupt
// signature → 400 + empty response body. D-0.5.21 silent 400 (no
// information leak).
func TestWebhookHandlerInvalidSignature400Empty(t *testing.T) {
	const secret = "whsec_test_secret"
	b := NewBilling(Config{Enabled: false, WebhookSecret: secret})
	handler := WebhookHandler(b, secret)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"id":"evt_test"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Corrupt signature.
	req.Header.Set("Stripe-Signature", "t=12345,v1=ffffffff")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	// http.Error appends a newline; strip it.
	if got := strings.TrimSpace(string(respBody)); got != "" {
		t.Fatalf("expected empty body, got %q", got)
	}
}

// TestWebhookHandlerMissingSignature400Empty: NoopBilling, no
// Stripe-Signature header → 400 + empty body. Same D-0.5.21 contract.
func TestWebhookHandlerMissingSignature400Empty(t *testing.T) {
	const secret = "whsec_test_secret"
	b := NewBilling(Config{Enabled: false, WebhookSecret: secret})
	handler := WebhookHandler(b, secret)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"id":"evt_test"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// No Stripe-Signature header at all.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(respBody)); got != "" {
		t.Fatalf("expected empty body, got %q", got)
	}
}

// TestWebhookHandlerEnabledFalseStillVerifies: T-0.5-stripe-spoof — even
// with BILLING_ENABLED=false (NoopBilling), an UNSIGNED webhook is
// rejected with 400. This is the implementation-layer defense against
// operator misconfig of BILLING_ENABLED.
func TestWebhookHandlerEnabledFalseStillVerifies(t *testing.T) {
	const secret = "whsec_test_secret"
	b := NewBilling(Config{Enabled: false, WebhookSecret: secret})
	handler := WebhookHandler(b, secret)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// First sub-case: corrupt signature → 400.
	{
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
		req.Header.Set("Stripe-Signature", "t=0,v1=deadbeef")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do (corrupt): %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 on corrupt sig (BILLING_ENABLED=false), got %d", resp.StatusCode)
		}
	}

	// Second sub-case: valid signature with BILLING_ENABLED=false → 200
	// (event ignored but signature checked).
	{
		sigHeader, body := signedPayload(secret, testEventBody("evt_2", "customer.subscription.created"))
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
		req.Header.Set("Stripe-Signature", sigHeader)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do (valid): %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on valid sig (BILLING_ENABLED=false), got %d", resp.StatusCode)
		}
	}
}
