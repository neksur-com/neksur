// Package migrate wraps the Atlas CLI for multi-tenant Postgres
// migrations. The package exports a small, testable surface that the
// `cmd/migrate` binary and the integration test fixture
// (tests/integration/saas_fixtures.go) both consume.
//
// D-0.5.17 + D-0.5.18: Atlas (versioned mode) is the canonical migration
// runner. This package is intentionally thin — it shells out to the
// `atlas` CLI rather than vendoring the (large) Atlas Go SDK, keeping
// the dependency footprint of the core monorepo small.
//
// Concurrency: each `ApplyPublic` / `ApplyTenant` invocation is a single
// `atlas` exec; the function is safe to call concurrently against
// different DSNs but not against the same target (Postgres advisory
// locks inside Atlas serialize per-target). The tenant-loop wrapper in
// `cmd/migrate` applies tenants sequentially for simplicity (D-0.5.17
// notes a future N-worker mode is possible if rollout latency becomes
// an issue).
//
// Note: the cmd/migrate-style decomposition (separate package + thin
// main) is a Rule 3 deviation from the plan text which said "exported
// RunForTenant helper" — Go does not permit importing from `package main`,
// so the testable surface must live in a non-main package. The function
// names match what the plan referenced.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AtlasBinary is the `atlas` CLI binary name. Override via NEKSUR_ATLAS_BIN
// for environments where the CLI is installed at a non-standard path
// (e.g., a developer laptop with ~/bin/atlas; CI runners use /usr/local/bin/atlas).
var AtlasBinary = func() string {
	if v := os.Getenv("NEKSUR_ATLAS_BIN"); v != "" {
		return v
	}
	return "atlas"
}()

// DirURL is the migration-directory URL passed to `--dir`. Relative to
// the working directory the binary is invoked from.
//
// Callers that run from a non-repo-root cwd (e.g., `go test` from
// tests/integration/) should resolve an absolute path via
// ResolveDirURL() and pass it directly to ApplyPublic / ApplyTenant
// via the WithDirURL functional option (TODO: not yet wired — the
// public API takes the DirURL constant). The current consumers are
// either cmd/migrate (run from repo root via `go run ./cmd/migrate`)
// or the saas_fixtures.go test fixture, which uses ResolveDirURL()
// + a thin wrapper around exec.Command.
const DirURL = "file://migrations/postgres"

// ResolveDirURL walks up from the current working directory until it
// finds a `migrations/postgres/V0001__enable_extensions.sql` marker
// file, then returns the absolute `file://...` URL pointing at the
// directory. Includes `?format=flyway` so Atlas parses the Phase 0
// V<seq>__<slug>.sql naming convention (D-0.5.18). Returns DirURL +
// `?format=flyway` unchanged on marker-walk failure (so the relative
// form is the fallback for cwd=<repo-root> invocations).
//
// Mirrors testfixture.migrationsDir() — the marker-file walk is the
// canonical Neksur pattern for "find the repo root from anywhere".
func ResolveDirURL() string {
	root, ok := findRepoRoot()
	if !ok {
		return DirURL + "?format=flyway"
	}
	return "file://" + filepath.Join(root, "migrations", "postgres") + "?format=flyway"
}

// ResolveConfigURL returns the absolute `file://<abs>/migrations/atlas.hcl`
// URL for the Atlas config file in the repo. Falls back to the relative
// form if the marker walk fails.
func ResolveConfigURL() string {
	root, ok := findRepoRoot()
	if !ok {
		return "file://migrations/atlas.hcl"
	}
	return "file://" + filepath.Join(root, "migrations", "atlas.hcl")
}

