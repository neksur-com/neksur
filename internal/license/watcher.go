// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// LicenseWatcher — fsnotify-driven hot-reload of the license manifest.
//
// Mirrors the CertWatcher pattern from internal/sqlproxy/cert_watcher.go
// per 03-PATTERNS §10.
//
// On a Write/Create/Rename/Remove event against the license file:
//  1. Re-read and re-verify the manifest via license.Verify.
//  2. On success → call SetManifest (atomic.Pointer swap, lock-free on the hot path).
//  3. On parse/verify error → log at error + KEEP the previous manifest
//     (graceful degradation: Pitfall 4 per-request expiry check gates each
//     IsFeatureAllowed call — bad parse doesn't grant new expiry).
//
// A 5-minute periodic backstop reloads the file regardless of events to
// defend against missed fsnotify events (Kubernetes Secret atomic-rename,
// channel overflow, OS transient issues).
//
// T-3-license-watcher-event-storm mitigation: the select loop coalesces
// rapid events naturally; the 5-min ticker dampens reload rate.
package license

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watcherPeriodicReloadInterval is the default backstop reload interval.
// Mirrors cert_watcher.go's 5-min constant.
const watcherPeriodicReloadInterval = 5 * time.Minute

// LicenseWatcher watches the license file for changes and hot-reloads the
// manifest via SetManifest on each successful Verify. Construct with a path
// and logger; start via Watch (run in a dedicated goroutine).
//
// Thread-safety: Watch owns one fsnotify watcher; do not call Watch concurrently.
type LicenseWatcher struct {
	path   string
	logger *slog.Logger
}

// NewLicenseWatcher constructs a LicenseWatcher for the given license file path.
// Returns an error if path is empty.
func NewLicenseWatcher(path string) (*LicenseWatcher, error) {
	if path == "" {
		return nil, fmt.Errorf("license: NewLicenseWatcher: path is required")
	}
	return &LicenseWatcher{path: path, logger: slog.Default()}, nil
}

// Watch runs the fsnotify loop until ctx is cancelled. Spawn in a dedicated
// goroutine at server startup. On each Write/Create/Rename/Remove event
// against the license file, the watcher re-reads and re-verifies the manifest:
//
//   - Success → SetManifest(newManifest).
//   - Failure → log at Error + keep the previous manifest (graceful degradation).
//
// Returns nil on clean shutdown (ctx cancelled) or an error if the fsnotify
// watcher itself could not be constructed (OS resource exhaustion).
func (w *LicenseWatcher) Watch(ctx context.Context) error {
	return watchWithInterval(ctx, w.path, watcherPeriodicReloadInterval)
}

// watchWithInterval is the implementation of the watcher loop with an injectable
// tick interval. Called by LicenseWatcher.Watch (production, 5min interval) and
// by tests (short interval for fast backstop verification).
func watchWithInterval(ctx context.Context, path string, interval time.Duration) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("license: LicenseWatcher: new fsnotify watcher: %w", err)
	}
	defer fsw.Close()

	if addErr := fsw.Add(path); addErr != nil {
		// File may not exist yet (pre-rotation window). Log and continue;
		// the ticker will retry and the Add below on Rename/Remove handles re-add.
		slog.Warn("license: watcher: could not add path; relying on periodic backstop",
			"path", path, "err", addErr)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	reload := func(reason string) {
		manifestBytes, readErr := os.ReadFile(path)
		if readErr != nil {
			// Graceful degradation — keep the previous manifest.
			// This can happen mid-rotation on Kubernetes (file not yet written).
			slog.Error("license: reload read failed; keeping previous manifest",
				"err", readErr,
				"path", path,
				"reason", reason,
			)
			return
		}
		newManifest, verifyErr := Verify(manifestBytes)
		if verifyErr != nil {
			// Graceful degradation — keep the previous manifest.
			// Pitfall 4: the old manifest's expiry is re-checked per-request via
			// IsFeatureAllowed, so an expired previous manifest stops working
			// naturally without granting access via the bad new file.
			slog.Error("license: reload verify failed; keeping previous manifest",
				"err", verifyErr,
				"path", path,
				"reason", reason,
			)
			return
		}
		SetManifest(newManifest)
		slog.Info("license: manifest hot-reloaded",
			"license_id", newManifest.LicenseID,
			"tier", newManifest.Tier,
			"expiry_utc", newManifest.ExpiryUTC,
			"reason", reason,
		)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Periodic belt-and-suspenders reload (backstop for missed events).
			reload("periodic")
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			// Kubernetes Secret-volume atomic-rename fires Rename/Remove on the
			// watched leaf path, NOT Write/Create on the underlying inode.
			// Re-add the watch on the new inode after a Remove/Rename so the
			// next rotation is also observed.
			interesting := fsnotify.Write | fsnotify.Create |
				fsnotify.Rename | fsnotify.Remove
			if ev.Op&interesting == 0 {
				continue
			}
			if ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				// Re-add watches on the new inode. Best-effort: file may not
				// exist yet mid-rotation; the ticker/next event will retry.
				_ = fsw.Remove(path)
				if addErr := fsw.Add(path); addErr != nil {
					slog.Error("license: watcher re-add path failed",
						"err", addErr, "path", path)
				}
			}
			reload(ev.Op.String())
		case werr, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			slog.Error("license: fsnotify error", "err", werr)
		}
	}
}
