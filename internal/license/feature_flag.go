// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// Package-level feature flag accessor backed by an atomic.Pointer[Manifest].
//
// Hot-reload path: Plan 03-13 fsnotify watcher calls SetManifest on file change.
// Expiry re-check per Pitfall 4: every IsFeatureAllowed call re-checks time.Now()
// against the manifest's ExpiryUTC — the 24h cron recheck catches rotation but does
// NOT gate per-feature checks. This ensures expired licenses are disabled immediately
// on the next feature check even if the cron has not run yet.
//
// Fail-closed semantics: if no manifest is loaded (current.Load() == nil), or if
// the manifest is expired beyond the grace period, IsFeatureAllowed returns false.
// This prevents elevation-of-privilege on boot before license verification runs.

package license

import (
	"sync/atomic"
	"time"
)

// current is the in-memory cached Manifest pointer.
// Uses atomic.Pointer for lock-free reads on the hot path (Plan 03-13 hot-reload).
var current atomic.Pointer[Manifest]

// SetManifest replaces the in-memory cached Manifest. Called at boot by license.Verify
// (via server startup in Plan 03-13) and on fsnotify license file rotation events.
// Passing nil clears the cached manifest (fail-closed until next SetManifest call).
func SetManifest(m *Manifest) {
	current.Store(m)
}

// IsFeatureAllowed reports whether the given feature name is present in the cached
// Manifest's AllowedFeatures slice AND the manifest is not expired beyond grace period.
//
// Fail-closed: returns false when:
//   - No manifest is loaded (current.Load() == nil).
//   - Manifest is expired beyond the 7-day grace period (Pitfall 4 per-request re-check).
//   - The feature name is not in AllowedFeatures (exact string match, no glob/regex —
//     T-3-license-feature-injection: unknown feature returns false).
func IsFeatureAllowed(feature string) bool {
	m := current.Load()
	if m == nil {
		return false
	}

	// Per-request expiry re-check (Pitfall 4): do not rely on the 24h cron alone.
	now := time.Now().UTC()
	if now.After(m.ExpiryUTC) {
		overdue := now.Sub(m.ExpiryUTC)
		if overdue > gracePeriod {
			// Expired beyond grace — fail-closed immediately.
			return false
		}
		// Within grace period — still allowed; operator should rotate.
	}

	for _, f := range m.AllowedFeatures {
		if f == feature {
			return true
		}
	}
	return false
}
