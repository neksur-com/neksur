//go:build integration

// Plan 00.5-03 Task 3 — WorkOS middleware end-to-end integration tests
// against a real Postgres+AGE container (for the tenant lookup path)
// and a httptest.NewServer-backed WorkOS mock (for the JWT validation
// + JWKS rotation + webhook signature paths).
//
// Five tests:
//
//	TestMiddlewareValidSession      — happy path: session cookie → tenant ctx → 200
//	TestMiddlewareMissingSession401 — no cookie → 401
//	TestMiddlewareTenantNotFound    — valid JWT, unknown org → 404
//	TestJWKSRotation                — mid-test JWKS rotation, post-rotation token validates
//	TestWebhookSig                  — valid sig 200; corrupt sig 400 empty body
//
// The WorkOS mock lives at httptest.NewServer; it serves
//   GET  /sso/jwks/<clientID>      — JWKS doc (rotating per test as needed)
// and we DO NOT exercise the WorkOS authenticate endpoint here
// (the middleware path is JWT-only — AuthenticateWithCode is only
// invoked at /callback, which is covered by the unit tests in
// internal/auth/workos/client_test.go).
package integration

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	workosauth "github.com/neksur-com/neksur/internal/auth/workos"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
)

// jwksMockState holds the mutable JWKS state for a test mock. The
// JWKS handler reads under .RLock; the test rotates under .Lock.
type jwksMockState struct {
	mu  sync.RWMutex
	kid string
	pub *rsa.PublicKey
}

func (s *jwksMockState) set(kid string, pub *rsa.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kid = kid
	s.pub = pub
}

func (s *jwksMockState) get() (string, *rsa.PublicKey) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kid, s.pub
}

// newJWKSMock returns a state + httptest.Server serving the JWKS at
// /sso/jwks/<clientID>. The state can be mutated mid-test to simulate
// WorkOS key rotation.
func newJWKSMock(t *testing.T, clientID string) (*jwksMockState, *httptest.Server) {
	t.Helper()
	state := &jwksMockState{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sso/jwks/"+clientID, func(w http.ResponseWriter, r *http.Request) {
		kid, pub := state.get()
		if pub == nil {
			http.Error(w, "no key", http.StatusServiceUnavailable)
			return
		}
		nB := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		eBytes := big.NewInt(int64(pub.E)).Bytes()
		eB := base64.RawURLEncoding.EncodeToString(eBytes)
		doc := map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
				"n": nB, "e": eB,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return state, srv
}

// mintTestJWT signs a token with the given private key + kid.
func mintTestJWT(t *testing.T, priv *rsa.PrivateKey, kid string, orgID string, ttl time.Duration) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":    "user_test",
		"sid":    "sid_test",
		"org_id": orgID,
		"exp":    time.Now().Add(ttl).Unix(),
		"iat":    time.Now().Unix(),
	})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return s
}

// rsaKey generates a new RSA-2048 keypair for test use.
func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa generate: %v", err)
	}
	return k
}

