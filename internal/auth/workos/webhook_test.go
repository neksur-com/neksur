package workos

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signWorkOSPayload produces a WorkOS-format signature header for the
// given body + secret + timestamp. Header format:
//
//	t=<unix_ms>,v1=<hex_hmac_sha256>
//
// where the HMAC is computed over `<unix_ms>.<body>` per the SDK
// reference implementation in
// /Users/evgeny/go/pkg/mod/github.com/workos/workos-go/v4@v4.16.0/pkg/webhooks/client.go
// (lines 94-107).
func signWorkOSPayload(t *testing.T, secret, body string, ts time.Time) string {
	t.Helper()
	tsStr := fmt.Sprintf("%d", ts.UnixMilli())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsStr + "." + body))
	digest := hex.EncodeToString(mac.Sum(nil))
	// WorkOS SDK expects "t=<ts>, v1=<sig>" — note the SPACE after the comma.
	// The SDK strips signatureParts[1][4:], which assumes a leading
	// " v1=" (4 chars). See workos-go/v4/pkg/webhooks/client_test.go::mockWebhookHeader.
	return fmt.Sprintf("t=%s, v1=%s", tsStr, digest)
}

// newTestClient creates a Client suitable for webhook handler tests.
// JWKS endpoint isn't exercised so we just point it at "" (won't fetch).
func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClientWithEndpoint("sk_test", "client_test", ".test.local", "http://localhost:1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestWebhookSig_ValidSignatureReturns200(t *testing.T) {
	t.Setenv("SCIM_ENABLED", "false") // verify still returns 200
	const secret = "webhook_secret_abc"
	c := newTestClient(t)
	h := c.HandleWebhook(secret)

	body := `{"event":"dsync.user.created","data":{"id":"u1"}}`
	sig := signWorkOSPayload(t, secret, body, time.Now())

	r := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	r.Header.Set("WorkOS-Signature", sig)
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
}

func TestWebhookSig_CorruptSignatureReturns400EmptyBody(t *testing.T) {
	t.Setenv("SCIM_ENABLED", "false")
	const secret = "webhook_secret_abc"
	c := newTestClient(t)
	h := c.HandleWebhook(secret)

	body := `{"event":"dsync.user.created"}`
	sig := signWorkOSPayload(t, "WRONG_SECRET", body, time.Now()) // wrong sig

	r := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	r.Header.Set("WorkOS-Signature", sig)
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", w.Code)
	}
	// "Empty body" per D-0.5.21: http.Error appends a single newline; we
	// accept "" or "\n". An attacker probing would learn nothing more.
	gotBody, _ := io.ReadAll(w.Result().Body)
	if s := strings.TrimSpace(string(gotBody)); s != "" {
		t.Errorf("body = %q; want empty (or whitespace-only)", string(gotBody))
	}
}

func TestWebhookSig_NoSignatureReturns400EmptyBody(t *testing.T) {
	t.Setenv("SCIM_ENABLED", "false")
	const secret = "webhook_secret_abc"
	c := newTestClient(t)
	h := c.HandleWebhook(secret)

	body := `{"event":"dsync.user.created"}`
	r := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	// no WorkOS-Signature header
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", w.Code)
	}
}

// TestWebhookSig_ScimDisabledStillVerifiesFirst — the critical D-0.5.21
// test: even with SCIM_ENABLED=false, signature verification runs FIRST.
// A wrong signature returns 400 (not 200), confirming the order of
// operations in webhook.go.
func TestWebhookSig_ScimDisabledStillVerifiesFirst(t *testing.T) {
	t.Setenv("SCIM_ENABLED", "false")
	const secret = "webhook_secret_abc"
	c := newTestClient(t)
	h := c.HandleWebhook(secret)

	body := `{"event":"dsync.user.created"}`
	sig := signWorkOSPayload(t, "WRONG_SECRET", body, time.Now())

	r := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	r.Header.Set("WorkOS-Signature", sig)
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("verify-before-flag-check broken: SCIM_ENABLED=false + wrong sig returned %d; want 400", w.Code)
	}
}

// TestWebhookSig_ScimEnabledValidSig — when SCIM_ENABLED=true and sig
// is valid, handler returns 200 and dispatches to the (empty Phase 0.5)
// switch.
func TestWebhookSig_ScimEnabledValidSig(t *testing.T) {
	t.Setenv("SCIM_ENABLED", "true")
	const secret = "webhook_secret_abc"
	c := newTestClient(t)
	h := c.HandleWebhook(secret)

	body := `{"event":"dsync.user.created","data":{"id":"u1"}}`
	sig := signWorkOSPayload(t, secret, body, time.Now())

	r := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	r.Header.Set("WorkOS-Signature", sig)
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("SCIM_ENABLED=true + valid sig: status = %d; want 200", w.Code)
	}
}
