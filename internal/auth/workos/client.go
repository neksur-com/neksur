// Package workos wraps the WorkOS Go SDK with Neksur-specific session
// + JWKS validation + cookie management. The package exports three
// public surfaces:
//
//   - Client: WorkOS API wrapper (NewClient, AuthenticateWithCode,
//     ValidateSession, LoadSession). Implements lazy + rotation-aware
//     JWKS caching against `<endpoint>/sso/jwks/<clientID>`.
//   - TenantMiddleware: HTTP middleware that loads a session cookie,
//     validates the JWT, looks up the tenant id, injects into ctx.
//   - HandleWebhook: HTTP handler for WorkOS webhooks. Verifies the
//     WorkOS-Signature header BEFORE checking SCIM_ENABLED — even when
//     the feature is disabled, unsigned/wrong-sig requests return 400
//     silently (D-0.5.21 T-0.5-session-hijack).
//
// Design rationale (RESEARCH §Pattern 2 lines 521-615 + §Don't Hand-Roll
// line 1020): we lean on the workos-go SDK's webhooks.ValidatePayload for
// signature verification (constant-time HMAC SHA-256 with timestamp
// tolerance + the WorkOS `t=<unix_ms>,v1=<hex>` header format), and on
// golang-jwt/v5 for JWT parsing + JWKS verification. Hand-rolled HMAC /
// JWT code is forbidden by the Phase 0 conventions (CLAUDE.md "no mocks
// for critical paths").
package workos

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/workos/workos-go/v4/pkg/usermanagement"

	"github.com/neksur-com/neksur/internal/tenant"
)

// _ = uuid.New // keep the uuid import even if it isn't referenced
// directly (it's part of the public Session type's downstream consumers).
var _ = uuid.Nil

// jwksTTL is how long a successful JWKS fetch is cached before the next
// validation triggers a refetch. Per RESEARCH Open Q#4: WorkOS rotates
// keys infrequently but unpredictably; a 1h TTL gives us a tight upper
// bound on stale-key liveness without pummeling WorkOS on every request.
// We ALSO re-fetch eagerly on a signature-verification failure (see
// ValidateSession), which is the path that catches surprise rotations.
const jwksTTL = 1 * time.Hour

// Default WorkOS API base URL. Overridable per-Client via NewClientWithEndpoint
// for testing (httptest.NewServer mock) and rare staging environments.
const defaultEndpoint = "https://api.workos.com"

// CookieName is the session cookie that the middleware reads. Matches
// the cookie set by /callback per D-0.5.21 T-0.5-session-hijack.
const CookieName = "neksur_session"

// Client is the WorkOS API wrapper. Construct via NewClient or
// NewClientWithEndpoint. The struct is immutable after construction
// except for the JWKS cache (jwks*, mu).
type Client struct {
	apiKey       string
	clientID     string
	cookieDomain string
	endpoint     string
	um           *usermanagement.Client
	httpClient   *http.Client

	mu          sync.RWMutex
	jwksKeys    map[string]*rsa.PublicKey
	jwksFetched time.Time
}

// Session is the post-authentication tuple returned by
// AuthenticateWithCode. RefreshToken is opaque to callers; the SDK
// re-validates / refreshes via AuthenticateWithRefreshToken (not yet
// wired in Phase 0.5 — short JWTs handle the common case per
// RESEARCH Pitfall 7 acceptance).
type Session struct {
	AccessToken    string
	RefreshToken   string
	OrganizationID string
	UserID         string
	ExpiresAt      time.Time
}

// Claims is the subset of JWT claims we read. WorkOS access tokens
// follow the standard RFC 7519 + workos-specific `org_id` / `sid`
// extensions.
type Claims struct {
	Sub   string
	Sid   string
	Exp   int64
	OrgID string
}

// NewClient constructs a Client against the production WorkOS endpoint.
// Equivalent to NewClientWithEndpoint(apiKey, clientID, cookieDomain,
// "https://api.workos.com").
func NewClient(apiKey, clientID, cookieDomain string) (*Client, error) {
	return NewClientWithEndpoint(apiKey, clientID, cookieDomain, defaultEndpoint)
}

