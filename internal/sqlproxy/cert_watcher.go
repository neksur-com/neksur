// CertWatcher — fsnotify-driven hot-reload of the sqlproxy server
// certificate. The Phase 0.5 deployment story rotates server certs
// on a 30-day cadence (Private CA + cert-manager); restarting the
// proxy on every rotation would interrupt long-running engine
// connections, so the proxy reads the cert through an indirection
// that watches the on-disk file pair and reloads in-place.
//
// Graceful degradation: a failed reload (e.g., the file system
// flipped to a stale half-written copy mid-rotation) does NOT panic
// or fall over — the watcher logs the error and KEEPS the previous
// cert. The next successful reload event swaps in the new cert. This
// matches RESEARCH §Pattern 7's "keep-going" semantics.

package sqlproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// CertWatcher caches a parsed *tls.Certificate and refreshes it on
// fsnotify Write / Create events against the cert + key file pair.
// Construct via NewCertWatcher; the *tls.Config (see tls.go) wires
// GetCertificate to the watcher's accessor.
type CertWatcher struct {
	certPath string
	keyPath  string

	mu   sync.RWMutex
	cert *tls.Certificate

	logger *slog.Logger
}

// NewCertWatcher loads the (certPath, keyPath) pair into memory and
// returns a watcher ready to serve GetCertificate. Returns an error
// if the initial load fails — callers MUST treat that as fatal (the
// proxy cannot start without a valid server cert).
//
// The watcher uses slog.Default() for reload-failure logging; callers
// that want a tagged logger should construct via NewCertWatcherWithLogger
// (not landed in dispatch A; add when needed).
func NewCertWatcher(certPath, keyPath string) (*CertWatcher, error) {
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("sqlproxy: NewCertWatcher: certPath and keyPath are required")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("sqlproxy: NewCertWatcher: initial load (%q, %q): %w", certPath, keyPath, err)
	}
	return &CertWatcher{
		certPath: certPath,
		keyPath:  keyPath,
		cert:     &cert,
		logger:   slog.Default(),
	}, nil
}

// GetCertificate is the *tls.Config.GetCertificate callback. Returns
// the most-recently-loaded cert under an RLock so concurrent
// handshakes do not block on an in-flight reload.
func (w *CertWatcher) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.cert == nil {
		return nil, fmt.Errorf("sqlproxy: CertWatcher.GetCertificate: no cert loaded")
	}
	return w.cert, nil
}

// Watch runs the fsnotify loop until ctx is cancelled. Spawn in a
// dedicated goroutine at server startup. On each Write / Create event
// against the cert or key file the watcher re-reads the pair via
// tls.LoadX509KeyPair; a successful reload swaps the cached cert
// under a write-lock, a failure logs at error severity and KEEPS the
// previous cert (graceful degradation — see package-doc rationale).
//
// Returns nil on clean shutdown (ctx cancelled) or an error if the
// fsnotify watcher itself could not be constructed (effectively only
// happens on OS resource exhaustion).
func (w *CertWatcher) Watch(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("sqlproxy: CertWatcher.Watch: new fsnotify watcher: %w", err)
	}
	defer fsw.Close()

	if err := fsw.Add(w.certPath); err != nil {
		return fmt.Errorf("sqlproxy: CertWatcher.Watch: add certPath %q: %w", w.certPath, err)
	}
	if err := fsw.Add(w.keyPath); err != nil {
		return fmt.Errorf("sqlproxy: CertWatcher.Watch: add keyPath %q: %w", w.keyPath, err)
	}

	// periodicReload backstops missed fsnotify events (WR-01). Kubernetes
	// secret-volume atomic-rename pattern fires Rename/Remove on the
	// watched file path, NOT Write/Create on the underlying inode — and
	// even with the Rename/Remove handling below an event may still be
	// dropped (fsnotify channel buffer overflow, transient OS issues).
	// A 5-min ticker ensures rotation eventually catches up even if every
	// event is missed.
	const periodicReloadInterval = 5 * time.Minute
	ticker := time.NewTicker(periodicReloadInterval)
	defer ticker.Stop()

	reload := func(reason string) {
		newCert, lerr := tls.LoadX509KeyPair(w.certPath, w.keyPath)
		if lerr != nil {
			// Graceful degradation — keep the previous cert.
			w.logger.Error("sqlproxy: cert reload failed; keeping previous cert",
				"err", lerr,
				"cert_path", w.certPath,
				"key_path", w.keyPath,
				"reason", reason,
			)
			return
		}
		w.mu.Lock()
		w.cert = &newCert
		w.mu.Unlock()
		w.logger.Info("sqlproxy: cert reloaded",
			"cert_path", w.certPath,
			"reason", reason,
		)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Periodic belt-and-suspenders reload (WR-01).
			reload("periodic")
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			// WR-01 fix: also react to Rename / Remove. Kubernetes
			// Secret-mounted volumes (cert-manager / ACM PCA pattern)
			// use atomic-rename of the parent symlink (..data dir),
			// firing Rename / Remove on the watched leaf path — but
			// NOT Write / Create on the underlying inode. fsnotify
			// v1.x requires re-adding the watch on the NEW inode
			// after a Remove for the next rotation to be observed.
			interesting := fsnotify.Write | fsnotify.Create |
				fsnotify.Rename | fsnotify.Remove
			if ev.Op&interesting == 0 {
				continue
			}
			if ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				// Re-add the watches on the new inode. Best-effort:
				// the file may not exist yet (mid-rotation) — the
				// next reload attempt below will fail gracefully and
				// the ticker / next event will retry.
				_ = fsw.Remove(w.certPath)
				_ = fsw.Remove(w.keyPath)
				if addErr := fsw.Add(w.certPath); addErr != nil {
					w.logger.Error("sqlproxy: cert watcher re-add certPath failed",
						"err", addErr, "cert_path", w.certPath)
				}
				if addErr := fsw.Add(w.keyPath); addErr != nil {
					w.logger.Error("sqlproxy: cert watcher re-add keyPath failed",
						"err", addErr, "key_path", w.keyPath)
				}
			}
			reload(ev.Op.String())
		case werr, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("sqlproxy: cert watcher fsnotify error",
				"err", werr,
			)
		}
	}
}
