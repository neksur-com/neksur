// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package license

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWatcher wraps LicenseWatcher with an injectable tick interval so unit
// tests don't have to wait 5 minutes for the periodic backstop.
type testWatcher struct {
	path     string
	interval time.Duration
}

// Watch calls watchWithInterval with the injected short tick.
func (tw *testWatcher) Watch(ctx context.Context) error {
	return watchWithInterval(ctx, tw.path, tw.interval)
}

// signWatcherManifest creates and signs a manifest for watcher tests.
// Uses generateTestKeypair from verifier_test.go (same package).
func signWatcherManifest(t *testing.T, priv *ecdsa.PrivateKey, tier string, features []string, expiry time.Time) []byte {
	t.Helper()
	m := &Manifest{
		LicenseID:       "test-lic-001",
		CustomerID:      "cust-001",
		TenantID:        "tenant-001",
		Tier:            tier,
		ExpiryUTC:       expiry,
		AllowedFeatures: features,
	}
	canonical, err := canonicalJSON(m)
	require.NoError(t, err)
	digest := sha256.Sum256(canonical)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	require.NoError(t, err)
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	bs, err := json.Marshal(m)
	require.NoError(t, err)
	return bs
}

// TestWatcher_DetectsFileChange writes a manifest to the license file and
// confirms the watcher picks it up via the periodic-backstop path.
func TestWatcher_DetectsFileChange(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	SetTestPublicKeyPEM(pubPEM)
	t.Cleanup(func() { SetTestPublicKeyPEM(nil) })

	dir := t.TempDir()
	licensePath := filepath.Join(dir, "license.json")
	expiry := time.Now().UTC().Add(24 * time.Hour)
	manifestBytes := signWatcherManifest(t, priv, "commercial", []string{"write_conflict"}, expiry)
	require.NoError(t, os.WriteFile(licensePath, manifestBytes, 0o600))

	// Clear any previous manifest.
	SetManifest(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tw := &testWatcher{path: licensePath, interval: 30 * time.Millisecond}
	go func() { _ = tw.Watch(ctx) }()

	// Wait for the watcher to pick up the manifest via periodic backstop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m := current.Load()
		if m != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	m := current.Load()
	require.NotNil(t, m, "watcher should have loaded the manifest via periodic backstop")
	assert.Equal(t, "commercial", m.Tier)
	assert.Equal(t, "test-lic-001", m.LicenseID)
}

// TestWatcher_GracefulOnParseError writes garbage to the license file and
// confirms the previous manifest is retained (graceful degradation).
func TestWatcher_GracefulOnParseError(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	SetTestPublicKeyPEM(pubPEM)
	t.Cleanup(func() { SetTestPublicKeyPEM(nil) })

	dir := t.TempDir()
	licensePath := filepath.Join(dir, "license.json")
	expiry := time.Now().UTC().Add(24 * time.Hour)
	manifestBytes := signWatcherManifest(t, priv, "commercial", []string{"write_conflict"}, expiry)
	require.NoError(t, os.WriteFile(licensePath, manifestBytes, 0o600))

	// Load the initial manifest (simulate boot-time load).
	initialManifest, err := Verify(manifestBytes)
	require.NoError(t, err)
	SetManifest(initialManifest)

	// Overwrite with garbage — the watcher should retain the previous manifest.
	require.NoError(t, os.WriteFile(licensePath, []byte("not-valid-json"), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tw := &testWatcher{path: licensePath, interval: 30 * time.Millisecond}
	go func() { _ = tw.Watch(ctx) }()

	// Give the watcher time to react to the bad file via periodic tick.
	time.Sleep(300 * time.Millisecond)

	m := current.Load()
	require.NotNil(t, m, "manifest should be retained after bad file write")
	assert.Equal(t, "test-lic-001", m.LicenseID,
		"watcher should retain previous manifest on parse/verify error")
}

// TestWatcher_PeriodicBackstop confirms the periodic tick reloads the manifest
// even when no fsnotify event fires (e.g., event dropped by OS).
// Injects a very short tick interval so the test completes quickly.
func TestWatcher_PeriodicBackstop(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	SetTestPublicKeyPEM(pubPEM)
	t.Cleanup(func() { SetTestPublicKeyPEM(nil) })

	dir := t.TempDir()
	licensePath := filepath.Join(dir, "license.json")
	expiry := time.Now().UTC().Add(24 * time.Hour)
	manifestBytes := signWatcherManifest(t, priv, "enterprise", []string{"compaction_coordination"}, expiry)
	require.NoError(t, os.WriteFile(licensePath, manifestBytes, 0o600))

	// Start with nil — periodic reload must pick it up without any explicit event.
	SetManifest(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 30ms tick so the backstop fires quickly.
	tw := &testWatcher{path: licensePath, interval: 30 * time.Millisecond}
	go func() { _ = tw.Watch(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m := current.Load()
		if m != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	m := current.Load()
	require.NotNil(t, m, "periodic backstop should have loaded the manifest")
	assert.Equal(t, "enterprise", m.Tier)
	assert.Contains(t, m.AllowedFeatures, "compaction_coordination")
}
