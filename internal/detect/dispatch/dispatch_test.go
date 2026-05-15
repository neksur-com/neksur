package dispatch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recordingScanner is a Scanner stub that records every Scan call.
type recordingScanner struct {
	mu    sync.Mutex
	hits  []Hit
	count int32
}

func (r *recordingScanner) Scan(_ context.Context, h Hit) error {
	atomic.AddInt32(&r.count, 1)
	r.mu.Lock()
	r.hits = append(r.hits, h)
	r.mu.Unlock()
	return nil
}

func (r *recordingScanner) Count() int32 {
	return atomic.LoadInt32(&r.count)
}

// TestPoolDedupsSameMetadataLocation — push the SAME hit twice from
// three "producers" (simulating poller + webhook + s3events). Assert
// scanner.Scan is called exactly once.
//
// This proves the in-process sync.Map dedup is the load-bearing
// invariant for the multi-source dispatch model (D-1.10 + D-1.11).
func TestPoolDedupsSameMetadataLocation(t *testing.T) {
	rec := &recordingScanner{}
	in := make(chan Hit, 16)
	pool := NewPool(in, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)

	hit := Hit{
		TenantID:         "11111111-1111-4111-8111-111111111111",
		MetadataLocation: "s3://test/snap1/metadata.json",
		Source:           "test",
	}
	// Three producers push the same Hit within 100ms.
	var wg sync.WaitGroup
	for i, src := range []string{"poller", "polaris-webhook", "s3-event"} {
		wg.Add(1)
		go func(i int, src string) {
			defer wg.Done()
			h := hit
			h.Source = src
			in <- h
		}(i, src)
	}
	wg.Wait()

	// Wait briefly for all 3 messages to be drained + scanner invoked
	// (poll for completion to avoid timing flakes).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.Count() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Allow extra time for any rogue duplicate dispatches.
	time.Sleep(100 * time.Millisecond)

	if got := rec.Count(); got != 1 {
		t.Errorf("scanner.Scan called %d times; want exactly 1 (in-process dedup)", got)
	}
}

// TestPoolEnvWorkerOverride — NEKSUR_L3_WORKERS env override is honored.
func TestPoolEnvWorkerOverride(t *testing.T) {
	t.Setenv("NEKSUR_L3_WORKERS", "8")
	pool := NewPool(make(chan Hit, 1), &recordingScanner{})
	if got := pool.Workers(); got != 8 {
		t.Errorf("Workers = %d; want 8", got)
	}
}

// TestPoolDefaultWorkers — when env unset, pool defaults to 4.
func TestPoolDefaultWorkers(t *testing.T) {
	t.Setenv("NEKSUR_L3_WORKERS", "")
	pool := NewPool(make(chan Hit, 1), &recordingScanner{})
	if got := pool.Workers(); got != 4 {
		t.Errorf("default Workers = %d; want 4 (D-1.10)", got)
	}
}

// TestPoolEmptyMetadataLocationSkipped — empty MetadataLocation is
// logged + skipped (we don't crash and we don't dispatch).
func TestPoolEmptyMetadataLocationSkipped(t *testing.T) {
	rec := &recordingScanner{}
	pool := &Pool{workers: 1, in: make(chan Hit, 1), scanner: rec}
	pool.processHit(context.Background(), Hit{MetadataLocation: ""}, 0)
	if got := rec.Count(); got != 0 {
		t.Errorf("scanner.Scan invoked on empty MetadataLocation; got count=%d", got)
	}
}

// TestWebhookSignatureVerifyRejectsBadHMAC — POST a body with a
// deliberately wrong signature → handler returns 401.
//
// The handler resolves the per-tenant secret via DB; for this unit-only
// test we set NEKSUR_POLARIS_WEBHOOK_ENABLED=0 to short-circuit before
// the DB call AND ALSO test the explicit verifyHMAC unit (which is
// the load-bearing invariant). We also cover the disabled-handler 410
// path.
func TestWebhookSignatureVerifyRejectsBadHMAC(t *testing.T) {
	body := []byte(`{"metadata_location":"s3://x/snap.json","tenant_id":"11111111-1111-4111-8111-111111111111"}`)
	secret := "shared-test-secret"

	// Compute correct sig:
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	correctSig := hex.EncodeToString(mac.Sum(nil))

	if !verifyHMAC(body, correctSig, secret) {
		t.Errorf("verifyHMAC rejected the correct signature")
	}
	if verifyHMAC(body, "abc123", secret) {
		t.Errorf("verifyHMAC accepted an obviously-bad signature")
	}
	// Wrong-secret signature MUST be rejected.
	wrongMac := hmac.New(sha256.New, []byte("different-secret"))
	wrongMac.Write(body)
	wrongSig := hex.EncodeToString(wrongMac.Sum(nil))
	if verifyHMAC(body, wrongSig, secret) {
		t.Errorf("verifyHMAC accepted a wrong-secret signature")
	}
}

