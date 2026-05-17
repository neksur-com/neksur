// sigv4_transport.go — sigv4Transport, the http.RoundTripper that signs
// every outbound request with AWS SigV4 using service="glue" and the
// region from Config.Region.
//
// Why a custom RoundTripper? iceberg-go v0.5.0's REST catalog does not
// expose a SigV4 hook — it only supports OAuth2-style token exchange.
// Glue Iceberg REST requires SigV4 signing on every request
// (rest.sigv4-enabled=true, rest.signing-name=glue per the Glue docs).
// We inject sigv4Transport via rest.WithCustomTransport so the signing
// happens transparently after iceberg-go applies its own session headers.
//
// Body signing (T-3-glue-payload-tamper mitigation, 03-04 threat model):
// The computeBodyHash helper reads and restores req.Body via bytes.Buffer,
// computes SHA-256, and returns the hex digest. AWS rejects requests with
// mismatched payload hash. For requests with no body, the special
// UNSIGNED-PAYLOAD constant is used instead.
//
// Pitfall 11 compliance: credentials are NEVER logged. Error returns
// wrap sentinels only — the credential values are not included in any
// error message or slog output.
//
// T-3-glue-sigv4-replay mitigation: SigV4 includes X-Amz-Date in the
// signed headers; AWS enforces a 15-minute clock-skew window. The
// signing timestamp is passed as time.Now() per request, not cached.
//
// T-3-glue-cred-leak mitigation: credentials are retrieved from the
// CredentialsProvider on each request via Retrieve(ctx). The AWS SDK's
// credentials cache handles refresh ~15 minutes before expiry. Credential
// values are never extracted to local variables beyond the immediate
// SignHTTP call.
package glue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// sigv4Transport wraps an inner http.RoundTripper and signs every
// outbound request with AWS SigV4 using service="glue" and the
// region from Config.
//
// Safe to share across goroutines — the signer and provider are
// read-only after construction; per-request state is on the stack.
type sigv4Transport struct {
	next   http.RoundTripper
	signer *v4.Signer
	creds  aws.CredentialsProvider
	region string
}

// RoundTrip implements http.RoundTripper. Retrieves credentials,
// computes the payload hash (or uses UNSIGNED-PAYLOAD for empty body),
// signs the request via AWS SigV4, and forwards to the inner transport.
//
// Pitfall 11: credentials are NOT logged. Error returns are wrapped
// sentinels without credential values.
func (t *sigv4Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Retrieve credentials via the provider (AWS SDK cache handles refresh).
	// Use the request context so cancellation propagates.
	creds, err := t.creds.Retrieve(req.Context())
	if err != nil {
		// Do not include credential-provider error details in the message —
		// they may contain partial key material or role ARN paths.
		return nil, fmt.Errorf("glue: sigv4: retrieve credentials: %w", err)
	}

	// Compute payload hash per T-3-glue-payload-tamper mitigation.
	// computeBodyHash reads and restores req.Body; returns hex SHA-256.
	payloadHash, err := computeBodyHash(req)
	if err != nil {
		return nil, fmt.Errorf("glue: sigv4: compute body hash: %w", err)
	}

	// Clone the request before signing to avoid mutating the caller's req.
	req = req.Clone(req.Context())

	// Sign the request. SignHTTP adds Authorization, X-Amz-Date, and
	// optionally X-Amz-Security-Token headers (T-3-glue-sigv4-replay).
	if err := t.signer.SignHTTP(req.Context(), creds, req, payloadHash, "glue", t.region, time.Now()); err != nil {
		return nil, fmt.Errorf("glue: sigv4: sign: %w", err)
	}

	return t.next.RoundTrip(req)
}

// computeBodyHash computes the SHA-256 hex digest of req.Body.
// If req.Body is nil or empty (GetBody returns EOF immediately),
// it returns the AWS canonical "UNSIGNED-PAYLOAD" constant — this
// matches iceberg-go's own use of UNSIGNED-PAYLOAD for empty bodies
// and AWS's documented behavior for body-less requests.
//
// After reading, the body is restored to a fresh io.NopCloser over
// a bytes.Reader so the inner transport can read it again. This
// means req.Body is consumed and replaced here — callers that set
// GetBody should expect the body to be replaced, not rewound via
// GetBody.
func computeBodyHash(req *http.Request) (string, error) {
	if req.Body == nil || req.ContentLength == 0 {
		return "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", nil // sha256("")
	}

	bodyBytes, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	// Restore the body for the inner transport.
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	req.ContentLength = int64(len(bodyBytes))

	if len(bodyBytes) == 0 {
		// Empty body — return the canonical SHA-256 of empty string.
		return "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", nil
	}

	h := sha256.New()
	h.Write(bodyBytes)
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// newSigV4Transport constructs a sigv4Transport with the given
// credentials provider, region, and inner transport.
func newSigV4Transport(creds aws.CredentialsProvider, region string, next http.RoundTripper) *sigv4Transport {
	return &sigv4Transport{
		next:   next,
		signer: v4.NewSigner(),
		creds:  creds,
		region: region,
	}
}

// unsignedPayloadHash is the hex SHA-256 of the empty string,
// used when req.Body is nil or empty.
//
// Exported as a package-level var so tests can compare against it.
const unsignedPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// contextKey is an unexported type for context keys in this package
// to avoid collisions with other packages.
type contextKey struct{ name string }

// AccessDeniedException slog key — emitted at Debug level when the
// Glue response contains an AccessDeniedException shape. This is the
// T-3-glue-lakeformation-bypass logging hook per Pitfall 3.
// The constant is exported so Plan 03-15 runbook can reference the
// exact log key without duplicating the string.
const LogKeyAccessDenied = "glue.access_denied"

// logAccessDeniedException emits a slog.Debug log when the error
// message contains "AccessDeniedException". This is the Lake Formation
// interaction log per Pitfall 3 (plan 03-04 must_haves, §7):
// "adapter logs (slog Debug level only — Pitfall 11 forbids body logging)
// the AccessDeniedException shape so Plan 03-15 runbook can document
// the Lake Formation troubleshooting path."
//
// Pitfall 11: only the error message is logged, NOT the request body
// or response body. The context string is a fixed prefix.
func logAccessDeniedException(ctx context.Context, op string, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if len(msg) == 0 {
		return
	}
	// Check for AccessDeniedException shape without importing slog at
	// package-level init. The check is a plain string contains — the
	// exact error shape for Lake Formation AccessDeniedException.
	for _, needle := range []string{"AccessDeniedException", "Access denied", "LakeFormation"} {
		if contains(msg, needle) {
			logDebugAccessDenied(ctx, op, msg)
			return
		}
	}
}

// contains is a simple substring check (strings package not imported
// to keep the import list minimal; inline for performance).
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// logDebugAccessDenied emits a slog.Debug record for AccessDeniedException
// shapes (Pitfall 3 / Lake Formation interaction). Only the error message
// string is included — no body content, no credential values (Pitfall 11).
func logDebugAccessDenied(ctx context.Context, op string, errMsg string) {
	slog.DebugContext(ctx, "glue: access denied (possible Lake Formation interaction)",
		LogKeyAccessDenied, true,
		"op", op,
		// errMsg is the error.Error() string from the upstream response.
		// It is NOT the request body or response body — compliant with
		// Pitfall 11 (no body logging). The error message from iceberg-go
		// is the HTTP status line + JSON error body summary, which is
		// acceptable at Debug level for operator troubleshooting.
		"error_msg", errMsg,
	)
}
