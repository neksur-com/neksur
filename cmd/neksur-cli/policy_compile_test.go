// neksur-cli policy compile — unit tests for Plan 01-09 Task 3.
//
// Each test:
//   1. Writes a CEL expression to a tempfile (or skips for the
//      missing-file case).
//   2. Invokes runPolicyCompile directly (no os.Exit shenanigans —
//      the function returns an int code).
//   3. Captures stdout + stderr via os.Pipe (the function uses
//      `fmt.Printf`/`fmt.Fprintln(os.Stderr, ...)` so we redirect
//      the os-level FDs to capture them).
//   4. Asserts: exit code + stdout substring + stderr substring.
//
// Coverage:
//   - TestPolicyCompileValidCEL: trivial `true` literal compiles.
//   - TestPolicyCompileInvalidCEL: gibberish text fails with
//     cel.ErrCompileFailed.
//   - TestPolicyCompileMissingFile: non-existent path exits 2.
//   - TestPolicyCompileUsesPlan15Env: Plan 01-05 manifest.has_column
//     binding resolves (proves the CLI dogfoods the SAME env the
//     gateway uses at runtime, not a fresh / partial env).

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdio runs fn() while redirecting os.Stdout + os.Stderr
// into pipes; returns the captured stdout + stderr strings + the
// return value of fn. Used to assert on the printed messages the
// subcommand emits.
func captureStdio(t *testing.T, fn func() int) (stdout, stderr string, code int) {
	t.Helper()
	origStdout := os.Stdout
	origStderr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr

	doneOut := make(chan string)
	doneErr := make(chan string)
	go func() {
		b, _ := io.ReadAll(rOut)
		doneOut <- string(b)
	}()
	go func() {
		b, _ := io.ReadAll(rErr)
		doneErr <- string(b)
	}()

	code = fn()

	// Flush + restore.
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = origStdout
	os.Stderr = origStderr
	stdout = <-doneOut
	stderr = <-doneErr
	return stdout, stderr, code
}

// TestPolicyCompileValidCEL: trivial `true` is a valid CEL bool
// expression — should compile cleanly and exit 0.
func TestPolicyCompileValidCEL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.cel")
	if err := os.WriteFile(path, []byte("true"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout, stderr, code := captureStdio(t, func() int {
		return runPolicyCompile(context.Background(), []string{path})
	})

	if code != 0 {
		t.Fatalf("expected exit 0, got %d. stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "compiles cleanly") {
		t.Errorf("stdout missing 'compiles cleanly': %q", stdout)
	}
	if !strings.Contains(stdout, "valid.cel") {
		t.Errorf("stdout missing file path: %q", stdout)
	}
}

// TestPolicyCompileInvalidCEL: gibberish should fail at compile and
// surface the cel.ErrCompileFailed sentinel hint.
func TestPolicyCompileInvalidCEL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.cel")
	// `this is not CEL` — three bare identifiers with no operator;
	// cel-go rejects this as a parse error.
	if err := os.WriteFile(path, []byte("this is not CEL"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, stderr, code := captureStdio(t, func() int {
		return runPolicyCompile(context.Background(), []string{path})
	})

	if code != 1 {
		t.Fatalf("expected exit 1, got %d. stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "policy compile:") {
		t.Errorf("stderr missing 'policy compile:' prefix: %q", stderr)
	}
	// cel-go's compile error message contains "ERROR:" — verify the
	// wrapped error surfaces it.
	if !strings.Contains(stderr, "compile failed") &&
		!strings.Contains(stderr, "ERROR:") {
		t.Errorf("stderr does not look like a CEL compile failure: %q", stderr)
	}
}

// TestPolicyCompileMissingFile: non-existent path → exit 2.
func TestPolicyCompileMissingFile(t *testing.T) {
	_, stderr, code := captureStdio(t, func() int {
		return runPolicyCompile(context.Background(),
			[]string{"/nonexistent/path/does/not/exist.cel"})
	})

	if code != 2 {
		t.Fatalf("expected exit 2, got %d. stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "read") {
		t.Errorf("stderr should mention 'read' (file open failed): %q", stderr)
	}
}

// TestPolicyCompileUsesPlan15Env: a CEL expression referencing the
// Plan 01-05 custom binding `manifest.has_column(table, "ssn")`
// must compile cleanly — this proves the CLI dogfoods the SAME env
// the gateway uses at runtime, not a stripped-down test env.
func TestPolicyCompileUsesPlan15Env(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p1-bindings.cel")
	const celText = `!manifest.has_column(table, "ssn")`
	if err := os.WriteFile(path, []byte(celText), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout, stderr, code := captureStdio(t, func() int {
		return runPolicyCompile(context.Background(), []string{path})
	})

	if code != 0 {
		t.Fatalf("expected exit 0 for valid Plan 01-05 binding usage, got %d. stderr=%s",
			code, stderr)
	}
	if !strings.Contains(stdout, "compiles cleanly") {
		t.Errorf("stdout missing 'compiles cleanly' for Plan 01-05 binding: %q", stdout)
	}
}

// TestPolicyCompileWrongUsage: no positional arg → exit 2 (usage).
func TestPolicyCompileWrongUsage(t *testing.T) {
	_, stderr, code := captureStdio(t, func() int {
		return runPolicyCompile(context.Background(), nil)
	})
	if code != 2 {
		t.Fatalf("expected exit 2 for missing arg, got %d. stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr missing usage text: %q", stderr)
	}
}
