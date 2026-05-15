// Unit tests for the L1 Catalog Gateway handlers — fakes only, no
// external dependencies (no Phase1Fixture, no testcontainers).
//
// Tests cover:
//   - TestCommitHandlerPathValidation — malformed namespace returns 400.
//   - TestCommitHandlerTenantMissing — missing tenant ctx returns 500.
//   - TestExtractPrincipalChainMTLSFirst — mTLS SAN beats Authorization.
//   - TestExtractPrincipalChainAuthHeaderSecond — Authorization beats session.
//   - TestExtractPrincipalChainSessionFallback — session fallback when no
//     mTLS / Authorization.
//   - TestExtractPrincipalNoSourcesFails — all three sources missing → ErrPrincipalMissing.
//   - TestValidatePrincipalNotEmpty — empty sub returns ErrPrincipalMissing.

package iceberg

import (
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"crypto/rand"
	"crypto/tls"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/tenant"
)

// TestCommitHandlerPathValidation — malformed prefix / namespace /
// table identifiers should return 400 BEFORE the handler reaches any
// downstream call. We don't need a fully populated Deps — the path
// check fires before any Deps field is dereferenced (the tenant ctx
// check fires first; with a tenant we then hit the path check).
func TestCommitHandlerPathValidation(t *testing.T) {
	deps := Deps{} // intentionally empty — path check fires before any Deps.* access
	handler := CommitHandler(deps)

	cases := []struct {
		name      string
		prefix    string
		namespace string
		table     string
		wantCode  int
	}{
		{"path-traversal-namespace", "prod-polaris", "..", "orders", 400},
		{"semicolon-in-table", "prod-polaris", "test", "orders;DROP", 400},
		{"empty-prefix", "", "test", "orders", 400},
		{"unicode-in-namespace", "prod-polaris", "tëst", "orders", 400},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenantUUID := uuid.MustParse("11111111-1111-4111-8111-aaaaaaaaaaaa")
			r := httptest.NewRequest(http.MethodPost, "/v1/iceberg/x/namespaces/y/tables/z", nil)
			r = r.WithContext(tenant.WithID(r.Context(), tenantUUID))
			r.SetPathValue("prefix", tc.prefix)
			r.SetPathValue("namespace", tc.namespace)
			r.SetPathValue("table", tc.table)
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != tc.wantCode {
				t.Errorf("status = %d; want %d (case %s)", w.Code, tc.wantCode, tc.name)
			}
		})
	}
}

// TestCommitHandlerTenantMissing — no tenant ctx → 500.
func TestCommitHandlerTenantMissing(t *testing.T) {
	deps := Deps{}
	handler := CommitHandler(deps)
	r := httptest.NewRequest(http.MethodPost, "/v1/iceberg/p/namespaces/n/tables/t", nil)
	r.SetPathValue("prefix", "p")
	r.SetPathValue("namespace", "n")
	r.SetPathValue("table", "t")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 on missing tenant", w.Code)
	}
}

// TestCommitHandlerWrongMethod — non-POST returns 405.
func TestCommitHandlerWrongMethod(t *testing.T) {
	handler := CommitHandler(Deps{})
	r := httptest.NewRequest(http.MethodGet, "/v1/iceberg/p/namespaces/n/tables/t", nil)
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", w.Code)
	}
}

// TestExtractPrincipalChainMTLSFirst — mTLS SAN beats both
// Authorization header and WorkOS session.
func TestExtractPrincipalChainMTLSFirst(t *testing.T) {
	cert := mustGenerateCertWithURISAN(t, "spiffe://neksur/test/user-1")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	// All three signals present.
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	r.Header.Set("Authorization", "Bearer "+mustSignJWT(t, "auth-header-sub"))
	r = r.WithContext(tenant.WithID(r.Context(), uuid.MustParse("11111111-1111-4111-8111-aaaaaaaaaaaa")))

	p, src, err := ExtractPrincipal(r)
	if err != nil {
		t.Fatalf("ExtractPrincipal err: %v", err)
	}
	if src != SourceMTLS {
		t.Errorf("source = %q; want SourceMTLS", src)
	}
	if p.Sub != "spiffe://neksur/test/user-1" {
		t.Errorf("sub = %q; want spiffe://neksur/test/user-1", p.Sub)
	}
}

// TestExtractPrincipalChainAuthHeaderSecond — no mTLS + Authorization
// + session → SourceAuthHeader.
func TestExtractPrincipalChainAuthHeaderSecond(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	// No TLS — fall through to Authorization.
	r.Header.Set("Authorization", "Bearer "+mustSignJWT(t, "auth-header-sub"))
	r = r.WithContext(tenant.WithID(r.Context(), uuid.MustParse("22222222-2222-4222-8222-bbbbbbbbbbbb")))

	p, src, err := ExtractPrincipal(r)
	if err != nil {
		t.Fatalf("ExtractPrincipal err: %v", err)
	}
	if src != SourceAuthHeader {
		t.Errorf("source = %q; want SourceAuthHeader", src)
	}
	if p.Sub != "auth-header-sub" {
		t.Errorf("sub = %q; want auth-header-sub", p.Sub)
	}
}

