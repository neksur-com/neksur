// Package store — safe-cypher helper for the AGE-backed policy loader.
//
// assertSafeCypherLiteral wraps graph.SanitizeCypherLiteral with a
// policy-store-scoped error sentinel so callers can errors.Is on the
// store-level sentinel rather than reaching into the graph package's
// internal errors. WR-A1 / WR-05 fix — replaces the previous escapeCypher
// shim that routed to graph.MustSanitizeCypherLiteral and panicked on
// bytes outside the ASCII allowlist.
//
// Two store-side callers consume this:
//
//   - age.go::LoadPoliciesForTable — propagates the error up through
//     ExecuteInTenant so the gateway's fail-closed Evaluate boundary
//     responds with 503 + audit-log (rather than crashing the process).
//   - attribute.go::layer2Graph — converts the error to slog.WarnContext
//     plus a fail-soft `return ""` (Pitfall 8: Layer 2 errors must NOT
//     bubble to CEL; the resolver consults Layer 3 instead).
//
// The panicking MustSanitizeCypherLiteral variant continues to exist in
// internal/graph/safe_cypher.go for callers that prefer panic-on-bug
// semantics (boot-time validation, etc.) — the AGE store no longer uses
// it.

package store

import (
	"errors"
	"fmt"

	"github.com/neksur-com/neksur/internal/graph"
)

// ErrUnsafeCypherLiteral is the store-scoped sentinel returned by
// assertSafeCypherLiteral on inputs that fail the canonical ASCII
// allowlist. Wrap with %w when bubbling up so callers
// (LoadPoliciesForTable, layer2Graph) can errors.Is on it without
// coupling to graph internals.
var ErrUnsafeCypherLiteral = errors.New("policy/store: unsafe cypher literal")

// assertSafeCypherLiteral validates s against the ASCII allowlist
// enforced by graph.SanitizeCypherLiteral. Returns (s, nil) on safe
// inputs and ("", wrapped error) on rejection. The error wraps
// ErrUnsafeCypherLiteral so callers can branch via errors.Is, while the
// underlying graph error (which carries byte/offset diagnostic
// information) remains accessible via errors.Unwrap chains.
func assertSafeCypherLiteral(s string) (string, error) {
	clean, err := graph.SanitizeCypherLiteral(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsafeCypherLiteral, err)
	}
	return clean, nil
}
