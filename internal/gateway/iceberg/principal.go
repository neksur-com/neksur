// Principal extraction — Pitfall 8 chain (mTLS SAN > Authorization
// bearer > WorkOS session).
//
// The L1 gateway needs a Principal for two purposes:
//   1. Inject into cel.Inputs.Principal so P2 write-ACL policies can
//      assert on `principal.sub` / `principal.roles`.
//   2. Audit emission (INTENDED_WRITE Person→Table edge + audit_log
//      principal_source column).
//
// Phase 1 trusts upstream-issued JWTs (the gateway is downstream of
// authentication; mTLS / Authorization headers come from a trusted
// upstream proxy). Phase 2 L4 cred vending will add JWT signature
// verification at the gateway boundary; the audit_log.principal_source
// column lets SecOps spot which path is in use per request.

package iceberg

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/neksur-com/neksur/internal/tenant"
)

// Principal is the typed bag of claims the gateway forwards to CEL +
// audit. Sub is the subject identifier (mTLS SAN URI / JWT sub claim /
// WorkOS user id); Email is the user's email when available; Roles is
// the role list for P2 ACL evaluation.
type Principal struct {
	Sub   string
	Email string
	Roles []string
}

// Source identifies which Pitfall 8 chain step produced the principal.
// The audit_log.principal_source column stores this verbatim (V0065 +
// CHECK constraint enforces the allowed values).
type Source string

// Source constants — these MUST match V0065's CHECK constraint values
// (mtls_san / auth_header / session). Any drift breaks audit log INSERTs.
const (
	// SourceMTLS — extracted from r.TLS.PeerCertificates[0]'s URI SAN.
	// Phase 0.5 ALB optionally terminates mTLS via Private CA; the
	// upstream Spark/Trino client cert's URI SAN is the principal id.
	SourceMTLS Source = "mtls_san"

	// SourceAuthHeader — extracted from `Authorization: Bearer <jwt>`
	// `sub` claim. Phase 1 does NOT verify the JWT signature (the
	// upstream WorkOS / OIDC provider already verified before
	// forwarding; gateway trusts the chain). Phase 2 L4 will sign-verify.
	SourceAuthHeader Source = "auth_header"

	// SourceSession — fallback to the WorkOS session attached by
	// TenantMiddleware. Defence-in-depth: production paths prefer mTLS
	// or Authorization bearer; session is the dev/internal path.
	SourceSession Source = "session"
)

// ExtractPrincipal applies the Pitfall 8 chain in order and returns the
// first principal it can construct. Returns ErrPrincipalMissing only
// when ALL three steps fail (production paths cannot reach this since
// TenantMiddleware guarantees a tenant ctx, which the SourceSession
// fallback uses).
//
// Step 1 — mTLS: r.TLS.PeerCertificates[0]'s URI SAN (or DNS SAN
// fallback). The trust anchor is the Phase 0.5 Private CA; ALB performs
// the chain-validity check before forwarding.
//
// Step 2 — Authorization: parse `Bearer <jwt>` and extract `sub` (and
// optional `email`) without signature verification. Phase 1 trusts the
// upstream chain; Phase 2 L4 adds signature verification at this
// boundary.
//
// Step 3 — WorkOS session: tenant ctx is already attached by
// TenantMiddleware; we fall back to using the tenant ID as the
// principal subject. This is the lowest-fidelity path (no per-user
// granularity, just per-tenant) — production deployments configure
// mTLS or upstream JWT for proper user attribution.
func ExtractPrincipal(r *http.Request) (*Principal, Source, error) {
	// Step 1 — mTLS SAN.
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cert := r.TLS.PeerCertificates[0]
		if san := extractSAN(cert); san != "" {
			return &Principal{Sub: san}, SourceMTLS, nil
		}
	}

	// Step 2 — Authorization: Bearer <jwt>.
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Bearer ") {
		token := strings.TrimPrefix(authz, "Bearer ")
		if p := parseJWTUnverified(token); p != nil {
			return p, SourceAuthHeader, nil
		}
	}

	// Step 3 — WorkOS session fallback. TenantMiddleware injects the
	// tenant UUID; we use that as the principal subject. Lower-fidelity
	// (per-tenant, not per-user) but always-on since the middleware is
	// the gateway's auth precondition.
	if tenantID, ok := tenant.IDFromContext(r.Context()); ok {
		return &Principal{Sub: tenantID.String()}, SourceSession, nil
	}

	return nil, "", ErrPrincipalMissing
}

// extractSAN returns the first URI SAN (preferred — `spiffe://...` /
// `urn:...` form) or the first DNS SAN (fallback) from a parsed
// certificate. Empty string if neither is present.
//
// SPIFFE / urn: URI SANs are the documented Phase 0.5 mTLS principal
// shape (Private CA issues client certs with `spiffe://neksur/<tenant>/<user>`
// or `urn:neksur:user:<id>` — the audit_log principal_source column
// preserves whichever shape the upstream chose).
func extractSAN(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	for _, u := range cert.URIs {
		if u != nil && u.String() != "" {
			return u.String()
		}
	}
	for _, dns := range cert.DNSNames {
		if dns != "" {
			return dns
		}
	}
	return ""
}

// parseJWTUnverified parses a JWT without signature verification and
// extracts the `sub` + `email` claims. Returns nil on parse failure
// (the chain falls through to the WorkOS session).
//
// Phase 1 explicitly does NOT verify (CONTEXT line 74 — passthrough
// mode); Phase 2 L4 adds JWKS verification at this boundary. The
// audit_log.principal_source column lets SecOps detect which path is in
// use per request — `auth_header` rows are the trust-the-upstream path.
func parseJWTUnverified(tokenStr string) *Principal {
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := jwt.MapClaims{}
	_, _, err := parser.ParseUnverified(tokenStr, claims)
	if err != nil {
		return nil
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil
	}
	email, _ := claims["email"].(string)
	roles := extractRoles(claims)
	return &Principal{Sub: sub, Email: email, Roles: roles}
}

// extractRoles pulls the role list from common claim shapes. Supports:
//   - `roles`     []string
//   - `roles`     "role1 role2 role3" (space-delimited)
//   - `groups`    []string (WorkOS convention)
// Returns empty slice on any other shape (P2 ACL policies fail-closed
// on missing roles).
func extractRoles(claims jwt.MapClaims) []string {
	if rs, ok := claims["roles"].([]any); ok {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			if s, ok := r.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if rs, ok := claims["roles"].(string); ok && rs != "" {
		return strings.Fields(rs)
	}
	if rs, ok := claims["groups"].([]any); ok {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			if s, ok := r.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// validatePrincipalNotEmpty is a defence-in-depth guard called by the
// handler before forwarding to CEL — empty principal sub MUST never
// reach P2 evaluation (a write-ACL with `principal.sub in [...]` would
// silently allow an empty-sub commit if the policy author wrote `""` in
// the allow-list).
func validatePrincipalNotEmpty(p *Principal) error {
	if p == nil || p.Sub == "" {
		return fmt.Errorf("gateway: %w: empty principal sub", ErrPrincipalMissing)
	}
	return nil
}

// Sentinel re-export so test code reading principal.go can branch on
// errors.Is without importing both this file and errors.go separately.
var _ = errors.Is
