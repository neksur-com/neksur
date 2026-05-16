// Unit tests for assertSafeCypherLiteral — WR-A1 / WR-05 closure.
//
// Tests the policy/store-scoped wrapper around graph.SanitizeCypherLiteral.
// Two halves:
//   - Safe inputs (tenant-UUID-shape, identifier-shape, etc.) return
//     (input, nil) — the function is a passthrough on accept.
//   - Unsafe inputs (each canonical injection class) return ("", err)
//     where errors.Is(err, ErrUnsafeCypherLiteral) is true AND the
//     error message includes a byte/offset hint surfaced from the
//     wrapped graph.ErrCypherUnsafeLiteral.

package store

import (
	"errors"
	"strings"
	"testing"
)

func TestAssertSafeCypherLiteral_Safe(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"tenant-uuid", "66666666-6666-4666-6666-666666666666"},
		{"namespace-dot", "sales.us"},
		{"identifier-underscore", "my_table_v2"},
		{"single-char", "a"},
		{"empty-string", ""},
		{"digits-only", "12345"},
		{"uppercase", "ABC"},
		{"uri-form", "namespace://name"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := assertSafeCypherLiteral(c.input)
			if err != nil {
				t.Fatalf("expected nil error for safe input %q, got %v", c.input, err)
			}
			if got != c.input {
				t.Fatalf("expected passthrough %q, got %q", c.input, got)
			}
		})
	}
}

func TestAssertSafeCypherLiteral_Unsafe_SingleQuote(t *testing.T) {
	assertUnsafe(t, "foo'bar")
}

func TestAssertSafeCypherLiteral_Unsafe_DoubleQuote(t *testing.T) {
	assertUnsafe(t, `foo"bar`)
}

func TestAssertSafeCypherLiteral_Unsafe_Backslash(t *testing.T) {
	assertUnsafe(t, `foo\bar`)
}

func TestAssertSafeCypherLiteral_Unsafe_Semicolon(t *testing.T) {
	assertUnsafe(t, "DROP;")
}

func TestAssertSafeCypherLiteral_Unsafe_Newline(t *testing.T) {
	assertUnsafe(t, "foo\nbar")
}

func TestAssertSafeCypherLiteral_Unsafe_NUL(t *testing.T) {
	assertUnsafe(t, "foo\x00bar")
}

func TestAssertSafeCypherLiteral_Unsafe_NonASCII(t *testing.T) {
	// German ö (multi-byte UTF-8).
	assertUnsafe(t, "föo")
}

func TestAssertSafeCypherLiteral_Unsafe_Brace(t *testing.T) {
	// Cypher template metacharacter.
	assertUnsafe(t, "{0}")
}

func TestAssertSafeCypherLiteral_Unsafe_Dollar(t *testing.T) {
	// Cypher template metacharacter + AGE dollar-quote risk.
	assertUnsafe(t, "$foo")
}

func TestAssertSafeCypherLiteral_Unsafe_Tab(t *testing.T) {
	assertUnsafe(t, "foo\tbar")
}

func TestAssertSafeCypherLiteral_Unsafe_Backtick(t *testing.T) {
	assertUnsafe(t, "foo`bar")
}

// assertUnsafe is the shared assertion: unsafe input → ("", err) where
// errors.Is(err, ErrUnsafeCypherLiteral) is true AND the error message
// surfaces a byte/offset hint from the wrapped graph-level error.
func assertUnsafe(t *testing.T, input string) {
	t.Helper()
	got, err := assertSafeCypherLiteral(input)
	if err == nil {
		t.Fatalf("expected error for unsafe input %q, got nil", input)
	}
	if got != "" {
		t.Fatalf("expected empty string return on unsafe input, got %q", got)
	}
	if !errors.Is(err, ErrUnsafeCypherLiteral) {
		t.Fatalf("expected errors.Is(err, ErrUnsafeCypherLiteral)=true, got err=%v", err)
	}
	// The wrapped graph.ErrCypherUnsafeLiteral surfaces a byte/offset hint
	// (e.g. "byte 0x27 at offset 3"). Confirm one of those hints surfaces
	// in the final message so future debuggers can correlate the rejection
	// to a specific byte.
	msg := err.Error()
	if !strings.Contains(msg, "byte 0x") || !strings.Contains(msg, "offset") {
		t.Fatalf("expected error message to include byte/offset hint, got %q", msg)
	}
}
