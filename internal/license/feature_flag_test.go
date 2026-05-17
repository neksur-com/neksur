// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package license

import (
	"testing"
	"time"
)

// TestIsFeatureAllowedTrue verifies that a cached manifest containing the requested feature
// and not expired returns true.
func TestIsFeatureAllowedTrue(t *testing.T) {
	m := &Manifest{
		LicenseID:       "lic-flag-001",
		CustomerID:      "allowed-corp",
		TenantID:        "00000000-0000-0000-0000-000000000010",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AllowedFeatures: []string{"schema_cache_broadcaster", "write_conflict"},
	}
	SetManifest(m)
	defer SetManifest(nil)

	if !IsFeatureAllowed("schema_cache_broadcaster") {
		t.Error("expected IsFeatureAllowed to return true for 'schema_cache_broadcaster'")
	}
	if !IsFeatureAllowed("write_conflict") {
		t.Error("expected IsFeatureAllowed to return true for 'write_conflict'")
	}
}

// TestIsFeatureAllowedFalse verifies that a cached manifest without the requested feature
// returns false.
func TestIsFeatureAllowedFalse(t *testing.T) {
	m := &Manifest{
		LicenseID:       "lic-flag-002",
		CustomerID:      "restricted-corp",
		TenantID:        "00000000-0000-0000-0000-000000000011",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AllowedFeatures: []string{"schema_cache_broadcaster"},
	}
	SetManifest(m)
	defer SetManifest(nil)

	if IsFeatureAllowed("write_conflict") {
		t.Error("expected IsFeatureAllowed to return false for 'write_conflict' (not in manifest)")
	}
	if IsFeatureAllowed("continuous_verifier") {
		t.Error("expected IsFeatureAllowed to return false for 'continuous_verifier' (not in manifest)")
	}
}

// TestIsFeatureAllowedNoManifest verifies that with no manifest loaded, IsFeatureAllowed
// returns false (fail-closed per Pitfall 4).
func TestIsFeatureAllowedNoManifest(t *testing.T) {
	SetManifest(nil)
	defer SetManifest(nil)

	if IsFeatureAllowed("schema_cache_broadcaster") {
		t.Error("expected IsFeatureAllowed to return false (fail-closed) when no manifest loaded")
	}
}

// TestIsFeatureAllowedExpired verifies that a cached manifest expired beyond the grace period
// causes IsFeatureAllowed to return false, even if the feature is listed.
// Per Pitfall 4: every IsFeatureAllowed call re-checks expiry.
func TestIsFeatureAllowedExpired(t *testing.T) {
	m := &Manifest{
		LicenseID:       "lic-flag-003",
		CustomerID:      "expired-flag-corp",
		TenantID:        "00000000-0000-0000-0000-000000000012",
		Tier:            "commercial",
		ExpiryUTC:       time.Now().UTC().Add(-8 * 24 * time.Hour), // 8 days ago — beyond grace
		AllowedFeatures: []string{"schema_cache_broadcaster", "write_conflict"},
	}
	SetManifest(m)
	defer SetManifest(nil)

	if IsFeatureAllowed("schema_cache_broadcaster") {
		t.Error("expected IsFeatureAllowed to return false for expired manifest (beyond grace)")
	}
}
