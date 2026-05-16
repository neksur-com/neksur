// transport_test.go — unit tests for sessionPolicyTransport, the
// http.RoundTripper that injects the X-Iceberg-Session-Policy header
// onto outbound LoadTable requests carrying a session policy in their
// context.
//
// These tests do NOT depend on the Polaris testcontainer — they exercise
// the RoundTripper in isolation by feeding it a stub `next` transport
// and asserting header shape.
package polaris

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

// stubRoundTripper records the http.Request handed to RoundTrip and
// returns a fixed canned response. The stub never makes a real network
// call.
type stubRoundTripper struct {
	called   int
	gotReq   *http.Request
	canned   *http.Response
	cannedEr error
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.called++
	s.gotReq = req
	if s.cannedEr != nil {
		return nil, s.cannedEr
	}
	if s.canned != nil {
		return s.canned, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Header:     http.Header{},
	}, nil
}

// TestSessionPolicyTransport_HeaderPropagated: when the ctx carries a
// session policy via contextWithSessionPolicy, the outbound request's
// X-Iceberg-Session-Policy header MUST carry the JSON bytes verbatim.
func TestSessionPolicyTransport_HeaderPropagated(t *testing.T) {
	t.Parallel()

	next := &stubRoundTripper{}
	tr := &sessionPolicyTransport{next: next}

	policy := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":["arn:aws:s3:::bucket/ns/tbl/*"],"Condition":{"StringEquals":{"aws:RequestedRegion":"us-east-1"}}}]}`)
	ctx := contextWithSessionPolicy(context.Background(), policy)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://polaris.example/v1/namespaces/ns/tables/tbl", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if next.called != 1 {
		t.Fatalf("next RoundTripper called %d times; want 1", next.called)
	}

	got := next.gotReq.Header.Get("X-Iceberg-Session-Policy")
	if got != string(policy) {
		t.Errorf("X-Iceberg-Session-Policy mismatch:\n got: %q\nwant: %q", got, string(policy))
	}

	// The original request handed to RoundTrip must NOT be mutated —
	// the transport must Clone the request before writing the header.
	if req.Header.Get("X-Iceberg-Session-Policy") != "" {
		t.Errorf("caller's req.Header was mutated; want clean (got %q)",
			req.Header.Get("X-Iceberg-Session-Policy"))
	}
}

// TestSessionPolicyTransport_NoPolicyNoHeader: a ctx WITHOUT a session
// policy produces an outbound request with NO X-Iceberg-Session-Policy
// header. This is the no-op default path — non-L4 LoadTable calls
// (ListTables, GetTable, etc.) MUST NOT carry the policy header.
func TestSessionPolicyTransport_NoPolicyNoHeader(t *testing.T) {
	t.Parallel()

	next := &stubRoundTripper{}
	tr := &sessionPolicyTransport{next: next}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://polaris.example/v1/namespaces/ns/tables/tbl", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if got := next.gotReq.Header.Get("X-Iceberg-Session-Policy"); got != "" {
		t.Errorf("X-Iceberg-Session-Policy unexpectedly set when no policy in ctx: %q", got)
	}
}

// TestSessionPolicyTransport_DelegatesToNext: the stub `next` is called
// exactly once and its response (including error case) is propagated
// verbatim.
func TestSessionPolicyTransport_DelegatesToNext(t *testing.T) {
	t.Parallel()

	t.Run("success_response", func(t *testing.T) {
		t.Parallel()
		canned := &http.Response{
			StatusCode: http.StatusTeapot,
			Body:       io.NopCloser(bytes.NewReader([]byte("brew"))),
			Header:     http.Header{},
		}
		next := &stubRoundTripper{canned: canned}
		tr := &sessionPolicyTransport{next: next}

		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://x", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if resp != canned {
			t.Errorf("response not propagated verbatim; got %p want %p", resp, canned)
		}
		if next.called != 1 {
			t.Errorf("next called %d; want 1", next.called)
		}
	})

	t.Run("error_propagation", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("boom")
		next := &stubRoundTripper{cannedEr: wantErr}
		tr := &sessionPolicyTransport{next: next}

		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://x", nil)
		_, err := tr.RoundTrip(req)
		if !errors.Is(err, wantErr) {
			t.Errorf("error not propagated: got %v want %v", err, wantErr)
		}
	})
}

// TestSessionPolicyTransport_EmptyPolicyTreatedAsAbsent: a zero-length
// []byte attached to the ctx is treated as "no policy" — no header is
// emitted. Defensive: prevents callers from accidentally setting an
// empty header on the wire.
func TestSessionPolicyTransport_EmptyPolicyTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	next := &stubRoundTripper{}
	tr := &sessionPolicyTransport{next: next}

	ctx := contextWithSessionPolicy(context.Background(), []byte{})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := next.gotReq.Header.Get("X-Iceberg-Session-Policy"); got != "" {
		t.Errorf("empty policy produced header: %q", got)
	}
}
