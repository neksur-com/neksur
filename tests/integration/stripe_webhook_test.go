//go:build integration

// Plan 00.5-05 Task 3 — Stripe webhook integration tests.
//
// These tests exercise the end-to-end HTTP path for /webhooks/stripe:
// httptest.NewServer wraps billing.WebhookHandler around either
// NoopBilling or stripeBilling, and we drive valid + invalid signatures
// to assert the D-0.5.21 T-0.5-stripe-spoof contract:
//
//   1. Verify-before-flag-check: even with BILLING_ENABLED=false (the
//      Phase 0.5 baseline), an UNSIGNED or wrong-signature webhook is
//      rejected with HTTP 400 + empty body.
//   2. Verify-then-200: with valid signature, the handler returns 200
//      regardless of BILLING_ENABLED (event is ignored under NoopBilling).
//   3. Missing-header path: no Stripe-Signature → 400 + empty body.
//
// The signature helper uses stripe-go/v82/webhook.GenerateTestSignedPayload
// (RESEARCH §Don't Hand-Roll line 1021) so we don't roll our own HMAC.
//
// A fourth test, TestStripeCLIReplay, exercises the real `stripe trigger`
// CLI shell-out path when STRIPE_CLI=1 — otherwise skipped (the CLI
// requires a live Stripe sandbox + `stripe login`).

package integration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/neksur-com/neksur/internal/billing"
)

// signedStripeEvent returns (sigHeader, body) — a valid Stripe-Signature
// header and matching event body. The body carries the SDK's pinned
// api_version so `webhook.ConstructEvent` doesn't reject on
// release-train mismatch.
func signedStripeEvent(t *testing.T, secret, eventID, eventType string) (string, []byte) {
	t.Helper()
	body := []byte(`{"id":"` + eventID + `","type":"` + eventType + `","api_version":"` + stripe.APIVersion + `"}`)
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   body,
		Secret:    secret,
		Timestamp: time.Now(),
	})
	return sp.Header, sp.Payload
}

// TestStripeWebhookSig exercises the happy path + the corrupt-sig path
// against billing.WebhookHandler(NoopBilling). REQ-billing-stripe.
func TestStripeWebhookSig(t *testing.T) {
	const secret = "whsec_test_secret"
	b := billing.NewBilling(billing.Config{Enabled: false, WebhookSecret: secret})

	srv := httptest.NewServer(billing.WebhookHandler(b, secret))
	defer srv.Close()

	// Sub-test 1: valid signature → 200.
	t.Run("valid_signature_200", func(t *testing.T) {
		sigHeader, body := signedStripeEvent(t, secret, "evt_valid", "customer.subscription.created")
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
		req.Header.Set("Stripe-Signature", sigHeader)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Sub-test 2: corrupt signature → 400 + empty body.
	t.Run("corrupt_signature_400_empty", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
		req.Header.Set("Stripe-Signature", "t=0,v1=deadbeef")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
		if got := strings.TrimSpace(string(body)); got != "" {
			t.Fatalf("expected empty body, got %q", got)
		}
	})
}

// TestStripeWebhookUnsignedReturns400 — no Stripe-Signature header at
// all returns 400 + empty body. D-0.5.21 silent 400.
func TestStripeWebhookUnsignedReturns400(t *testing.T) {
	const secret = "whsec_test_secret"
	b := billing.NewBilling(billing.Config{Enabled: false, WebhookSecret: secret})

	srv := httptest.NewServer(billing.WebhookHandler(b, secret))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
	// Deliberately no Stripe-Signature header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "" {
		t.Fatalf("expected empty body, got %q", got)
	}
}

// TestStripeWebhookEnabledFalseStillVerifies — T-0.5-stripe-spoof contract
// at the HTTP layer: with BILLING_ENABLED=false (NoopBilling), an
// unsigned webhook returns 400; a validly-signed webhook returns 200.
// This is the integration-tier echo of internal/billing's unit test;
// here we drive it through the real HTTP handler stack.
func TestStripeWebhookEnabledFalseStillVerifies(t *testing.T) {
	const secret = "whsec_test_secret"
	b := billing.NewBilling(billing.Config{Enabled: false, WebhookSecret: secret})

	srv := httptest.NewServer(billing.WebhookHandler(b, secret))
	defer srv.Close()

	// Valid sig under BILLING_ENABLED=false → 200.
	t.Run("valid_sig_under_disabled_flag_returns_200", func(t *testing.T) {
		sigHeader, body := signedStripeEvent(t, secret, "evt_dis_valid", "customer.subscription.created")
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
		req.Header.Set("Stripe-Signature", sigHeader)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// Corrupt sig under BILLING_ENABLED=false → 400 (T-0.5-stripe-spoof
	// at the implementation level — NoopBilling still verifies).
	t.Run("corrupt_sig_under_disabled_flag_returns_400", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
		req.Header.Set("Stripe-Signature", "t=0,v1=ffff")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 (T-0.5-stripe-spoof), got %d", resp.StatusCode)
		}
	})
}

// TestStripeCLIReplay — gated on STRIPE_CLI=1. Shells out to
// `stripe trigger customer.subscription.created --secret=<webhook_secret>
// --forward-to=<srv.URL>` to drive a real Stripe CLI event replay
// through the webhook handler. Requires `stripe` CLI installed and
// `stripe login` already complete.
func TestStripeCLIReplay(t *testing.T) {
	if os.Getenv("STRIPE_CLI") != "1" {
		t.Skip("STRIPE_CLI != 1; skipping Stripe CLI replay test (requires `stripe login`)")
	}
	const secret = "whsec_test_secret"
	b := billing.NewBilling(billing.Config{Enabled: false, WebhookSecret: secret})
	srv := httptest.NewServer(billing.WebhookHandler(b, secret))
	defer srv.Close()

	cmd := exec.Command("stripe", "trigger", "customer.subscription.created",
		"--api-key", os.Getenv("STRIPE_API_KEY"),
		"--forward-to", srv.URL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stripe trigger: %v\noutput: %s", err, string(out))
	}
	// The CLI's `forward-to` mode prints "Setting up fixtures..." then
	// the HTTP response status. We just check non-empty + no panic.
	if len(out) == 0 {
		t.Fatal("stripe trigger produced no output")
	}
}