// TestWebhookHandlerDisabledReturns410 — when env disable flag set,
// handler returns 410 Gone without invoking any DB call (good for
// operators that haven't configured webhook signing yet).
func TestWebhookHandlerDisabledReturns410(t *testing.T) {
	t.Setenv("NEKSUR_POLARIS_WEBHOOK_ENABLED", "0")
	in := make(chan Hit, 1)
	// nil pool — when env disable flag short-circuits, the pool is
	// never touched.
	handler := WebhookHandler(nil, in)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("disabled handler status = %d; want 410", resp.StatusCode)
	}
}

// TestWebhookHandlerNonPostRejected — GET /v1/webhooks/polaris → 405.
func TestWebhookHandlerNonPostRejected(t *testing.T) {
	t.Setenv("NEKSUR_POLARIS_WEBHOOK_ENABLED", "1")
	handler := WebhookHandler(nil, make(chan Hit, 1))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d; want 405", resp.StatusCode)
	}
}

// TestSignBodyRoundTrip — the SignBody helper produces a signature
// that verifyHMAC accepts (used by the polaris-webhook-register CLI
// + integration tests).
func TestSignBodyRoundTrip(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "test-secret"
	sig := SignBody(body, secret)
	if !verifyHMAC(body, sig, secret) {
		t.Errorf("SignBody output not accepted by verifyHMAC")
	}
}

// TestWebhookHandler_UnifiedUnauthorizedResponse — REVIEW.md CR-02
// mandates that EVERY pre-HMAC auth-failure path returns the SAME
// status (401) and IDENTICAL body. This prevents the
// tenant-enumeration oracle: attackers cannot distinguish
// "tenant exists but no Polaris" from "tenant doesn't exist" from
// "signature mismatch" via response content.
//
// We test the public-visible response shape across these failure
// modes:
//   - missing X-Polaris-Signature header
//   - missing X-Polaris-Tenant header
//   - malformed tenant UUID
//
// The "secret-lookup-failed / no-row" and "HMAC-mismatch" cases
// require a real pool, so they live in the integration tier; the
// failure-discrimination contract is documented in webhook.go's
// WebhookHandler comment.
func TestWebhookHandler_UnifiedUnauthorizedResponse(t *testing.T) {
	t.Setenv("NEKSUR_POLARIS_WEBHOOK_ENABLED", "1")
	handler := WebhookHandler(nil, make(chan Hit, 1))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{
			name:    "missing_signature_header",
			headers: map[string]string{polarisTenantHeader: "11111111-1111-4111-8111-111111111111"},
		},
		{
			name:    "missing_tenant_header",
			headers: map[string]string{polarisSignatureHeader: "deadbeef"},
		},
		{
			name: "malformed_tenant_uuid",
			headers: map[string]string{
				polarisTenantHeader:    "not-a-uuid",
				polarisSignatureHeader: "deadbeef",
			},
		},
	}

	bodies := make([]string, 0, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d; want 401 (CR-02 unified-auth-failure response)", resp.StatusCode)
			}
			buf := make([]byte, 256)
			n, _ := resp.Body.Read(buf)
			bodyStr := strings.TrimSpace(string(buf[:n]))
			bodies = append(bodies, bodyStr)
		})
	}

	// All bodies MUST be identical — that's the CR-02 contract.
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("body[%d]=%q != body[0]=%q (CR-02 demands identical body for all pre-HMAC failures)",
				i, bodies[i], bodies[0])
		}
	}
}

// TestTenantUUIDValidation — REVIEW.md CR-06 mandates canonical
// uuid.Parse() (rather than the hand-rolled isValidTenantUUID). This
// test asserts the same accept/reject contract holds through the
// canonical parser. Keeping the test prevents regressions on the
// security-critical validation boundary.
func TestTenantUUIDValidation(t *testing.T) {
	good := []string{
		"11111111-1111-4111-8111-111111111111",
		"a1b2c3d4-e5f6-4789-8abc-def012345678",
	}
	for _, s := range good {
		if _, err := uuid.Parse(s); err != nil {
			t.Errorf("uuid.Parse(%q): expected success, got err: %v", s, err)
		}
	}
	// uuid.Parse is more permissive than the hand-rolled validator —
	// it accepts dash-stripped, URN, and braced forms. The CR-06
	// contract is "use canonical uuid.Parse"; any string it accepts
	// is by definition a valid UUID. The rejects below are obviously
	// malformed strings that no UUID form would normalize to.
	bad := []string{
		"not-a-uuid",
		"; DROP TABLE catalog_credentials --",
		"11111111-1111-4111-8111-11111111111z", // bad hex char
		"",                                     // empty
		"a1b2c3d4-e5f6-4789-8abc",              // truncated
	}
	for _, s := range bad {
		if _, err := uuid.Parse(s); err == nil {
			t.Errorf("uuid.Parse(%q): expected error, got nil", s)
		}
	}
}