// TestExtractPrincipalChainSessionFallback — no mTLS, no Authorization
// → SourceSession.
func TestExtractPrincipalChainSessionFallback(t *testing.T) {
	tenantUUID := uuid.MustParse("33333333-3333-4333-8333-cccccccccccc")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r = r.WithContext(tenant.WithID(r.Context(), tenantUUID))

	p, src, err := ExtractPrincipal(r)
	if err != nil {
		t.Fatalf("ExtractPrincipal err: %v", err)
	}
	if src != SourceSession {
		t.Errorf("source = %q; want SourceSession", src)
	}
	if p.Sub != tenantUUID.String() {
		t.Errorf("sub = %q; want tenant UUID %s", p.Sub, tenantUUID)
	}
}

// TestExtractPrincipalNoSourcesFails — all three signals missing →
// ErrPrincipalMissing.
func TestExtractPrincipalNoSourcesFails(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	// No TLS, no Authorization, no tenant ctx.
	_, _, err := ExtractPrincipal(r)
	if !errors.Is(err, ErrPrincipalMissing) {
		t.Errorf("err = %v; want ErrPrincipalMissing", err)
	}
}

// TestValidatePrincipalNotEmpty — empty sub triggers ErrPrincipalMissing.
func TestValidatePrincipalNotEmpty(t *testing.T) {
	cases := []struct {
		name string
		p    *Principal
		want error
	}{
		{"nil", nil, ErrPrincipalMissing},
		{"empty-sub", &Principal{Sub: ""}, ErrPrincipalMissing},
		{"non-empty", &Principal{Sub: "user-1"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePrincipalNotEmpty(tc.p)
			if (tc.want == nil) != (err == nil) {
				t.Errorf("err = %v; want %v", err, tc.want)
				return
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestParseJWTUnverified — extracts sub + email + roles from a typical
// WorkOS-shaped token.
func TestParseJWTUnverified(t *testing.T) {
	tok := mustSignJWTWithClaims(t, jwt.MapClaims{
		"sub":   "user-42",
		"email": "user42@example.com",
		"roles": []any{"writer", "auditor"},
	})
	p := parseJWTUnverified(tok)
	if p == nil {
		t.Fatalf("parseJWTUnverified: nil")
	}
	if p.Sub != "user-42" {
		t.Errorf("sub = %q; want user-42", p.Sub)
	}
	if p.Email != "user42@example.com" {
		t.Errorf("email = %q; want user42@example.com", p.Email)
	}
	if len(p.Roles) != 2 || p.Roles[0] != "writer" || p.Roles[1] != "auditor" {
		t.Errorf("roles = %v; want [writer auditor]", p.Roles)
	}
}

// TestParseJWTUnverifiedRolesString — space-delimited roles string is
// also accepted (some upstream IDPs emit this shape).
func TestParseJWTUnverifiedRolesString(t *testing.T) {
	tok := mustSignJWTWithClaims(t, jwt.MapClaims{
		"sub":   "user-43",
		"roles": "alpha beta gamma",
	})
	p := parseJWTUnverified(tok)
	if p == nil || len(p.Roles) != 3 {
		t.Fatalf("parseJWTUnverified roles = %v; want 3 roles", p)
	}
}

// TestExtractSANPrefersURI — URI SAN wins over DNS SAN.
func TestExtractSANPrefersURI(t *testing.T) {
	uri, _ := url.Parse("spiffe://neksur/foo")
	cert := &x509.Certificate{
		URIs:     []*url.URL{uri},
		DNSNames: []string{"foo.example.com"},
	}
	got := extractSAN(cert)
	if got != "spiffe://neksur/foo" {
		t.Errorf("extractSAN = %q; want URI", got)
	}
}

// TestExtractSANDNSFallback — DNS SAN used when URI SAN missing.
func TestExtractSANDNSFallback(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{"foo.example.com"}}
	got := extractSAN(cert)
	if got != "foo.example.com" {
		t.Errorf("extractSAN = %q; want DNS", got)
	}
}

// ---- helpers ---------------------------------------------------------

// mustGenerateCertWithURISAN constructs a self-signed cert with the
// given URI SAN. Only used for the principal extraction test — we
// don't actually mTLS-handshake; we just stuff the cert into r.TLS.
func mustGenerateCertWithURISAN(t *testing.T, sanStr string) *x509.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	uri, err := url.Parse(sanStr)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", sanStr, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{uri},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	return cert
}

// mustSignJWT builds a minimal HS256 JWT with a sub claim. Phase 1
// gateway does NOT verify signature so the secret is irrelevant to the
// test contract — we just need a parseable token.
func mustSignJWT(t *testing.T, sub string) string {
	t.Helper()
	return mustSignJWTWithClaims(t, jwt.MapClaims{"sub": sub})
}

// mustSignJWTWithClaims builds a JWT with arbitrary claims.
func mustSignJWTWithClaims(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("jwt sign: %v", err)
	}
	return signed
}