// findRepoRoot walks the cwd upwards until it finds the canonical Phase 0
// marker file. Returns ("", false) on failure (e.g., test binary running
// in a temp dir).
func findRepoRoot() (string, bool) {
	cur, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 6; i++ {
		marker := filepath.Join(cur, "migrations", "postgres", "V0001__enable_extensions.sql")
		if _, err := os.Stat(marker); err == nil {
			return cur, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", false
}

// RevisionsSchema is the schema in which atlas writes its
// atlas_schema_revisions table. Shared across ALL tenants per
// RESEARCH §Pitfall 9 — single cross-tenant audit table.
const RevisionsSchema = "public"

// ExcludeAGECatalog is the canonical AGE-catalog exclusion. Phase 0.5
// migrations must NEVER touch ag_catalog.* (Pitfall 3). The exclude is
// enforced solely via the `exclude` block in `migrations/atlas.hcl`:
// `atlas migrate apply` in v1.2.1 does NOT accept `--exclude` on the
// CLI (only `atlas schema inspect/diff` does). Kept as a constant so
// other call sites (e.g., future declarative-mode runners) reference
// one canonical value.
const ExcludeAGECatalog = "ag_catalog.*"

// maxDeadlockAttempts is the retry budget for SQLSTATE 40P01
// (deadlock_detected). Per the plan: 3 attempts with linear backoff.
const maxDeadlockAttempts = 3

// ApplyPublic applies all pending migrations to the `public` schema of
// the database addressed by `dsn`. It is idempotent — Atlas's revision
// tracker means a second invocation against an up-to-date target is a
// no-op exit-0.
//
// Returns nil on success; a non-nil error wrapping the atlas CLI exit
// status + stderr on failure.
func ApplyPublic(ctx context.Context, dsn string) error {
	// Use --config + --env so atlas.hcl's `format = flyway` (and the
	// exclude block) take effect; --url overrides the env block's URL
	// so we don't need DATABASE_URL_PUBLIC set.
	args := []string{
		"--config", ResolveConfigURL(),
		"--env", "public",
		"migrate", "apply",
		"--url", dsn,
		"--dir", ResolveDirURL(),
		"--revisions-schema", RevisionsSchema,
	}
	return retryOnDeadlock(func() error {
		return runAtlas(ctx, args, os.Stdout, os.Stderr)
	}, maxDeadlockAttempts)
}

// ApplyTenant applies all pending migrations to the given tenant schema.
// It composes a search_path-scoped DSN of the form
//
//	<baseDSN>?search_path=<schema>,public
//
// (or appends `&search_path=...` if the base DSN already has a query
// string). The result is passed to `atlas migrate apply` with the same
// flags as ApplyPublic.
//
// `RunForTenant` is the name the plan referenced for this function; we
// keep ApplyTenant as the idiomatic name and provide RunForTenant as an
// alias below for backwards-compatibility with the plan text.
func ApplyTenant(ctx context.Context, baseDSN, schema string) error {
	dsn, err := composeTenantDSN(baseDSN, schema)
	if err != nil {
		return fmt.Errorf("ApplyTenant: compose DSN: %w", err)
	}
	args := []string{
		"--config", ResolveConfigURL(),
		"--env", "tenant",
		"migrate", "apply",
		"--url", dsn,
		"--dir", ResolveDirURL(),
		"--revisions-schema", RevisionsSchema,
	}
	return retryOnDeadlock(func() error {
		return runAtlas(ctx, args, os.Stdout, os.Stderr)
	}, maxDeadlockAttempts)
}

// RunForTenant is the plan-referenced name for ApplyTenant. Kept as a
// thin alias so tests/integration/saas_fixtures.go can use either form.
func RunForTenant(ctx context.Context, baseDSN, schema string) error {
	return ApplyTenant(ctx, baseDSN, schema)
}

// composeTenantDSN appends `search_path=<schema>,public` to the base DSN
// query string. Postgres URI parsing is forgiving enough that we can do
// this with a simple substring check rather than pulling in net/url for
// what is otherwise a tightly-scoped helper.
func composeTenantDSN(baseDSN, schema string) (string, error) {
	if schema == "" {
		return "", errors.New("composeTenantDSN: schema must be non-empty")
	}
	sep := "?"
	if strings.Contains(baseDSN, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%ssearch_path=%s,public", baseDSN, sep, schema), nil
}

// runAtlas exec's the atlas binary with the given args. stdout/stderr
// are piped through; the function returns the exec error verbatim so
// retryOnDeadlock can introspect ExitError.
func runAtlas(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, AtlasBinary, args...)
	cmd.Stdout = stdout
	// Tee stderr through a small buffer so retryOnDeadlock can inspect
	// it for SQLSTATE 40P01 (deadlock_detected).
	var stderrBuf stderrCapture
	stderrBuf.dst = stderr
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	if err != nil {
		return &atlasError{wrapped: err, stderr: stderrBuf.String()}
	}
	return nil
}

// atlasError carries the atlas exit error plus a captured copy of
// stderr for SQLSTATE inspection.
type atlasError struct {
	wrapped error
	stderr  string
}

func (e *atlasError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.stderr) > 0 {
		return fmt.Sprintf("atlas: %v\n--- stderr ---\n%s", e.wrapped, e.stderr)
	}
	return fmt.Sprintf("atlas: %v", e.wrapped)
}

func (e *atlasError) Unwrap() error { return e.wrapped }

// stderrCapture wraps an io.Writer and also retains the last N KB of
// stderr in memory so callers can grep for SQLSTATE codes. Bounded to
// 64 KB to avoid OOM on a chatty Atlas run.
type stderrCapture struct {
	dst io.Writer
	buf []byte
}

const stderrCaptureMax = 64 * 1024

func (s *stderrCapture) Write(p []byte) (int, error) {
	if s.dst != nil {
		_, _ = s.dst.Write(p)
	}
	if len(s.buf)+len(p) > stderrCaptureMax {
		room := stderrCaptureMax - len(s.buf)
		if room > 0 {
			s.buf = append(s.buf, p[:room]...)
		}
	} else {
		s.buf = append(s.buf, p...)
	}
	return len(p), nil
}

func (s *stderrCapture) String() string { return string(s.buf) }

// retryOnDeadlock runs fn up to n times. On error, inspects the wrapped
// atlasError.stderr for SQLSTATE `40P01` (deadlock_detected) and retries
// with linear backoff (attempt * 200ms). Any other error returns
// immediately.
func retryOnDeadlock(fn func() error, n int) error {
	var lastErr error
	for attempt := 1; attempt <= n; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		var aerr *atlasError
		if !errors.As(err, &aerr) {
			return err // unknown shape — surface immediately
		}
		// SQLSTATE 40P01 = deadlock_detected (Postgres canonical).
		if !strings.Contains(aerr.stderr, "40P01") {
			return err
		}
		if attempt < n {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return fmt.Errorf("retryOnDeadlock: %d attempts exhausted: %w", n, lastErr)
}
