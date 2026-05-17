// transport.go — databricksContextTransport and refreshOn401Transport,
// the http.RoundTripper chain that (1) injects the X-Databricks-Workspace-Id
// header onto every outbound Iceberg REST request and (2) retries once on
// 401 after refreshing the OAuth token (Pitfall 2 mitigation).
//
// Transport chain (inner to outer, per New() construction order):
//
//   http.DefaultTransport.Clone() (or BaseTransportWrap result)
//     ← databricksContextTransport   (injects X-Databricks-Workspace-Id)
//         ← refreshOn401Transport    (retries once on 401 via token refresh)
//             ← iceberg-go.sessionTransport (set via rest.WithCustomTransport)
//
// Pitfall 2 (Unity OAuth token refresh):
//   Unity's Databricks OAuth tokens may expire mid-session. iceberg-go's
//   automatic token refresh (via the oauth2-server-uri + credential props)
//   covers the normal case, but edge cases exist where the token cache
//   misses a 401 (e.g., the token was revoked server-side). The
//   refreshOn401Transport handles this by retrying the request ONCE after
//   observing a 401, triggering a fresh iceberg-go token exchange. On a
//   second consecutive 401 it returns the error to the caller with no
//   further retry (T-3-unity-401-loop DoS prevention).
//
// Pitfall 11 (no query/artifact body logging):
//   Neither transport ever logs request bodies or response bodies.
//   Error returns wrap sentinel errors only.
package unity

import (
	"net/http"
)

// databricksContextTransport wraps an inner http.RoundTripper and injects the
// X-Databricks-Workspace-Id header onto every outbound request.
//
// The WorkspaceID is set from the operator-level Config (per-tenant) — it is
// NOT taken from the HTTP request, so a malicious caller cannot spoof
// workspace routing (T-3-unity-workspace-spoof mitigation).
//
// The transport is safe to share across goroutines — workspaceID is set once
// at construction and never mutated; per-request state is on the stack via
// req.Clone.
type databricksContextTransport struct {
	next        http.RoundTripper
	workspaceID string
}

// RoundTrip implements http.RoundTripper. Sets X-Databricks-Workspace-Id on a
// Clone of the request (never mutating the caller's req) and delegates to the
// inner RoundTripper.
func (t *databricksContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating — never write to the caller's req.Header.
	req = req.Clone(req.Context())
	req.Header.Set("X-Databricks-Workspace-Id", t.workspaceID)
	return t.next.RoundTrip(req)
}

// refreshOn401Transport wraps an inner http.RoundTripper and retries the
// request ONCE when the upstream returns HTTP 401 (token expired / revoked).
//
// On the retry the transport clones the original request (so the body, if
// any, is re-readable — iceberg-go REST catalog requests are always non-body
// or have a serialized body already read into a bytes.Reader). If the retry
// also returns 401, the response is returned as-is to the caller — no
// infinite loop (T-3-unity-401-loop DoS mitigation).
//
// This transport does NOT itself perform the OAuth token refresh — it relies
// on iceberg-go's sessionTransport (which wraps the full transport chain) to
// re-exchange the credential when it sees the 401. The retry in RoundTrip
// causes iceberg-go's inner token-refresh logic to fire on the cloned request.
//
// The transport is safe to share across goroutines — no mutable state.
type refreshOn401Transport struct {
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper. Issues the request via the inner
// transport; if the response is 401, clones the request and retries once.
// Pitfall 2: retries AT MOST ONCE — returns the 401 response on the second
// attempt without closing/discarding it so the caller can read the body.
func (t *refreshOn401Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	// 401 — close the first response body to free the connection, then
	// retry once. The cloned request preserves ctx, headers, URL, method.
	// Per Pitfall 11: we do NOT log the response body.
	resp.Body.Close()

	retryReq := req.Clone(req.Context())
	return t.next.RoundTrip(retryReq)
}
