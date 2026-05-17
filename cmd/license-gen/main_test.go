// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/license"
)

// setupTestKeypair generates an ephemeral ECDSA P-256 keypair in a temp directory
// and returns the path to the private key PEM file. The public key is embedded in
// the license package (not swapped out here — the CLI generates a manifest, and
// the roundtrip test verifies using the CLI's private key + the test-overridden pubkey).
func setupTestKeypair(t *testing.T) (privKeyPath string, priv *ecdsa.PrivateKey) {
	t.Helper()
	dir := t.TempDir()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}

	// Write private key (PKCS8).
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	privKeyPath = filepath.Join(dir, "test-priv.pem")
	if err := os.WriteFile(privKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	return privKeyPath, priv
}

// overridePubKey replaces the license package's embedded public key with the given
// ECDSA public key for the duration of the test. Restored via t.Cleanup.
func overridePubKey(t *testing.T, pub *ecdsa.PublicKey) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	license.SetTestPublicKeyPEM(pubPEM)
	t.Cleanup(func() { license.SetTestPublicKeyPEM(nil) })
}

// TestGenSignedManifestRoundtrip verifies that the CLI produces a valid signed manifest
// that passes license.Verify.
func TestGenSignedManifestRoundtrip(t *testing.T) {
	privKeyPath, priv := setupTestKeypair(t)
	overridePubKey(t, &priv.PublicKey)

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")

	expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"-customer-id=acme-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000001",
		"-tier=commercial",
		"-expiry-utc=" + expiry,
		"-allowed-features=schema_cache_broadcaster,write_conflict",
		"-private-key-path=" + privKeyPath,
		"-out=" + outPath,
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run returned code %d; stderr: %s", code, stderr.String())
	}

	manifestBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}

	result, err := license.Verify(manifestBytes)
	if err != nil {
		t.Fatalf("license.Verify failed: %v", err)
	}
	if result == nil {
		t.Fatal("license.Verify returned nil manifest")
	}
	if result.CustomerID != "acme-corp" {
		t.Errorf("CustomerID: want acme-corp, got %s", result.CustomerID)
	}
	if result.Tier != "commercial" {
		t.Errorf("Tier: want commercial, got %s", result.Tier)
	}
	if len(result.AllowedFeatures) != 2 {
		t.Errorf("AllowedFeatures: want 2, got %d", len(result.AllowedFeatures))
	}
}

// TestGenRejectsInvalidTier verifies that an invalid tier causes exit code != 0
// and stderr contains "tier must be one of".
func TestGenRejectsInvalidTier(t *testing.T) {
	privKeyPath, _ := setupTestKeypair(t)
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")
	expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"-customer-id=acme-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000001",
		"-tier=premium",
		"-expiry-utc=" + expiry,
		"-allowed-features=f1",
		"-private-key-path=" + privKeyPath,
		"-out=" + outPath,
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code for invalid tier, got 0")
	}
	if !strings.Contains(stderr.String(), "tier must be one of") {
		t.Fatalf("expected 'tier must be one of' in stderr, got: %s", stderr.String())
	}
}

// TestGenRejectsExpiryInPast verifies that a past expiry causes exit code != 0
// and stderr contains "expiry must be in future".
func TestGenRejectsExpiryInPast(t *testing.T) {
	privKeyPath, _ := setupTestKeypair(t)
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")

	pastExpiry := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"-customer-id=acme-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000001",
		"-tier=commercial",
		"-expiry-utc=" + pastExpiry,
		"-allowed-features=f1",
		"-private-key-path=" + privKeyPath,
		"-out=" + outPath,
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code for past expiry, got 0")
	}
	if !strings.Contains(stderr.String(), "expiry must be in future") {
		t.Fatalf("expected 'expiry must be in future' in stderr, got: %s", stderr.String())
	}
}

// TestGenRejectsMissingPrivateKey verifies that a nonexistent private key path
// causes exit code != 0.
func TestGenRejectsMissingPrivateKey(t *testing.T) {
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")
	expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"-customer-id=acme-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000001",
		"-tier=commercial",
		"-expiry-utc=" + expiry,
		"-allowed-features=f1",
		"-private-key-path=/nonexistent/path/to/key.pem",
		"-out=" + outPath,
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code for missing private key, got 0")
	}
}

// TestGenRejectsMalformedAllowedFeatures verifies that empty feature entries cause exit code != 0.
func TestGenRejectsMalformedAllowedFeatures(t *testing.T) {
	privKeyPath, _ := setupTestKeypair(t)
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "license.json")
	expiry := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"-customer-id=acme-corp",
		"-tenant-id=00000000-0000-0000-0000-000000000001",
		"-tier=commercial",
		"-expiry-utc=" + expiry,
		"-allowed-features= ,, ",
		"-private-key-path=" + privKeyPath,
		"-out=" + outPath,
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code for malformed allowed-features, got 0")
	}
}
