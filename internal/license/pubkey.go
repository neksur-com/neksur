// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package license

// WARNING: The public key embedded below is a TEST/STAGING key generated for
// development and integration testing purposes only.
//
// The TEST/STAGING private key lives in internal/license/testdata/test-priv.pem
// and MUST NOT be used for production license signing.
//
// Production key generation and the offline signing ceremony are documented in:
//   Plan 03-15 operator runbook: "Production License Key Ceremony"
//
// Per RESEARCH Anti-pattern §Putting the license public key in a separate file:
// the public key is embedded at compile time via //go:embed. Operators cannot
// substitute a different key without rebuilding the binary (T-3-license-keysub).

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	_ "embed"
)

//go:embed neksur-license-pubkey.pem
var publicKeyPEM []byte

// loadEmbeddedPublicKey parses the compile-time-embedded ECDSA P-256 public key.
// Returns an error if the PEM is malformed or the key is not ECDSA.
// Per 03-RESEARCH Example 4 lines 494-505.
//
// When a test override is active (via SetTestPublicKeyPEM), that PEM is used instead.
// This allows cmd/license-gen tests and integration tests to inject ephemeral keys.
func loadEmbeddedPublicKey() (*ecdsa.PublicKey, error) {
	keyPEM := publicKeyPEM
	if override := testPublicKeyOverride.Load(); override != nil {
		keyPEM = *override
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("license: embedded key PEM is malformed or missing")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("license: parse public key from embed: %w", err)
	}
	ecdsaPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("license: embedded key is not ECDSA")
	}
	return ecdsaPub, nil
}
