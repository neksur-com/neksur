// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package license

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// generateTestKeypair creates a fresh ECDSA P-256 keypair for testing.
// Returns (privKey, pubKeyPEM).
func generateTestKeypair(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return priv, pubPEM
}

// signManifest signs a Manifest with the given private key and returns the manifest
// with the Signature field set.
func signManifest(t *testing.T, m *Manifest, priv *ecdsa.PrivateKey) []byte {
	t.Helper()
	canonical, err := canonicalJSON(m)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	digest := sha256.Sum256(canonical)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	bs, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal signed manifest: %v", err)
	}
	return bs
}

// TestVerifySignRoundtrip verifies that a manifest signed with a test key is correctly
// verified when the embedded public key is overridden with the matching test public key.
func TestVerifySignRoundtrip(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)

	m := &Manifest{
		LicenseID:       "lic-round-001",
		CustomerID:      "roundtrip-corp",
		TenantID:        "00000000-0000-0000-0000-000000000003",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AllowedFeatures: []string{"schema_cache_broadcaster"},
	}
	manifestBytes := signManifest(t, m, priv)

	// Override the embedded public key PEM for this test.
	orig := publicKeyPEM
	publicKeyPEM = pubPEM
	defer func() { publicKeyPEM = orig }()

	result, err := Verify(manifestBytes)
	if err != nil {
		t.Fatalf("Verify returned error for valid manifest: %v", err)
	}
	if result == nil {
		t.Fatal("Verify returned nil manifest on success")
	}
	if result.CustomerID != m.CustomerID {
		t.Fatalf("CustomerID mismatch: want %q got %q", m.CustomerID, result.CustomerID)
	}
}

// TestVerifyTamperRejected verifies that mutating one byte in a signed manifest's customer_id
// causes Verify to return an error containing "signature verification failed".
func TestVerifyTamperRejected(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)

	m := &Manifest{
		LicenseID:       "lic-tamper-001",
		CustomerID:      "legit-corp",
		TenantID:        "00000000-0000-0000-0000-000000000004",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AllowedFeatures: []string{"write_conflict"},
	}
	manifestBytes := signManifest(t, m, priv)

	// Tamper: replace "legit-corp" with "legit-borg" (one byte change).
	tampered := strings.Replace(string(manifestBytes), "legit-corp", "legit-borg", 1)

	orig := publicKeyPEM
	publicKeyPEM = pubPEM
	defer func() { publicKeyPEM = orig }()

	_, err := Verify([]byte(tampered))
	if err == nil {
		t.Fatal("expected error for tampered manifest, got nil")
	}
	if !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("expected 'signature verification failed' in error, got: %v", err)
	}
}

// TestVerifyMalformedPubKey verifies that a garbage embedded public key causes an error
// containing "embedded key" or "parse public key".
func TestVerifyMalformedPubKey(t *testing.T) {
	priv, _ := generateTestKeypair(t)

	m := &Manifest{
		LicenseID:       "lic-badkey-001",
		CustomerID:      "badkey-corp",
		TenantID:        "00000000-0000-0000-0000-000000000005",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AllowedFeatures: []string{},
	}
	manifestBytes := signManifest(t, m, priv)

	// Replace embedded pubkey with garbage.
	orig := publicKeyPEM
	publicKeyPEM = []byte("not-a-valid-pem")
	defer func() { publicKeyPEM = orig }()

	_, err := Verify(manifestBytes)
	if err == nil {
		t.Fatal("expected error for malformed pubkey, got nil")
	}
	if !strings.Contains(err.Error(), "embedded key") && !strings.Contains(err.Error(), "parse public key") {
		t.Fatalf("expected 'embedded key' or 'parse public key' in error, got: %v", err)
	}
}

// TestVerifyExpiredBeyondGraceRejected verifies that a license expired more than 7 days ago
// returns an error containing "expired beyond grace".
func TestVerifyExpiredBeyondGraceRejected(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)

	// Expired 8 days ago.
	m := &Manifest{
		LicenseID:       "lic-expired-001",
		CustomerID:      "expired-corp",
		TenantID:        "00000000-0000-0000-0000-000000000006",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(-8 * 24 * time.Hour),
		AllowedFeatures: []string{"schema_cache_broadcaster"},
	}
	manifestBytes := signManifest(t, m, priv)

	orig := publicKeyPEM
	publicKeyPEM = pubPEM
	defer func() { publicKeyPEM = orig }()

	result, err := Verify(manifestBytes)
	if err == nil {
		t.Fatal("expected error for license expired beyond grace, got nil")
	}
	if !strings.Contains(err.Error(), "expired beyond grace") {
		t.Fatalf("expected 'expired beyond grace' in error, got: %v", err)
	}
	// Per design: Verify still returns the manifest even on expiry error.
	if result == nil {
		t.Fatal("expected non-nil manifest even on expiry error")
	}
}

// TestVerifyExpiredWithinGraceAccepted verifies that a license expired 1 day ago (within 7-day grace)
// returns (manifest, nil).
func TestVerifyExpiredWithinGraceAccepted(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)

	// Expired 1 day ago — within grace.
	m := &Manifest{
		LicenseID:       "lic-grace-001",
		CustomerID:      "grace-corp",
		TenantID:        "00000000-0000-0000-0000-000000000007",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(-1 * 24 * time.Hour),
		AllowedFeatures: []string{"write_conflict"},
	}
	manifestBytes := signManifest(t, m, priv)

	orig := publicKeyPEM
	publicKeyPEM = pubPEM
	defer func() { publicKeyPEM = orig }()

	result, err := Verify(manifestBytes)
	if err != nil {
		t.Fatalf("expected nil error for license within grace period, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil manifest within grace period")
	}
}

// TestVerifyNotYetExpired verifies that a license expiring in the future returns (manifest, nil).
func TestVerifyNotYetExpired(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)

	m := &Manifest{
		LicenseID:       "lic-future-001",
		CustomerID:      "future-corp",
		TenantID:        "00000000-0000-0000-0000-000000000008",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AllowedFeatures: []string{"continuous_verifier"},
	}
	manifestBytes := signManifest(t, m, priv)

	orig := publicKeyPEM
	publicKeyPEM = pubPEM
	defer func() { publicKeyPEM = orig }()

	result, err := Verify(manifestBytes)
	if err != nil {
		t.Fatalf("expected nil error for future license, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil manifest for future license")
	}
}
