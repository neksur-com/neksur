// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

//go:build integration

// Package integration_test — license signing integration tests.
//
// These tests execute real ECDSA P-256 sign-verify assertions with ephemeral
// keypairs generated per-test (no shared mutable state). File is owned by Plan 03-02
// per N-1 fix (no upstream stub — real assertions from the start).
//
// Run with: go test -tags=integration -count=1 -run "TestLicense" ./tests/integration/...

package integration_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/license"
	"github.com/neksur-com/neksur/internal/licensegen"
)

// generateIntegrationKeypair creates an ephemeral ECDSA P-256 keypair for integration tests.
// Returns the private key, the corresponding PEM-encoded public key bytes, and the
// path to the written private key PEM file (in t.TempDir()).
func generateIntegrationKeypair(t *testing.T) (*ecdsa.PrivateKey, []byte, string) {
	t.Helper()
	dir := t.TempDir()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA P-256 keypair: %v", err)
	}

	// Write private key (PKCS#8) to temp dir.
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS#8 private key: %v", err)
	}
	privPath := filepath.Join(dir, "test-priv.pem")
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	// Encode public key PEM.
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal PKIX public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	return priv, pubPEM, privPath
}

// setIntegrationPublicKey overrides the embedded public key for the duration of the test.
func setIntegrationPublicKey(t *testing.T, pubPEM []byte) {
	t.Helper()
	license.SetTestPublicKeyPEM(pubPEM)
	t.Cleanup(func() { license.SetTestPublicKeyPEM(nil) })
}

// signManifestDirect builds and signs a Manifest using the private key at privKeyPath,
// bypassing the CLI's future-expiry validation. Used for grace-period sub-tests
// where we need past expiry dates that the CLI would reject.
func signManifestDirect(t *testing.T, priv *ecdsa.PrivateKey, customerID, tenantID, tier string, expiry time.Time, features []string) []byte {
	t.Helper()

	m := &license.Manifest{
		LicenseID:       "direct-" + customerID,
		CustomerID:      customerID,
		TenantID:        tenantID,
		Tier:            tier,
		ExpiryUTC:       expiry.UTC(),
		AllowedFeatures: features,
	}

	canonical, err := license.CanonicalJSON(m)
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

// TestLicenseSignAndVerifyRoundtrip generates an ECDSA keypair on the fly, signs a manifest
// using cmd/license-gen.Run, then verifies with license.Verify. Asserts manifest fields match.
func TestLicenseSignAndVerifyRoundtrip(t *testing.T) {
	priv, pubPEM, privPath := generateIntegrationKeypair(t)
	_ = priv
	setIntegrationPublicKey(t, pubPEM)

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")
	expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	var stdoutBuf, stderrBuf strings.Builder
	code := licensegen.Run([]string{
		"-customer-id=integration-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000020",
		"-tier=enterprise",
		"-expiry-utc=" + expiry,
		"-allowed-features=schema_cache_broadcaster,write_conflict,continuous_verifier",
		"-private-key-path=" + privPath,
		"-out=" + outPath,
	}, &stdoutBuf, &stderrBuf)

	if code != 0 {
		t.Fatalf("license-gen.Run returned %d; stderr: %s", code, stderrBuf.String())
	}

	manifestBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output manifest: %v", err)
	}

	m, err := license.Verify(manifestBytes)
	if err != nil {
		t.Fatalf("license.Verify failed: %v", err)
	}
	if m == nil {
		t.Fatal("license.Verify returned nil manifest on success")
	}

	// Assert manifest fields match inputs.
	if m.CustomerID != "integration-corp" {
		t.Errorf("CustomerID: want integration-corp, got %q", m.CustomerID)
	}
	if m.TenantID != "00000000-0000-0000-0000-000000000020" {
		t.Errorf("TenantID: want 00000000-0000-0000-0000-000000000020, got %q", m.TenantID)
	}
	if m.Tier != "enterprise" {
		t.Errorf("Tier: want enterprise, got %q", m.Tier)
	}
	if len(m.AllowedFeatures) != 3 {
		t.Errorf("AllowedFeatures: want 3, got %d: %v", len(m.AllowedFeatures), m.AllowedFeatures)
	}
}

