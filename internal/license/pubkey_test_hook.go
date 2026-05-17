// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// pubkey_test_hook.go provides a package-level override for the embedded public key PEM.
// This is needed by cmd/license-gen tests and integration tests that generate ephemeral
// keypairs and need to override the compile-time-embedded key for roundtrip verification.
//
// NOT used in production paths — the override is nil by default and loadEmbeddedPublicKey
// falls through to the //go:embed value when override is nil.

package license

import "sync/atomic"

// testPublicKeyPEM is an optional override for the embedded public key PEM.
// Nil means "use the compiled-in publicKeyPEM".
// Tests set this via SetTestPublicKeyPEM.
var testPublicKeyOverride atomic.Pointer[[]byte]

// SetTestPublicKeyPEM sets a test-only override for the embedded public key PEM.
// Pass nil to clear the override and revert to the compiled-in key.
// Called by cmd/license-gen tests and integration tests to inject ephemeral keys.
func SetTestPublicKeyPEM(pemData []byte) {
	if pemData == nil {
		testPublicKeyOverride.Store(nil)
	} else {
		testPublicKeyOverride.Store(&pemData)
	}
}
