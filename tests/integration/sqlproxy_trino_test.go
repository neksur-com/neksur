//go:build integration

// Plan 02-05 Wave 2 dispatch D — TestSQLProxyTrinoInjection.
//
// Boots Phase2Fixture, seeds a Table + an `active` CompiledPolicy node
// for the trino engine, starts the sqlproxy.Server in-process via
// httptest.StartTLS, and asserts:
//
//   1. POST /v1/sql/trino/{prefix}/dummy with a valid envelope returns
//      HTTP 200 and a {"rewritten_query": "..."} body containing the
//      structural splice marker `/* neksur-policy: ` added by dispatch
//      B's rewriteWithBody helper.
//   2. Cache-hit latency over 100 sequential requests is P95 < 50 ms
//      (REQ-NFR-latency-sql-proxy budget).
//
// Phase 2 stops at injection — Server's success path returns a
// {"rewritten_query": "..."} envelope without forwarding to a live
// Trino HTTP backend, so this test does NOT need fx.Trino's listener
// to participate in the proxy round-trip. Live engine forwarding lands
// in Phase 3.
//
// This file also hosts the file-local startSqlproxyTestServer helper
// shared with sqlproxy_mtls_handshake_test.go + sqlproxy_failclosed_test.go.

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/stretchr/testify/require"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	sqlproxydialect "github.com/neksur-com/neksur/internal/sqlproxy/dialect"
	"github.com/neksur-com/neksur/internal/tenant"
	"github.com/neksur-com/neksur/tests/testfixture"
)

const sqlProxyTrinoTenant = "d10cd10c-0205-4d11-8a11-111111111111"

// TestSQLProxyTrinoInjection — see file header.
func TestSQLProxyTrinoInjection(t *testing.T) {
	// CR-01 fix: the dialect's structural rewrite is a no-op (appends a
	// SQL comment, does NOT splice into WHERE) and is gated off by
	// default. The Phase 2 integration tests exercise the structural
	// shape only — set the env-gate explicitly so the dialect emits the
	// `/* neksur-policy: */` comment without failing closed.
	t.Setenv("NEKSUR_SQLPROXY_PHASE2_ALLOW_NOOP", "1")
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, sqlProxyTrinoTenant)
	_ = fx.ProvisionEngineRegistry(t, sqlProxyTrinoTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	defer gc.Close()

	// Seed the Table node (LoadCompiledForTable matches
	// (cp:CompiledPolicy)-[:APPLIES_TO]->(t:Table {name, namespace})
	// — the Table MUST exist before UpsertCompiledPolicy MERGEs the
	// APPLIES_TO edge).
	const policyID = "trino-inj-policy"
	const tableName = "orders"
	const tableNS = "ns"
	const trinoVersion = "467" // matches Phase2Fixture.ProvisionEngineRegistry
	seedPolicyOfKind(t, gc, sqlProxyTrinoTenant, policyID,
		`region = 'us-east-1'`, tableName, tableNS, "Policy", "row_filter",
		"ROW_FILTER_GOVERNS")

	// Upsert a CompiledPolicy with status=active for trino. We bypass
	// the cross-engine compiler and call the store directly — this
	// isolates the proxy under test from compiler correctness, which
	// has its own Plan 02-04 coverage.
	tenantUUID := uuid.MustParse(sqlProxyTrinoTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	cstore := store.NewCompiledStore(gc)
	require.NoError(t, cstore.UpsertCompiledPolicy(ctx, store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "trino",
		EngineVersion:  trinoVersion,
		TableName:      tableName,
		TableNamespace: tableNS,
		Status:         store.CompiledPolicyStatusActive,
		SourceChecksum: "deadbeef",
		ArtifactBody:   "WHERE region='us-east-1'",
	}))

	ts, clientCert, caPEM := startSqlproxyTestServer(t, fx)
	defer ts.Close()

	client := newSqlproxyTestClient(t, caPEM, clientCert)

	// One warm-up request to populate the LRU cache (so the subsequent
	// 100 measured requests are cache-hits and the P95 budget reflects
	// the cache-hit hot path the REQ-NFR-latency-sql-proxy budget
	// targets).
	body := mustJSON(t, map[string]any{
		"query": "SELECT * FROM orders",
		"table": map[string]string{"namespace": tableNS, "name": tableName},
		"principal": map[string]any{
			"sub":   "u1",
			"email": "u1@example.com",
			"roles": []string{"analyst"},
		},
	})
	resp, rewritten := doSqlproxyPOST(t, client, ts.URL+"/v1/sql/trino/myprefix/dummy",
		body, sqlProxyTrinoTenant)
	require.Equal(t, http.StatusOK, resp.StatusCode, "warm-up: body=%s", rewritten)
	require.Contains(t, rewritten, "/* neksur-policy: ",
		"rewritten_query must contain structural splice marker")

	// Drive 100 cache-hit requests, collect latencies, assert P95
	// < 50 ms.
	const sampleN = 100
	latencies := make([]time.Duration, 0, sampleN)
	for i := 0; i < sampleN; i++ {
		start := time.Now()
		resp, _ := doSqlproxyPOST(t, client, ts.URL+"/v1/sql/trino/myprefix/dummy",
			body, sqlProxyTrinoTenant)
		latencies = append(latencies, time.Since(start))
		require.Equal(t, http.StatusOK, resp.StatusCode, "iter %d", i)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	// P95 index = ceil(0.95 * N) - 1 = 94 for N=100.
	p95 := latencies[94]
	t.Logf("sqlproxy cache-hit P95 over %d requests: %s", sampleN, p95)
	require.Less(t, p95, 50*time.Millisecond,
		"REQ-NFR-latency-sql-proxy: P95 cache-hit latency budget exceeded")
}

// startSqlproxyTestServer wires the sqlproxy.Server in-process behind
// an httptest TLS listener configured with mTLS-required client auth.
// Returns the server, a client cert signed by the same CA, and the CA
// bundle PEM so the test client can verify the server.
//
// The handler is wrapped in a tenant-injecting middleware that reads
// the tenant ID from an X-Test-Tenant header (production wires the
// real workosauth.TenantMiddleware in cmd/neksur-server/main.go; the
// proxy is the unit under test here, not the auth chain).
func startSqlproxyTestServer(t *testing.T, fx *Phase2Fixture) (*httptest.Server, tls.Certificate, []byte) {
	t.Helper()
	caPEM, srvCertPEM, srvKeyPEM, clientCert := testfixture.IssueCAAndServerAndClient(
		t, "localhost", "test-client")

	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "server.crt")
	keyPath := filepath.Join(tmp, "server.key")
	caPath := filepath.Join(tmp, "ca.crt")
	require.NoError(t, os.WriteFile(certPath, srvCertPEM, 0o600))
	require.NoError(t, os.WriteFile(keyPath, srvKeyPEM, 0o600))
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600))

	cw, err := sqlproxy.NewCertWatcher(certPath, keyPath)
	require.NoError(t, err)
	tlsCfg, err := sqlproxy.NewTLSConfig(cw, caPath)
	require.NoError(t, err)

	// httptest.Server.StartTLS panics if s.TLS.Certificates is empty
	// (it indexes [0] to derive its built-in client). sqlproxy.NewTLSConfig
	// only sets GetCertificate (for hot-reload), so we ALSO load the
	// initial cert pair into Certificates to satisfy httptest. Real
	// handshakes still go through GetCertificate first (Go TLS server
	// preference order), so this does not change the runtime cert source.
	srvBaseline, err := tls.LoadX509KeyPair(certPath, keyPath)
	require.NoError(t, err)
	tlsCfg.Certificates = []tls.Certificate{srvBaseline}

	// Build the in-process Injectors over the live fx.Graph-backed store.
	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	require.NoError(t, err)
	t.Cleanup(gc.Close)
	cache, err := lru.New[sqlproxy.CacheKey, sqlproxy.ArtifactEntry](128)
	require.NoError(t, err)
	injDeps := sqlproxy.InjectorDeps{
		Store: store.NewCompiledStore(gc),
		Cache: cache,
	}
	trinoInj, err := sqlproxydialect.BuildInjector("trino", injDeps)
	require.NoError(t, err)
	sparkInj, err := sqlproxydialect.BuildInjector("spark", injDeps)
	require.NoError(t, err)
	dremioInj, err := sqlproxydialect.BuildInjector("dremio", injDeps)
	require.NoError(t, err)

	srv, err := sqlproxy.NewServer(sqlproxy.Deps{
		Injectors: map[string]sqlproxy.Injector{
			"trino":  trinoInj,
			"spark":  sparkInj,
			"dremio": dremioInj,
		},
		TLSConfig: tlsCfg,
	})
	require.NoError(t, err)

	handler := tenantInjectingMiddleware(srv.Handler())
	ts := httptest.NewUnstartedServer(handler)
	ts.TLS = tlsCfg
	ts.StartTLS()
	return ts, clientCert, caPEM
}