// seedTenantRow INSERTs a row into public.tenants for the given
// (tenantID, workosOrgID). Returns the tenant.Repo connected to the
// fixture for subsequent operations.
//
// Run as superuser (which bypasses RLS by virtue of being a Postgres
// superuser per Phase 0 deviation #7).
func seedTenantRow(t *testing.T, fx *SaasFixture, tenantID uuid.UUID, workosOrgID string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO public.tenants (id, workos_org_id, lifecycle_state, pool)
		 VALUES ($1, $2, 'active', 'A')
		 ON CONFLICT (workos_org_id) DO NOTHING`,
		tenantID, workosOrgID,
	); err != nil {
		t.Fatalf("seedTenantRow: %v", err)
	}
}

// buildTestPool builds a pgxpool.Pool against the fixture's superuser
// DSN with the Phase 0.5 BeforeAcquire DISCARD ALL hook + the AGE
// prelude AfterConnect, suitable for tenant.Repo to consume.
func buildTestPool(t *testing.T, fx *SaasFixture) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	graph.WithBeforeAcquireDiscardAll(cfg)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	return pool
}

// makeAuthRequest builds an HTTP request carrying a session cookie
// named neksur_session with the given JWT value.
func makeAuthRequest(t *testing.T, jwt string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/foo", nil)
	r.AddCookie(&http.Cookie{
		Name:  workosauth.CookieName,
		Value: jwt,
		Path:  "/",
	})
	return r
}

// nextHandler returns a 200 handler that records the tenant id observed
// in r.Context() for the caller to inspect.
type nextHandler struct {
	gotTenant uuid.UUID
	called    bool
}

func (n *nextHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.called = true
	if tid, ok := tenant.IDFromContext(r.Context()); ok {
		n.gotTenant = tid
	}
	w.WriteHeader(http.StatusOK)
}

func TestMiddlewareValidSession(t *testing.T) {
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const clientID = "client_test_valid"
	const orgID = "org_VALID01"
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")

	// Seed: provision tenant schema + role + tenant.tenants row
	fx.ProvisionTenant(t, tenantID.String())
	seedTenantRow(t, fx, tenantID, orgID)

	// JWKS mock + WorkOS client + middleware
	priv := rsaKey(t)
	state, srv := newJWKSMock(t, clientID)
	state.set("kid1", &priv.PublicKey)

	wc, err := workosauth.NewClientWithEndpoint("sk_test", clientID, ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	pool := buildTestPool(t, fx)
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	next := &nextHandler{}
	mw := workosauth.TenantMiddleware(wc, repo)(next)

	// Token signed by current key, claiming orgID.
	tok := mintTestJWT(t, priv, "kid1", orgID, time.Hour)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, makeAuthRequest(t, tok))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if !next.called {
		t.Errorf("next handler not called")
	}
	if next.gotTenant != tenantID {
		t.Errorf("tenant ctx = %s; want %s", next.gotTenant, tenantID)
	}
}

func TestMiddlewareMissingSession401(t *testing.T) {
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const clientID = "client_test_missing"
	priv := rsaKey(t)
	state, srv := newJWKSMock(t, clientID)
	state.set("kid1", &priv.PublicKey)

	wc, err := workosauth.NewClientWithEndpoint("sk_test", clientID, ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	pool := buildTestPool(t, fx)
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	next := &nextHandler{}
	mw := workosauth.TenantMiddleware(wc, repo)(next)

	// Request with NO cookie.
	r := httptest.NewRequest("GET", "/api/foo", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", w.Code)
	}
	if next.called {
		t.Errorf("next handler called despite missing session")
	}
}

func TestMiddlewareTenantNotFound(t *testing.T) {
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const clientID = "client_test_notfound"
	// Note: we explicitly do NOT seed public.tenants for this org.
	const unknownOrgID = "org_NOEXIST"

	priv := rsaKey(t)
	state, srv := newJWKSMock(t, clientID)
	state.set("kid1", &priv.PublicKey)

	wc, err := workosauth.NewClientWithEndpoint("sk_test", clientID, ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	pool := buildTestPool(t, fx)
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	next := &nextHandler{}
	mw := workosauth.TenantMiddleware(wc, repo)(next)

	tok := mintTestJWT(t, priv, "kid1", unknownOrgID, time.Hour)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, makeAuthRequest(t, tok))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404; body=%s", w.Code, w.Body.String())
	}
	if next.called {
		t.Errorf("next handler called despite ErrTenantNotFound")
	}
}

func TestJWKSRotation(t *testing.T) {
	fx := StartSaasFixture(t)
	defer fx.Terminate()

	const clientID = "client_test_rotation"
	const orgID = "org_ROTATE1"
	tenantID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	fx.ProvisionTenant(t, tenantID.String())
	seedTenantRow(t, fx, tenantID, orgID)

	priv1 := rsaKey(t)
	priv2 := rsaKey(t)
	state, srv := newJWKSMock(t, clientID)
	state.set("kid1", &priv1.PublicKey)

	wc, err := workosauth.NewClientWithEndpoint("sk_test", clientID, ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	pool := buildTestPool(t, fx)
	defer pool.Close()
	repo := tenant.NewRepo(pool)

	next := &nextHandler{}
	mw := workosauth.TenantMiddleware(wc, repo)(next)

	// Phase 1 — validate with kid1 (warms the JWKS cache).
	tok1 := mintTestJWT(t, priv1, "kid1", orgID, time.Hour)
	w1 := httptest.NewRecorder()
	mw.ServeHTTP(w1, makeAuthRequest(t, tok1))
	if w1.Code != http.StatusOK {
		t.Fatalf("phase1 status = %d; want 200; body=%s", w1.Code, w1.Body.String())
	}

	// Rotate the JWKS — drop kid1, publish kid2.
	state.set("kid2", &priv2.PublicKey)

	// Phase 2 — a new token signed with kid2 must validate. The client's
	// keyfunc will miss the cache, force-refresh JWKS, and resolve kid2.
	next2 := &nextHandler{}
	mw2 := workosauth.TenantMiddleware(wc, repo)(next2)
	tok2 := mintTestJWT(t, priv2, "kid2", orgID, time.Hour)
	w2 := httptest.NewRecorder()
	mw2.ServeHTTP(w2, makeAuthRequest(t, tok2))

	if w2.Code != http.StatusOK {
		t.Errorf("phase2 status = %d; want 200; body=%s", w2.Code, w2.Body.String())
	}
	if next2.gotTenant != tenantID {
		t.Errorf("phase2 tenant ctx = %s; want %s", next2.gotTenant, tenantID)
	}
}

func TestWebhookSig(t *testing.T) {
	const secret = "test_webhook_secret_xyz"
	const clientID = "client_test_webhook"
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()

	wc, err := workosauth.NewClientWithEndpoint("sk_test", clientID, ".test.local", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	h := wc.HandleWebhook(secret)

	// Path 1 — valid signature → 200.
	body := `{"event":"dsync.user.created","data":{"id":"u1"}}`
	sig := signWorkOSPayloadIntegration(t, secret, body, time.Now())
	r := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	r.Header.Set("WorkOS-Signature", sig)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("valid sig: status = %d; want 200", w.Code)
	}

	// Path 2 — corrupt signature → 400 with empty body.
	corruptSig := signWorkOSPayloadIntegration(t, "WRONG_SECRET", body, time.Now())
	r2 := httptest.NewRequest("POST", "/webhooks/workos", strings.NewReader(body))
	r2.Header.Set("WorkOS-Signature", corruptSig)
	w2 := httptest.NewRecorder()
	h(w2, r2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("corrupt sig: status = %d; want 400", w2.Code)
	}
	gotBody, _ := io.ReadAll(w2.Result().Body)
	if s := strings.TrimSpace(string(gotBody)); s != "" {
		t.Errorf("corrupt sig: body = %q; want empty", string(gotBody))
	}
}

// signWorkOSPayloadIntegration mirrors the unit-test helper in
// internal/auth/workos/webhook_test.go::signWorkOSPayload. We don't
// share the test code across packages (the unit-test fn is lowercase).
//
// HMAC-SHA256(<ts>.<body>) per WorkOS SDK. Header format includes a
// SPACE after the comma — the SDK assumes `t=<ts>, v1=<sig>` (see
// workos-go v4.16 pkg/webhooks/client_test.go::mockWebhookHeader).
func signWorkOSPayloadIntegration(t *testing.T, secret, body string, ts time.Time) string {
	t.Helper()
	tsStr := fmt.Sprintf("%d", ts.UnixMilli())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsStr + "." + body))
	digest := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("t=%s, v1=%s", tsStr, digest)
}
