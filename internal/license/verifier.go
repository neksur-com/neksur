// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

package license

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// gracePeriod is the fixed 7-day license expiry grace window per 03-RESEARCH Pattern 6.
// Not operator-configurable (T-3-license-grace-abuse: fixed value prevents policy bypass).
const gracePeriod = 7 * 24 * time.Hour

// Verify parses and verifies a signed license manifest.
//
// Verification steps per 03-RESEARCH §Pattern 6:
//  1. Parse JSON into Manifest struct.
//  2. Canonicalize (sorted keys, sorted AllowedFeatures, drop Signature field).
//  3. SHA-256 hash the canonical bytes.
//  4. Base64-decode the Signature field.
//  5. Load the compile-time-embedded ECDSA P-256 public key.
//  6. ecdsa.VerifyASN1 (NOT ecdsa.Verify — ASN.1-DER format per D-3.04 and SignASN1 pairing).
//  7. Check expiry + 7-day grace period (T-3-license-replay mitigation).
//
// Error behavior:
//   - Signature failure → returns (nil, error) — fail-closed.
//   - Expired beyond grace → returns (manifest, error) — manifest available for logging.
//   - Within grace → returns (manifest, nil) — operator has 7 days to rotate.
func Verify(manifestBytes []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("license: parse manifest: %w", err)
	}

	// Step 2+3: Canonicalize and hash.
	canonical, err := canonicalJSON(&m)
	if err != nil {
		return nil, fmt.Errorf("license: canonicalize: %w", err)
	}
	digest := sha256.Sum256(canonical)

	// Step 4: Decode base64 signature.
	sigBytes, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return nil, fmt.Errorf("license: decode signature: %w", err)
	}

	// Step 5: Load embedded public key.
	ecdsaPub, err := loadEmbeddedPublicKey()
	if err != nil {
		return nil, err
	}

	// Step 6: Verify signature — MUST use VerifyASN1 to match SignASN1 output.
	// T-3-license-bypass: any modification to fields covered by the signature fails here.
	if !ecdsa.VerifyASN1(ecdsaPub, digest[:], sigBytes) {
		return nil, errors.New("license: signature verification failed — refuse to boot")
	}

	// Step 7: Expiry + grace period check.
	// T-3-license-replay: IsFeatureAllowed also re-checks per Pitfall 4.
	now := time.Now().UTC()
	if now.After(m.ExpiryUTC) {
		overdue := now.Sub(m.ExpiryUTC)
		if overdue > gracePeriod {
			return &m, fmt.Errorf("license: expired beyond grace period (%s ago, grace=%s)",
				overdue.Round(time.Second), gracePeriod)
		}
		// Within grace period — license still valid; operator should rotate soon.
		// Metric license_grace_period_remaining_seconds wired in Plan 03-13.
	}

	return &m, nil
}

// GracePeriodRemaining returns the time remaining in the 7-day grace window for an
// expired manifest. Returns 0 if the manifest has not yet expired.
// Exposed for integration tests (TestLicenseExpiryGracePeriod sub-test b).
func GracePeriodRemaining(m *Manifest) time.Duration {
	now := time.Now().UTC()
	if !now.After(m.ExpiryUTC) {
		return 0
	}
	overdue := now.Sub(m.ExpiryUTC)
	if overdue >= gracePeriod {
		return 0
	}
	return gracePeriod - overdue
}

// decodeBase64 is an alias for base64.StdEncoding.DecodeString used internally.
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