// tenantInjectingMiddleware is the test-only TenantMiddleware shim. It
// pulls the tenant UUID from the X-Test-Tenant header and attaches it
// via tenant.WithID before delegating to the sqlproxy handler.
//
// Production wires workosauth.TenantMiddleware in main.go; the proxy
// itself is the unit under test in this file, so we skip the WorkOS
// session resolution chain.
func tenantInjectingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get("X-Test-Tenant")
		if tid == "" {
			http.Error(w, "X-Test-Tenant header required", http.StatusInternalServerError)
			return
		}
		parsed, err := uuid.Parse(tid)
		if err != nil {
			http.Error(w, "X-Test-Tenant invalid", http.StatusBadRequest)
			return
		}
		ctx := tenant.WithID(r.Context(), parsed)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newSqlproxyTestClient builds an *http.Client that trusts the test CA
// and presents `clientCert` on every TLS handshake.
func newSqlproxyTestClient(t *testing.T, caPEM []byte, clientCert tls.Certificate) *http.Client {
	t.Helper()
	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(caPEM), "CA bundle must contain at least one cert")
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS13,
				RootCAs:      caPool,
				Certificates: []tls.Certificate{clientCert},
			},
		},
	}
}

// doSqlproxyPOST issues a POST against the sqlproxy server with the
// X-Test-Tenant header set. Returns the response + the rewritten_query
// (or the raw error body when status != 200) for assertion convenience.
func doSqlproxyPOST(t *testing.T, client *http.Client, url string, body []byte, tenantID string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Tenant", tenantID)
	resp, err := client.Do(req)
	require.NoError(t, err)
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	if resp.StatusCode == http.StatusOK {
		var env struct {
			Rewritten string `json:"rewritten_query"`
		}
		if err := json.Unmarshal(respBody, &env); err == nil && env.Rewritten != "" {
			return resp, env.Rewritten
		}
	}
	return resp, string(respBody)
}

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