// NewClientWithEndpoint constructs a Client against the given endpoint.
// The endpoint is the WorkOS API base URL (no trailing slash);
// JWKS is fetched from <endpoint>/sso/jwks/<clientID>.
//
// Used by tests/integration/workos_session_test.go to point the client
// at a httptest.NewServer mock.
func NewClientWithEndpoint(apiKey, clientID, cookieDomain, endpoint string) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("workos: NewClient: empty apiKey")
	}
	if clientID == "" {
		return nil, errors.New("workos: NewClient: empty clientID")
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")

	um := usermanagement.NewClient(apiKey)
	um.Endpoint = endpoint

	return &Client{
		apiKey:       apiKey,
		clientID:     clientID,
		cookieDomain: cookieDomain,
		endpoint:     endpoint,
		um:           um,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		jwksKeys:     map[string]*rsa.PublicKey{},
	}, nil
}

// CookieDomain exposes the domain configured for session cookies (used
// by /callback handlers to set the Set-Cookie header).
func (c *Client) CookieDomain() string { return c.cookieDomain }

// ClientID returns the configured WorkOS client id (used by /callback
// for the AuthenticateWithCode call).
func (c *Client) ClientID() string { return c.clientID }

// AuthenticateWithCode exchanges the OAuth `code` (received at /callback)
// for an access token + refresh token + organization id. Wraps the SDK
// call with error context.
func (c *Client) AuthenticateWithCode(ctx context.Context, code string) (*Session, error) {
	resp, err := c.um.AuthenticateWithCode(ctx, usermanagement.AuthenticateWithCodeOpts{
		ClientID: c.clientID,
		Code:     code,
	})
	if err != nil {
		return nil, fmt.Errorf("workos: authenticate: %w", err)
	}

	// Best-effort extract `exp` from the access token. If it fails (e.g.,
	// malformed token), we still return the session with a zero ExpiresAt
	// — the next ValidateSession call will catch the malformed JWT.
	expires := time.Now().Add(1 * time.Hour) // safe default
	if claims, perr := parseClaimsUnverified(resp.AccessToken); perr == nil && claims.Exp > 0 {
		expires = time.Unix(claims.Exp, 0)
	}

	return &Session{
		AccessToken:    resp.AccessToken,
		RefreshToken:   resp.RefreshToken,
		OrganizationID: resp.OrganizationID,
		UserID:         resp.User.ID,
		ExpiresAt:      expires,
	}, nil
}

// ValidateSession parses + verifies the access token against the cached
// JWKS. Re-fetches JWKS once on a signature-verification failure so
// post-rotation tokens validate correctly. Returns ErrWorkOSSessionInvalid
// on any failure (caller maps to HTTP 401).
//
// JWKS rotation contract (T-0.5-jwks-stale-key mitigation):
//   1. parse → keyfunc resolves a *rsa.PublicKey from the in-memory cache.
//   2. If cache miss OR signature error, refresh JWKS once, retry.
//   3. Still fails → ErrWorkOSSessionInvalid.
//
// The single retry-on-fail bounds the request blast radius: even a
// systematic JWKS-fetch failure costs at most one extra HTTP roundtrip
// per request, never an infinite loop.
func (c *Client) ValidateSession(ctx context.Context, accessToken string) (*Claims, error) {
	// First attempt — cached key.
	tok, err := jwt.Parse(accessToken, c.makeKeyfunc(ctx, false))
	if err == nil && tok.Valid {
		claims, cerr := extractClaims(tok)
		if cerr != nil {
			return nil, fmt.Errorf("workos: validate: %w", tenant.ErrWorkOSSessionInvalid)
		}
		return claims, nil
	}

	// Signature failure → force JWKS refresh, retry once.
	if isSignatureError(err) || isKeyNotFoundError(err) {
		tok, err = jwt.Parse(accessToken, c.makeKeyfunc(ctx, true))
		if err == nil && tok.Valid {
			claims, cerr := extractClaims(tok)
			if cerr != nil {
				return nil, fmt.Errorf("workos: validate: %w", tenant.ErrWorkOSSessionInvalid)
			}
			return claims, nil
		}
	}
	return nil, fmt.Errorf("workos: validate: %w", tenant.ErrWorkOSSessionInvalid)
}

