// TLS configuration for the sqlproxy listener — RESEARCH §Pattern 7
// + D-2.08 §"internal mTLS". The proxy lives behind the internal
// service mesh boundary; every connecting engine presents a client
// certificate issued by the Phase 0.5 Private CA and validated against
// the operator-supplied CA bundle.
//
// Hard requirements (do NOT relax without an ADR amendment):
//
//   - MinVersion = TLS 1.3 — no TLS 1.2 fallback. The proxy carries
//     CompiledPolicy artifacts (column masks, row filters); TLS 1.2
//     CBC cipher suites have known padding-oracle weaknesses that
//     are unacceptable on the policy-decision wire.
//   - ClientAuth = RequireAndVerifyClientCert — the proxy NEVER
//     accepts an anonymous TLS handshake. Misconfiguration that
//     would silently downgrade to optional client auth is the
//     #1 D-2.08 risk.
//   - GetCertificate sourced from a CertWatcher so the server cert
//     can be rotated without restart (fsnotify hot-reload).

package sqlproxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// NewTLSConfig assembles the *tls.Config the sqlproxy server passes
// to (*http.Server).TLSConfig. caBundlePath is the path to the PEM
// file containing one or more CA certificates — the proxy verifies
// every client cert against this pool (RequireAndVerifyClientCert).
//
// Returns an error if the CA bundle cannot be read or contains no
// usable certificates. Callers MUST treat the error as fatal: a
// proxy that started without a CA bundle would silently downgrade
// to anonymous TLS, which violates D-2.08.
func NewTLSConfig(certWatcher *CertWatcher, caBundlePath string) (*tls.Config, error) {
	if certWatcher == nil {
		return nil, fmt.Errorf("sqlproxy: NewTLSConfig: certWatcher is required")
	}
	if caBundlePath == "" {
		return nil, fmt.Errorf("sqlproxy: NewTLSConfig: caBundlePath is required")
	}
	pem, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("sqlproxy: NewTLSConfig: read CA bundle %q: %w", caBundlePath, err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(pem); !ok {
		return nil, fmt.Errorf("sqlproxy: NewTLSConfig: CA bundle %q contained no usable certificates", caBundlePath)
	}
	return &tls.Config{
		// TLS 1.3 minimum — see package-doc requirements.
		MinVersion: tls.VersionTLS13,
		// Require a verified client cert on every handshake (mTLS).
		ClientAuth: tls.RequireAndVerifyClientCert,
		// Verify client certs against the operator-supplied bundle.
		ClientCAs: pool,
		// Server cert sourced from the hot-reloading CertWatcher so
		// rotation does not require a process restart.
		GetCertificate: certWatcher.GetCertificate,
	}, nil
}
