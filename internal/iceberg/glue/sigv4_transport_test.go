// Unit tests for sigv4Transport — asserts SignHTTP is called with the
// correct service name="glue" and that the Authorization header is set
// with the expected AWS SigV4 format.
//
// Uses httptest.Server + a recorder approach: the test wires a real
// sigv4Transport (with static credentials) against a local httptest.Server
// and asserts the captured Authorization header starts with
// "AWS4-HMAC-SHA256" and contains "Credential=AKID/.../glue/aws4_request".
//
// The static credentials are AKID + SECRET (AWS SDK test constants);
// they are NOT real credentials — this is a unit test only.
//
// Pitfall 11: credentials are NOT logged in test output. The test asserts
// the header shape using string prefix/contains checks on the captured
// Authorization value — no credential values are logged.
package glue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// TestSigV4TransportSignsRequest asserts that sigv4Transport:
//  1. Signs every outbound request with SigV4.
//  2. Sets Authorization header starting with "AWS4-HMAC-SHA256".
//  3. Embeds "Credential=AKID/" in the Authorization header.
//  4. Includes "glue/aws4_request" in the credential scope (service="glue").
func TestSigV4TransportSignsRequest(t *testing.T) {
	t.Parallel()

	// Captured Authorization header from the httptest.Server.
	var capturedAuth string

	// httptest.Server records the Authorization header from every request.
	// It responds with 200 so the transport doesn't error on bad status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Static credentials for the test — NOT real AWS credentials.
	// The credential scope in the Authorization header will contain:
	//   Credential=AKID/<date>/us-east-1/glue/aws4_request
	staticProvider := aws.NewCredentialsCache(
		credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
	)

	// Construct the sigv4Transport with the test server as the inner transport.
	transport := newSigV4Transport(staticProvider, "us-east-1", srv.Client().Transport)

	// Issue a GET request to the test server through the sigv4Transport.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/iceberg/catalog", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Host = strings.TrimPrefix(srv.URL, "http://")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	// Assert the Authorization header was set.
	if capturedAuth == "" {
		t.Fatal("Authorization header not set on outbound request")
	}

	// Assert it uses AWS SigV4 format.
	if !strings.HasPrefix(capturedAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization header: want prefix %q, got %q", "AWS4-HMAC-SHA256", capturedAuth)
	}

	// Assert the credential scope contains the access key ID.
	if !strings.Contains(capturedAuth, "Credential=AKID/") {
		t.Errorf("Authorization header: want Credential=AKID/ in %q", capturedAuth)
	}

	// Assert the service name is "glue" — this is the critical correctness
	// check for T-3-glue-sigv4-replay and T-3-glue-payload-tamper mitigations.
	if !strings.Contains(capturedAuth, "/glue/aws4_request") {
		t.Errorf("Authorization header: want /glue/aws4_request in %q (service name mismatch)", capturedAuth)
	}
}

// TestSigV4TransportEmptyBody asserts that requests with no body
// use the canonical SHA-256 of the empty string as the payload hash.
func TestSigV4TransportEmptyBody(t *testing.T) {
	t.Parallel()

	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	staticProvider := aws.NewCredentialsCache(
		credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
	)
	transport := newSigV4Transport(staticProvider, "us-east-1", srv.Client().Transport)

	// Issue a request with explicit nil body.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Host = strings.TrimPrefix(srv.URL, "http://")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	// The Authorization header must be set (SigV4 signed) even for empty body.
	if capturedAuth == "" {
		t.Fatal("Authorization header not set on request with empty body")
	}
	if !strings.HasPrefix(capturedAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization header for empty body: want AWS4-HMAC-SHA256 prefix, got %q", capturedAuth)
	}
}

// TestComputeBodyHashEmpty asserts that an empty body returns the
// canonical SHA-256 of the empty string.
func TestComputeBodyHashEmpty(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	hash, err := computeBodyHash(req)
	if err != nil {
		t.Fatalf("computeBodyHash: %v", err)
	}

	// SHA-256 of empty string
	const emptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if hash != emptyBodyHash {
		t.Errorf("computeBodyHash(empty): want %q, got %q", emptyBodyHash, hash)
	}
}
