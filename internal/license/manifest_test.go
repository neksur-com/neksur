// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package license

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestManifestCanonicalJSONDeterministic verifies that the same Manifest with shuffled
// AllowedFeatures produces byte-equal canonical output.
func TestManifestCanonicalJSONDeterministic(t *testing.T) {
	expiry := time.Date(2027, 5, 17, 0, 0, 0, 0, time.UTC)

	m1 := &Manifest{
		LicenseID:       "lic-001",
		CustomerID:      "acme-corp",
		TenantID:        "00000000-0000-0000-0000-000000000001",
		Tier:            "commercial",
		ExpiryUTC:       expiry,
		AllowedFeatures: []string{"write_conflict", "schema_cache_broadcaster", "continuous_verifier"},
		Signature:       "sig-abc",
	}

	// Same manifest with AllowedFeatures in a different order.
	m2 := &Manifest{
		LicenseID:       "lic-001",
		CustomerID:      "acme-corp",
		TenantID:        "00000000-0000-0000-0000-000000000001",
		Tier:            "commercial",
		ExpiryUTC:       expiry,
		AllowedFeatures: []string{"continuous_verifier", "write_conflict", "schema_cache_broadcaster"},
		Signature:       "sig-abc",
	}

	c1, err := canonicalJSON(m1)
	if err != nil {
		t.Fatalf("canonicalJSON m1: %v", err)
	}
	c2, err := canonicalJSON(m2)
	if err != nil {
		t.Fatalf("canonicalJSON m2: %v", err)
	}

	if !bytes.Equal(c1, c2) {
		t.Fatalf("canonical outputs differ for same manifest with shuffled AllowedFeatures:\nm1: %s\nm2: %s", c1, c2)
	}

	// Verify the canonical form is valid JSON.
	var m map[string]interface{}
	if err := json.Unmarshal(c1, &m); err != nil {
		t.Fatalf("canonical output is not valid JSON: %v", err)
	}
}

// TestManifestCanonicalJSONIgnoresSignature verifies that canonical form excludes the Signature field,
// so manifests with different Signature values produce the same canonical bytes.
func TestManifestCanonicalJSONIgnoresSignature(t *testing.T) {
	expiry := time.Date(2027, 5, 17, 0, 0, 0, 0, time.UTC)

	base := Manifest{
		LicenseID:       "lic-002",
		CustomerID:      "beta-co",
		TenantID:        "00000000-0000-0000-0000-000000000002",
		Tier:            "enterprise",
		ExpiryUTC:       expiry,
		AllowedFeatures: []string{"schema_cache_broadcaster"},
	}

	mA := base
	mA.Signature = "AAAA"
	mB := base
	mB.Signature = "BBBB"

	cA, err := canonicalJSON(&mA)
	if err != nil {
		t.Fatalf("canonicalJSON mA: %v", err)
	}
	cB, err := canonicalJSON(&mB)
	if err != nil {
		t.Fatalf("canonicalJSON mB: %v", err)
	}

	if !bytes.Equal(cA, cB) {
		t.Fatalf("canonical outputs differ when only Signature differs:\nmA: %s\nmB: %s", cA, cB)
	}

	// Ensure "signature" key is absent from canonical form.
	var decoded map[string]interface{}
	if err := json.Unmarshal(cA, &decoded); err != nil {
		t.Fatalf("canonical output is not valid JSON: %v", err)
	}
	if _, found := decoded["signature"]; found {
		t.Fatal("canonical JSON must not contain 'signature' key")
	}
}
