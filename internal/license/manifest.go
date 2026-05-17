// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// Package license implements ECDSA P-256 license manifest signing and verification
// for the Neksur multi-engine governance plane.
//
// Design decision D-3.04: ECDSA P-256 chosen over Ed25519 for audit-team familiarity.
// Public key embedded at compile time via //go:embed (see pubkey.go).
// Private key is never in this repo; the signing ceremony is documented in Plan 03-15.
package license

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Manifest is the signed license manifest structure per 03-RESEARCH §Pattern 6 + Example 4.
// All fields are required in the JSON representation.
// The Signature field is omitted from the canonical form used for signing.
type Manifest struct {
	LicenseID       string    `json:"license_id"`
	CustomerID      string    `json:"customer_id"`
	TenantID        string    `json:"tenant_id"`
	Tier            string    `json:"tier"`             // "commercial" | "enterprise"
	ExpiryUTC       time.Time `json:"expiry_utc"`
	AllowedFeatures []string  `json:"allowed_features"`
	Signature       string    `json:"signature,omitempty"` // base64 ASN.1-DER; excluded from canonical form
}

// canonicalJSON serializes a Manifest in a deterministic form for signing and verification.
//
// Strategy per 03-RESEARCH Example 4:
//  1. Zero the Signature field (excluded from signing scope).
//  2. Sort AllowedFeatures alphabetically (operator-supplied; any semantically-equivalent
//     ordering must canonicalize to the same bytes).
//  3. Serialize via a sorted-key JSON encoder (top-level keys sorted alphabetically).
//
// This is intentionally simpler than full RFC8785 (no float canonicalization edge cases —
// we don't sign any floats). Anti-pattern reference: 03-RESEARCH §Hand-rolling JSON
// canonicalization — we use stdlib encoding/json per-key, no third-party lib.
func canonicalJSON(m *Manifest) ([]byte, error) {
	cp := *m
	cp.Signature = ""

	// Sort AllowedFeatures alphabetically.
	if cp.AllowedFeatures != nil {
		sorted := make([]string, len(cp.AllowedFeatures))
		copy(sorted, cp.AllowedFeatures)
		sort.Strings(sorted)
		cp.AllowedFeatures = sorted
	}

	return sortedKeysJSON(&cp)
}

// CanonicalJSON is the exported form of canonicalJSON, used by cmd/license-gen so that
// signer and verifier share the exact same canonicalization path.
// T-3-license-canon-drift: signer imports this function from the license package.
func CanonicalJSON(m *Manifest) ([]byte, error) {
	return canonicalJSON(m)
}

// sortedKeysJSON serializes v to JSON with top-level object keys sorted alphabetically.
// Per 03-RESEARCH Example 4 lines 909-940.
func sortedKeysJSON(v interface{}) ([]byte, error) {
	bs, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal to JSON: %w", err)
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(bs, &asMap); err != nil {
		return nil, fmt.Errorf("unmarshal to map: %w", err)
	}

	keys := make([]string, 0, len(asMap))
	for k := range asMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		// Key.
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("marshal key %q: %w", k, err)
		}
		buf.Write(kb)
		buf.WriteByte(':')
		// Value.
		vb, err := json.Marshal(asMap[k])
		if err != nil {
			return nil, fmt.Errorf("marshal value for key %q: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
