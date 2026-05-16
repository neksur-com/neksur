// transport.go — sessionPolicyTransport, the http.RoundTripper that
// injects the per-request X-Iceberg-Session-Policy header onto LoadTable
// requests during L4 credential vending.
//
// Why a custom RoundTripper? iceberg-go v0.5.0's REST catalog does not
// expose a per-request header API — LoadTable takes (ctx, identifier).
// We need to attach the inline session policy JSON to every L4 LoadTable
// call WITHOUT affecting non-L4 calls (ListTables, GetTable, plain
// LoadTable used by L1 read paths).
//
// Mechanism (per /Users/evgeny/neksur/.planning/phases/02-cross-engine-policy-enforcement-core-read-write/02-13-DECISION.md):
//
//  1. The L4 caller (polarisAdapter.IssueScopedSTSCredentials) builds the
//     session policy JSON via credvend.BuildSessionPolicy, then attaches
//     it to the ctx via contextWithSessionPolicy(ctx, policy).
//  2. The ctx flows through iceberg-go's rest.Catalog.LoadTable(ctx, ident)
//     into rest.go's do(ctx, ...) helper, which calls
//     http.NewRequestWithContext(ctx, method, uri, nil). The Request's
//     ctx is therefore the ctx the L4 caller built.
//  3. iceberg-go applies its own default headers (incl.
//     X-Iceberg-Access-Delegation: vended-credentials) via
//     sessionTransport.RoundTrip, then delegates to the inner
//     RoundTripper — which is the sessionPolicyTransport when injected
//     via rest.WithCustomTransport.
//  4. sessionPolicyTransport.RoundTrip reads the policy from
//     req.Context(), sets the X-Iceberg-Session-Policy header (on a
//     Clone of the request so the caller's req is not mutated), and
//     delegates to its embedded base transport.
//
// Evidence (verbatim source line refs):
//   - rest.go:239 — http.NewRequestWithContext propagates ctx
//   - rest.go:182-230 — sessionTransport.RoundTrip applies defaultHeaders
//     before delegating to its inner RoundTripper
//   - rest.go:532-535 — WithCustomTransport replaces session.RoundTripper
//   - options.go:124-130 — public WithCustomTransport(http.RoundTripper) API
//
// Non-L4 calls (ListTables, GetTable, plain LoadTable) attach NO policy
// to their ctx — the transport is a no-op for those.
package polaris

import (
	"context"
	"net/http"
)

// sessionPolicyKey is the unexported context key under which the L4
// caller attaches the session policy JSON bytes. Unexported so the
// contract is confined to the polaris package; external callers cannot
// accidentally set or read the header.
type sessionPolicyKey struct{}

// contextWithSessionPolicy returns a context that the sessionPolicyTransport
// reads to set the X-Iceberg-Session-Policy header on outbound requests.
//
// policyJSON is the marshalled inline session policy from
// credvend.BuildSessionPolicy. A nil or empty []byte is treated as
// "no policy attached" by the transport (no header emitted).
func contextWithSessionPolicy(ctx context.Context, policyJSON []byte) context.Context {
	return context.WithValue(ctx, sessionPolicyKey{}, policyJSON)
}

// sessionPolicyTransport wraps an inner http.RoundTripper and, when the
// outbound request's ctx carries session policy bytes, sets the
// X-Iceberg-Session-Policy header on a Clone of the request.
//
// The transport is safe to share across goroutines — the underlying
// `next` is the only mutable shared state; per-request state is on the
// stack via req.Clone.
type sessionPolicyTransport struct {
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper. If the request's ctx carries
// a non-empty session policy []byte under sessionPolicyKey{}, the
// X-Iceberg-Session-Policy header is set on a Clone of the request
// (never mutating the caller's req) and the cloned request is forwarded
// to the inner RoundTripper. Otherwise the original request is
// forwarded unmodified — the no-op default path for non-L4 calls.
func (t *sessionPolicyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if policy, ok := req.Context().Value(sessionPolicyKey{}).([]byte); ok && len(policy) > 0 {
		// Clone before mutating — never write to the caller's req.Header.
		req = req.Clone(req.Context())
		req.Header.Set("X-Iceberg-Session-Policy", string(policy))
	}
	return t.next.RoundTrip(req)
}
