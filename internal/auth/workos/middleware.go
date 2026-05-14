package workos

import (
	"errors"
	"net/http"

	"github.com/neksur-com/neksur/internal/tenant"
)

// TenantMiddleware wraps an http.Handler with WorkOS session
// authentication + tenant context injection. This is the SINGLE
// entry point for tenant identity in the application — no handler
// reads session cookies directly; every authenticated path delegates
// to this middleware.
//
// Behavior:
//   - Cookie missing / JWT invalid / expired → HTTP 401 "unauthorized"
//   - Valid session but no matching public.tenants row → HTTP 404 "tenant not found"
//   - DB error during tenant lookup → HTTP 500 "internal error"
//   - Success → tenant id injected into ctx via tenant.WithID, request
//     forwarded to `next` with the augmented context.
//
// The middleware does NOT issue cookies (the /callback handler owns
// session-cookie issuance) and does NOT refresh JWTs (refresh-on-near-
// expiry is a Phase 1+ enhancement per RESEARCH §Pattern 2 step 7).
// Phase 0.5 uses short JWTs (5–60min) so the refresh path is
// out-of-band: a user with an expired token gets a 401, the browser
// follows the /login redirect, and AuthKit re-issues.
//
// Threat model:
//   - T-0.5-session-hijack (mitigate): cookie attrs HttpOnly+Secure+
//     SameSite=Lax (set by /callback) prevent token exfiltration via
//     XSS / CSRF; JWT validation against rotating JWKS prevents replay
//     of revoked keys.
//   - T-0.5-jwks-stale-key (mitigate): ValidateSession retries with a
//     fresh JWKS on signature-failure (covered by TestJWKSRotation).
//   - T-0.5-rls-bypass-without-guc (mitigate): tenant lookup goes
//     through public.tenant_by_workos_org (V0044 SECURITY DEFINER) so
//     it works BEFORE app.current_tenant is set; this is the ONLY
//     intended RLS-bypass path.
func TenantMiddleware(c *Client, tenantRepo *tenant.Repo) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session, err := c.LoadSession(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if session.OrganizationID == "" {
				// Edge case: the user signed in but didn't pick an
				// org. Treat as a 401 — Phase 0.5 doesn't support
				// not-yet-org-attached users.
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			tenantID, err := tenantRepo.ByWorkOSOrgID(r.Context(), session.OrganizationID)
			switch {
			case errors.Is(err, tenant.ErrTenantNotFound):
				http.Error(w, "tenant not found", http.StatusNotFound)
				return
			case err != nil:
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			ctx := tenant.WithID(r.Context(), tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