// LoadSession reads the session cookie from r and validates the JWT.
// Returns (Session, nil) on success; (nil, ErrWorkOSSessionInvalid) on
// any failure path (missing cookie, malformed JWT, expired, bad sig).
//
// The middleware uses LoadSession; calling code does NOT manually
// read the cookie. This keeps the cookie name in one place.
func (c *Client) LoadSession(r *http.Request) (*Session, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return nil, fmt.Errorf("workos: loadsession: %w", tenant.ErrWorkOSSessionInvalid)
	}
	if cookie.Value == "" {
		return nil, fmt.Errorf("workos: loadsession: %w", tenant.ErrWorkOSSessionInvalid)
	}

	claims, err := c.ValidateSession(r.Context(), cookie.Value)
	if err != nil {
		return nil, err
	}
	return &Session{
		AccessToken:    cookie.Value,
		OrganizationID: claims.OrgID,
		UserID:         claims.Sub,
		ExpiresAt:      time.Unix(claims.Exp, 0),
	}, nil
}

// makeKeyfunc returns a jwt.Keyfunc bound to this client. When forceRefresh
// is true, the keyfunc always fetches fresh JWKS before resolving keys.
// When false, it uses the cache if fresh; otherwise refreshes.
func (c *Client) makeKeyfunc(ctx context.Context, forceRefresh bool) jwt.Keyfunc {
	return func(tok *jwt.Token) (interface{}, error) {
		// Only RSA-signed tokens are accepted from WorkOS.
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("workos: unexpected signing method: %v", tok.Header["alg"])
		}
		kid, _ := tok.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("workos: jwt header missing kid")
		}

		c.mu.RLock()
		key, ok := c.jwksKeys[kid]
		fresh := time.Since(c.jwksFetched) < jwksTTL
		c.mu.RUnlock()

		if ok && fresh && !forceRefresh {
			return key, nil
		}

		if err := c.refreshJWKS(ctx); err != nil {
			return nil, fmt.Errorf("workos: refresh jwks: %w", err)
		}

		c.mu.RLock()
		key, ok = c.jwksKeys[kid]
		c.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("workos: no key for kid=%s after refresh", kid)
		}
		return key, nil
	}
}

// refreshJWKS fetches <endpoint>/sso/jwks/<clientID>, parses the JWK
// set, and replaces the in-memory key map.
func (c *Client) refreshJWKS(ctx context.Context) error {
	url := fmt.Sprintf("%s/sso/jwks/%s", c.endpoint, c.clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("jwks HTTP %d: %s", resp.StatusCode, string(body))
	}
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	newKeys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := jwkToRSA(k.N, k.E)
		if err != nil {
			continue
		}
		newKeys[k.Kid] = pub
	}
	c.mu.Lock()
	c.jwksKeys = newKeys
	c.jwksFetched = time.Now()
	c.mu.Unlock()
	return nil
}

// jwkToRSA constructs an *rsa.PublicKey from the URL-base64 n/e fields
// of a JWK. Pure helper; no I/O.
func jwkToRSA(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nBytes)
	var e int
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

// parseClaimsUnverified extracts claims from an access token without
// verifying the signature. Used by AuthenticateWithCode to populate
// Session.ExpiresAt for downstream cookie Max-Age computation. The
// downstream ValidateSession call DOES verify the signature.
func parseClaimsUnverified(accessToken string) (*Claims, error) {
	parser := jwt.NewParser()
	tok, _, err := parser.ParseUnverified(accessToken, jwt.MapClaims{})
	if err != nil {
		return nil, err
	}
	return extractClaims(tok)
}

// extractClaims converts the jwt.MapClaims into our typed Claims struct.
func extractClaims(tok *jwt.Token) (*Claims, error) {
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("workos: claims not a MapClaims")
	}
	c := &Claims{}
	if v, ok := mc["sub"].(string); ok {
		c.Sub = v
	}
	if v, ok := mc["sid"].(string); ok {
		c.Sid = v
	}
	if v, ok := mc["org_id"].(string); ok {
		c.OrgID = v
	}
	// jwt-go represents numerics as float64 by default.
	if v, ok := mc["exp"].(float64); ok {
		c.Exp = int64(v)
	}
	return c, nil
}

// isSignatureError reports whether err is jwt's "signature is invalid"
// path. We retry with a fresh JWKS in that case.
func isSignatureError(err error) bool {
	return errors.Is(err, jwt.ErrSignatureInvalid) ||
		errors.Is(err, jwt.ErrTokenSignatureInvalid)
}

// isKeyNotFoundError reports whether our makeKeyfunc returned the
// "no key for kid=... after refresh" / "missing kid" sentinel that
// jwt.Parse wraps. We retry once on this path too.
func isKeyNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no key for kid") ||
		strings.Contains(s, "missing kid")
}
