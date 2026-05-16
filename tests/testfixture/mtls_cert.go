package testfixture

// mtls_cert.go — Phase 2 mTLS test material. Production uses ACM PCA
// per Plan 02-08; this fixture is for TESTS ONLY.
//
// Each call to IssueClientCert / IssueExpiredClientCert generates a
// fresh self-signed ECDSA P-256 CA + leaf pair so tests are fully
// isolated (no shared root). The CA private key is held in memory and
// discarded when the test exits — use in production is forbidden and
// would silently widen the trust set.
//
// Typical usage in a Plan 02-05 dispatch D integration test:
//
//	caPEM, srvCertPEM, srvKeyPEM, clientCert :=
//	    testfixture.IssueCAAndServerAndClient(t, "sqlproxy.local", "engine-uri")
//	// write srvCertPEM + srvKeyPEM to NEKSUR_TLS_CERT_PATH/KEY_PATH,
//	// caPEM to NEKSUR_CA_BUNDLE_PATH, then connect with clientCert.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	cryptotls "crypto/tls"
)

// IssueClientCert returns a fresh self-signed CA-backed client cert
// pair for use in mTLS integration tests. Validity: 1 hour from now.
//
// The CA PEM is returned so tests can build a server-side CertPool
// that trusts the issued client cert. Each call generates a fresh CA
// (no shared root) so tests are fully isolated.
//
// Use in production is forbidden — the CA private key is in-memory
// and discarded when the test exits.
func IssueClientCert(t *testing.T, commonName string) (clientCert cryptotls.Certificate, caCertPEM []byte) {
	t.Helper()
	now := time.Now()
	_, caCertPEM, _, clientCert = issueCAAndLeaf(t, commonName,
		now, now.Add(1*time.Hour),
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		nil, nil)
	return clientCert, caCertPEM
}

// IssueExpiredClientCert returns a client cert that expired 1 hour ago.
// Used by TestSQLProxyMTLSHandshake to verify expired-cert rejection
// (the proxy MUST refuse the handshake — RequireAndVerifyClientCert
// + go's x509 default time check enforces this).
func IssueExpiredClientCert(t *testing.T, commonName string) (clientCert cryptotls.Certificate, caCertPEM []byte) {
	t.Helper()
	now := time.Now()
	_, caCertPEM, _, clientCert = issueCAAndLeaf(t, commonName,
		now.Add(-2*time.Hour), now.Add(-1*time.Hour),
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		nil, nil)
	return clientCert, caCertPEM
}

// IssueServerCert returns a server cert + key pair backed by the same
// in-memory CA that produced caCertPEM. The caller supplies the CA's
// private key (returned alongside caCertPEM by issueCAAndLeaf via the
// IssueCAAndServerAndClient helper) so the new server cert chains
// correctly.
//
// Returns the PEM-encoded cert + PEM-encoded private key as byte
// slices, ready to be written to NEKSUR_TLS_CERT_PATH /
// NEKSUR_TLS_KEY_PATH.
func IssueServerCert(t *testing.T, commonName string, caCertPEM []byte, caKey *ecdsa.PrivateKey) (certPEM, keyPEM []byte) {
	t.Helper()
	caCert := parseSingleCert(t, caCertPEM)
	now := time.Now()

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testfixture: IssueServerCert: ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now,
		NotAfter:     now.Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName, "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("testfixture: IssueServerCert: x509.CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("testfixture: IssueServerCert: MarshalECPrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// IssueCAAndServerAndClient is the single-call helper for dispatch D's
// integration tests. It generates a fresh self-signed CA, a server
// cert (with serverCN as SAN), and a client cert (with clientCN as
// CN) — all chained to the same in-memory CA — in one call.
//
// Returned values:
//   - caBundlePEM   — write to NEKSUR_CA_BUNDLE_PATH (the proxy's
//     ClientCAs pool); also load into the client's RootCAs to verify
//     the server cert.
//   - serverCertPEM — write to NEKSUR_TLS_CERT_PATH.
//   - serverKeyPEM  — write to NEKSUR_TLS_KEY_PATH.
//   - clientCert    — assign to tls.Config.Certificates on the test
//     client; this is what the proxy will see during mTLS handshake.
func IssueCAAndServerAndClient(t *testing.T, serverCN, clientCN string) (caBundlePEM, serverCertPEM, serverKeyPEM []byte, clientCert cryptotls.Certificate) {
	t.Helper()
	now := time.Now()
	caKey, caPEM, _, cliCert := issueCAAndLeaf(t, clientCN,
		now, now.Add(1*time.Hour),
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		nil, nil)
	srvCertPEM, srvKeyPEM := IssueServerCert(t, serverCN, caPEM, caKey)
	return caPEM, srvCertPEM, srvKeyPEM, cliCert
}

// issueCAAndLeaf is the shared workhorse. It:
//   - generates a fresh CA keypair + self-signed CA cert,
//   - generates a fresh leaf keypair,
//   - issues a leaf cert with the given validity window + ExtKeyUsage
//     (DNS names + IPs are optional, used for server certs).
//
// Returned values:
//   - caKey         — the CA's private key; held by the caller if it
//     wants to issue additional leaves against the same CA (server
//     cert + client cert sharing one CA).
//   - caCertPEM     — PEM-encoded CA cert.
//   - leafCertPEM   — PEM-encoded leaf cert (unused by some callers,
//     returned for completeness / debugging).
//   - leafTLSCert   — tls.Certificate ready for tls.Config.Certificates.
func issueCAAndLeaf(
	t *testing.T,
	leafCN string,
	notBefore, notAfter time.Time,
	extKeyUsage []x509.ExtKeyUsage,
	dnsNames []string,
	ipAddrs []net.IP,
) (caKey *ecdsa.PrivateKey, caCertPEM, leafCertPEM []byte, leafTLSCert cryptotls.Certificate) {
	t.Helper()

	// --- CA ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: ca ecdsa.GenerateKey: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: "neksur-test-ca"},
		NotBefore:             time.Now().Add(-1 * time.Minute), // small backdate to avoid clock-skew flakes
		NotAfter:              time.Now().Add(2 * time.Hour),    // outlive any test leaf
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: ca x509.CreateCertificate: %v", err)
	}
	caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	// --- Leaf ---
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: leaf ecdsa.GenerateKey: %v", err)
	}
	caCertParsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: parse ca cert: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: leafCN},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  extKeyUsage,
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCertParsed, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: leaf x509.CreateCertificate: %v", err)
	}
	leafCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})

	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: MarshalECPrivateKey: %v", err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})

	leafTLSCert, err = cryptotls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		t.Fatalf("testfixture: issueCAAndLeaf: tls.X509KeyPair: %v", err)
	}
	return caKey, caCertPEM, leafCertPEM, leafTLSCert
}

// parseSingleCert decodes the first CERTIFICATE PEM block in `caPEM`
// and parses it. Test-only helper; panics via t.Fatalf on malformed
// input.
func parseSingleCert(t *testing.T, caPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("testfixture: parseSingleCert: no CERTIFICATE PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("testfixture: parseSingleCert: %v", err)
	}
	return cert
}

// randomSerial returns a fresh 64-bit random serial number for a cert
// template. The collision probability across a single test process is
// vanishingly small; even if it ever did collide, the certs would still
// validate (uniqueness is a directory convention, not an x509 rule).
func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	max := new(big.Int).Lsh(big.NewInt(1), 64)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		t.Fatalf("testfixture: randomSerial: rand.Int: %v", err)
	}
	return n
}