// TestLicenseTamperRejected signs a valid manifest, flips 1 byte in the customer_id JSON value,
// and asserts that license.Verify returns an error containing "signature verification failed".
func TestLicenseTamperRejected(t *testing.T) {
	priv, pubPEM, privPath := generateIntegrationKeypair(t)
	_ = priv
	setIntegrationPublicKey(t, pubPEM)

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")
	expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	var stdoutBuf, stderrBuf strings.Builder
	code := licensegen.Run([]string{
		"-customer-id=tamper-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000021",
		"-tier=commercial",
		"-expiry-utc=" + expiry,
		"-allowed-features=write_conflict",
		"-private-key-path=" + privPath,
		"-out=" + outPath,
	}, &stdoutBuf, &stderrBuf)

	if code != 0 {
		t.Fatalf("license-gen.Run returned %d; stderr: %s", code, stderrBuf.String())
	}

	manifestBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output manifest: %v", err)
	}

	// Tamper: change "tamper-corp" to "tamper-borg" (1 byte).
	tampered := strings.Replace(string(manifestBytes), "tamper-corp", "tamper-borg", 1)
	if tampered == string(manifestBytes) {
		t.Fatal("tamper substitution did not change anything — test setup error")
	}

	_, verifyErr := license.Verify([]byte(tampered))
	if verifyErr == nil {
		t.Fatal("expected Verify to reject tampered manifest, got nil error")
	}
	if !strings.Contains(verifyErr.Error(), "signature verification failed") {
		t.Fatalf("expected 'signature verification failed' in error, got: %v", verifyErr)
	}
}

// TestLicenseExpiryGracePeriod covers three expiry sub-cases:
// (a) expired 8 days ago → Verify returns error containing "expired beyond grace"
// (b) expired 1 day ago → Verify returns nil error; GracePeriodRemaining returns positive duration ≤ 6d
// (c) expires in 30 days → Verify returns nil error; GracePeriodRemaining returns 0
func TestLicenseExpiryGracePeriod(t *testing.T) {
	t.Run("beyond_grace", func(t *testing.T) {
		priv, pubPEM, _ := generateIntegrationKeypair(t)
		setIntegrationPublicKey(t, pubPEM)

		// Expired 8 days ago — beyond 7-day grace.
		// Sign directly (bypassing CLI future-expiry validation).
		manifestBytes := signManifestDirect(t, priv, "grace-a-corp",
			"00000000-0000-0000-0000-000000000022",
			"commercial", time.Now().UTC().Add(-8*24*time.Hour),
			[]string{"schema_cache_broadcaster"})

		result, verifyErr := license.Verify(manifestBytes)
		if verifyErr == nil {
			t.Fatal("expected error for manifest expired beyond grace, got nil")
		}
		if !strings.Contains(verifyErr.Error(), "expired beyond grace") {
			t.Fatalf("expected 'expired beyond grace' in error, got: %v", verifyErr)
		}
		if result == nil {
			t.Fatal("expected non-nil manifest even on expiry-beyond-grace error")
		}
	})

	t.Run("within_grace", func(t *testing.T) {
		priv, pubPEM, _ := generateIntegrationKeypair(t)
		setIntegrationPublicKey(t, pubPEM)

		// Expired 1 day ago — within 7-day grace.
		expiryTime := time.Now().UTC().Add(-1 * 24 * time.Hour)
		manifestBytes := signManifestDirect(t, priv, "grace-b-corp",
			"00000000-0000-0000-0000-000000000023",
			"commercial", expiryTime,
			[]string{"write_conflict"})

		result, verifyErr := license.Verify(manifestBytes)
		if verifyErr != nil {
			t.Fatalf("expected nil error for manifest within grace period, got: %v", verifyErr)
		}
		if result == nil {
			t.Fatal("expected non-nil manifest within grace period")
		}

		// GracePeriodRemaining should be positive and ≤ 6 days.
		remaining := license.GracePeriodRemaining(result)
		if remaining <= 0 {
			t.Errorf("GracePeriodRemaining: expected positive duration, got %v", remaining)
		}
		maxRemaining := 6 * 24 * time.Hour
		if remaining > maxRemaining {
			t.Errorf("GracePeriodRemaining: expected ≤ 6d, got %v", remaining)
		}
	})

	t.Run("not_yet_expired", func(t *testing.T) {
		priv, pubPEM, privPath := generateIntegrationKeypair(t)
		_ = priv
		setIntegrationPublicKey(t, pubPEM)

		outDir := t.TempDir()
		outPath := filepath.Join(outDir, "license.json")
		expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

		var stdoutBuf, stderrBuf strings.Builder
		code := licensegen.Run([]string{
			"-customer-id=grace-c-corp",
			"-tenant-id=00000000-0000-0000-0000-000000000024",
			"-tier=enterprise",
			"-expiry-utc=" + expiry,
			"-allowed-features=continuous_verifier",
			"-private-key-path=" + privPath,
			"-out=" + outPath,
		}, &stdoutBuf, &stderrBuf)
		if code != 0 {
			t.Fatalf("license-gen.Run returned %d; stderr: %s", code, stderrBuf.String())
		}

		manifestBytes, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}

		result, verifyErr := license.Verify(manifestBytes)
		if verifyErr != nil {
			t.Fatalf("expected nil error for future manifest, got: %v", verifyErr)
		}
		if result == nil {
			t.Fatal("expected non-nil manifest for future license")
		}

		// GracePeriodRemaining should be 0 for a non-expired license.
		remaining := license.GracePeriodRemaining(result)
		if remaining != 0 {
			t.Errorf("GracePeriodRemaining for non-expired license: expected 0, got %v", remaining)
		}
	})
}
