package workos

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/neksur-com/neksur/internal/tenant"
)

// jwksHandler returns an http.Handler that serves a JWKS doc for the
// provided *rsa.PublicKey under the named kid.
func jwksHandler(t *testing.T, kid string, pub *rsa.PublicKey) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nB := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		eBytes := big.NewInt(int64(pub.E)).Bytes()
		eB := base64.RawURLEncoding.EncodeToString(eBytes)
		doc := map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": kid,
				"alg": "RS256",
				"use": "sig",
				"n":   nB,
				"e":   eB,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
}

// mintJWT signs a token with the given private key + kid. exp is unix epoch.
func mintJWT(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return s
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa generate: %v", err)
	}
	return k
}

func TestValidateSessionExpired(t *testing.T) {
	priv := newRSAKey(t)
	mux := http.NewServeMux()
	mux.Handle("/sso/jwks/client_test", jwksHandler(t, "kid1", &priv.PublicKey))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewClientWithEndpoint("sk_test", "client_test", ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Expired token: exp 1h ago.
	tok := mintJWT(t, priv, "kid1", jwt.MapClaims{
		"sub":    "user_01",
		"sid":    "sid_01",
		"org_id": "org_FAKE",
		"exp":    time.Now().Add(-1 * time.Hour).Unix(),
		"iat":    time.Now().Add(-2 * time.Hour).Unix(),
	})

	_, err = c.ValidateSession(context.Background(), tok)
	if err == nil {
		t.Fatal("expected ErrWorkOSSessionInvalid for expired token; got nil")
	}
	if !errors.Is(err, tenant.ErrWorkOSSessionInvalid) {
		t.Fatalf("expected errors.Is ErrWorkOSSessionInvalid; got %v", err)
	}
}

func TestValidateSessionInvalidSignature(t *testing.T) {
	priv1 := newRSAKey(t)
	priv2 := newRSAKey(t) // different key — signature will fail

	mux := http.NewServeMux()
	mux.Handle("/sso/jwks/client_test", jwksHandler(t, "kid1", &priv1.PublicKey))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewClientWithEndpoint("sk_test", "client_test", ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Sign with priv2 but advertise kid1 — JWKS for kid1 uses priv1's pub.
	tok := mintJWT(t, priv2, "kid1", jwt.MapClaims{
		"sub":    "user_01",
		"sid":    "sid_01",
		"org_id": "org_FAKE",
		"exp":    time.Now().Add(1 * time.Hour).Unix(),
	})

	_, err = c.ValidateSession(context.Background(), tok)
	if err == nil {
		t.Fatal("expected ErrWorkOSSessionInvalid for bad signature; got nil")
	}
	if !errors.Is(err, tenant.ErrWorkOSSessionInvalid) {
		t.Fatalf("expected errors.Is ErrWorkOSSessionInvalid; got %v", err)
	}
}

func TestValidateSessionValid(t *testing.T) {
	priv := newRSAKey(t)
	mux := http.NewServeMux()
	mux.Handle("/sso/jwks/client_test", jwksHandler(t, "kid1", &priv.PublicKey))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewClientWithEndpoint("sk_test", "client_test", ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	tok := mintJWT(t, priv, "kid1", jwt.MapClaims{
		"sub":    "user_01",
		"sid":    "sid_01",
		"org_id": "org_VALID01",
		"exp":    time.Now().Add(1 * time.Hour).Unix(),
		"iat":    time.Now().Unix(),
	})

	claims, err := c.ValidateSession(context.Background(), tok)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if claims.OrgID != "org_VALID01" {
		t.Errorf("OrgID = %q; want org_VALID01", claims.OrgID)
	}
	if claims.Sub != "user_01" {
		t.Errorf("Sub = %q; want user_01", claims.Sub)
	}
}

// TestLoadSessionMissingCookie covers the 401 path when no cookie is set.
func TestLoadSessionMissingCookie(t *testing.T) {
	priv := newRSAKey(t)
	mux := http.NewServeMux()
	mux.Handle("/sso/jwks/client_test", jwksHandler(t, "kid1", &priv.PublicKey))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := NewClientWithEndpoint("sk_test", "client_test", ".test.local", srv.URL)

	r := httptest.NewRequest("GET", "/", nil)
	_, err := c.LoadSession(r)
	if !errors.Is(err, tenant.ErrWorkOSSessionInvalid) {
		t.Fatalf("LoadSession missing cookie: err = %v; want ErrWorkOSSessionInvalid", err)
	}
}

// TestJWKSRefreshOnRotation covers the keyfunc retry path. First call
// caches kid1; we then rotate the JWKS to kid2 + reject kid1; a new
// token signed with kid2 must validate after the keyfunc detects the
// missing-kid and refreshes.
func TestJWKSRefreshOnRotation(t *testing.T) {
	priv1 := newRSAKey(t)
	priv2 := newRSAKey(t)

	var (
		mux        = http.NewServeMux()
		currentKey *rsa.PublicKey
		currentKid string
	)
	currentKey = &priv1.PublicKey
	currentKid = "kid1"
	// Single handler reads `currentKey` / `currentKid` from the
	// enclosing test scope — easy way to "rotate" mid-test.
	mux.HandleFunc("/sso/jwks/client_test", func(w http.ResponseWriter, r *http.Request) {
		nB := base64.RawURLEncoding.EncodeToString(currentKey.N.Bytes())
		eBytes := big.NewInt(int64(currentKey.E)).Bytes()
		eB := base64.RawURLEncoding.EncodeToString(eBytes)
		doc := map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": currentKid, "alg": "RS256", "use": "sig",
				"n": nB, "e": eB,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := NewClientWithEndpoint("sk_test", "client_test", ".test.local", srv.URL)

	// Phase 1: validate a token with kid1.
	tok1 := mintJWT(t, priv1, "kid1", jwt.MapClaims{
		"sub": "u", "org_id": "org_A", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := c.ValidateSession(context.Background(), tok1); err != nil {
		t.Fatalf("phase1 validate kid1: %v", err)
	}

	// Rotate JWKS to kid2 (priv2).
	currentKey = &priv2.PublicKey
	currentKid = "kid2"

	// Phase 2: validate a token with kid2. The client doesn't know about
	// kid2 yet — the keyfunc must refresh.
	tok2 := mintJWT(t, priv2, "kid2", jwt.MapClaims{
		"sub": "u", "org_id": "org_A", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := c.ValidateSession(context.Background(), tok2); err != nil {
		t.Fatalf("phase2 validate kid2 (post-rotation): %v", err)
	}
}

// helper to silence "unused" linter when go test runs an empty file
var _ = fmt.Sprintf
