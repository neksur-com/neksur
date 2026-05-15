// Safe-Cypher string literal sanitization — Phase 1 CR-01 mitigation.
//
// AGE 1.6's `cypher('graph', $$ ... $$)` SQL function takes the Cypher
// body as a dollar-quoted PostgreSQL text literal. Caller-supplied
// strings that are spliced into single-quoted Cypher string literals
// (e.g., `MERGE (n {iceberg_id: '%s'})`) are an injection surface — a
// single `'` in the user input terminates the Cypher string literal
// and opens an attacker-controlled clause.
//
// The Phase 1 per-package `escapeCypher` helpers (in
// internal/ingest/snapshot.go, internal/gateway/iceberg/audit.go,
// internal/policy/store/age.go, internal/detect/regex/emit.go,
// internal/detect/dispatch/poller.go) only do `'` → `\'`. AGE's Cypher
// parser does NOT consistently interpret `\'` as an escape inside the
// dollar-quoted block — the backslash becomes literal text and the
// next `'` terminates the literal, allowing arbitrary Cypher injection
// (see code review CR-01).
//
// Mitigation: a strict allowlist character-class validator that
// rejects any character that could break out of either the inner
// Cypher string literal OR the outer dollar-quoted PostgreSQL text
// literal. Callers MUST route every untrusted-input splice through
// either:
//
//   - SanitizeCypherLiteral(s) (string, error) — returns (clean, nil)
//     on a safe input, or ("", ErrCypherUnsafeLiteral) on a rejected
//     one. Use this in code paths that can plumb an error back to the
//     HTTP layer (e.g., OpenLineage receiver → 400 Bad Request).
//
//   - MustSanitizeCypherLiteral(s) string — panics on unsafe input.
//     Use this for paths where the input has already been validated by
//     an upstream check (e.g., gateway path identifiers passed through
//     identifierRegex) and a residual unsafe value indicates a
//     defence-in-depth violation worth fail-stopping.
//
// Phase 2 may switch to AGE positional parameter binding (`cypher('g',
// $$ ... $$, $1)`) when AGE 1.6's parameter-binding gap is resolved.
// Until then this allowlist is the canonical Phase 1 defence.
//
// Allowlist:
//   - ASCII letters [a-zA-Z]
//   - ASCII digits [0-9]
//   - URI-safe punctuation: `.`, `_`, `-`, `/`, `:`, `@`, `+`, `=`,
//     `~`, `%`, `?`, `&`, `,`, `[`, `]`, `(`, `)`, `*`, ` ` (space)
//   - Reasoning: OpenLineage Dataset URIs use `namespace://name` form,
//     and table identifiers + principal subs (URN / SPIFFE) need URI
//     punctuation. Cypher reserved chars `'`, `"`, `\`, `$`, `{`, `}`,
//     `` ` ``, `;` are NOT in the allowlist — they are the injection
//     vectors. Control characters (including CR/LF/NUL/tab) are NOT in
//     the allowlist.
//
// Non-ASCII (Unicode) characters are REJECTED — the AGE 1.6 Cypher
// parser's handling of multi-byte UTF-8 sequences inside dollar-quoted
// blocks is not well-tested; rejecting non-ASCII keeps the attack
// surface minimal. Phase 2 may relax this for tenants that need
// Unicode identifiers if AGE's behavior is confirmed safe.

package graph

import (
	"errors"
	"fmt"
)

// ErrCypherUnsafeLiteral is returned by SanitizeCypherLiteral when the
// input contains characters that could break out of an AGE Cypher
// dollar-quoted block + inner string literal. Callers should map this
// to HTTP 400 Bad Request (the input came from an untrusted caller).
var ErrCypherUnsafeLiteral = errors.New("graph: unsafe character in Cypher string literal")

// cypherLiteralAllow is the byte-level allowlist (ASCII only). Indexed
// by byte value; true means the character is safe to splice into a
// single-quoted Cypher string literal inside an AGE dollar-quoted
// block. Computed once at package init.
var cypherLiteralAllow = func() [256]bool {
	var a [256]bool
	// Letters.
	for b := byte('a'); b <= 'z'; b++ {
		a[b] = true
	}
	for b := byte('A'); b <= 'Z'; b++ {
		a[b] = true
	}
	// Digits.
	for b := byte('0'); b <= '9'; b++ {
		a[b] = true
	}
	// URI-safe punctuation. NOTE: `'`, `"`, `\`, `$`, `{`, `}`, `` ` ``,
	// `;`, `<`, `>`, `!`, `^`, `|`, `#` deliberately excluded — they are
	// either Cypher reserved tokens or AGE dollar-quoting risk vectors.
	for _, c := range []byte{
		'.', '_', '-', '/', ':', '@', '+', '=', '~', '%',
		'?', '&', ',', '[', ']', '(', ')', '*', ' ',
	} {
		a[c] = true
	}
	return a
}()

// SanitizeCypherLiteral validates that `s` contains only characters
// from the safe-Cypher-literal allowlist (ASCII letters, digits,
// URI-safe punctuation). Returns the input unchanged on success, or
// "" + ErrCypherUnsafeLiteral on rejection.
//
// Use this in code paths that can plumb the error back to an HTTP
// response (e.g., OpenLineage receiver, audit emission entry points).
// For deep-internal call sites that have already been validated, see
// MustSanitizeCypherLiteral.
func SanitizeCypherLiteral(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !cypherLiteralAllow[b] {
			return "", fmt.Errorf("%w: byte 0x%02x at offset %d", ErrCypherUnsafeLiteral, b, i)
		}
	}
	return s, nil
}

// MustSanitizeCypherLiteral panics on unsafe input. Use only at
// defence-in-depth chokepoints where the caller has already validated
// the input upstream (e.g., gateway path identifiers that matched
// identifierRegex). A panic here surfaces a programming bug — the
// upstream validator failed to apply, or a new untrusted-input path
// was added without routing through SanitizeCypherLiteral first.
//
// Production handlers should NOT call this on untrusted input — use
// SanitizeCypherLiteral and return 400 instead.
func MustSanitizeCypherLiteral(s string) string {
	out, err := SanitizeCypherLiteral(s)
	if err != nil {
		panic(fmt.Sprintf("graph: MustSanitizeCypherLiteral defence-in-depth violation: %v (value=%q)", err, s))
	}
	return out
}

// IsSafeCypherLiteral reports whether SanitizeCypherLiteral would
// accept `s` without consuming the value. Useful for table-driven
// tests + grep-anchored acceptance gates.
func IsSafeCypherLiteral(s string) bool {
	_, err := SanitizeCypherLiteral(s)
	return err == nil
}
