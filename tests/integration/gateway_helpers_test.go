//go:build integration

// Plan 01-06 Task 3 — shared test helpers for the gateway BLOCKING tests.
//
// Helpers here are package-internal so each gateway test file can wire
// the Phase1Fixture-backed handler stack without re-implementing the
// boilerplate.
//
// Why a fake adapter:
//   The polaris adapter's CommitTable cannot accept non-empty
//   Requirements/Updates in Phase 1 (Plan 01-02 deviation: iceberg-go
//   v0.5 lacks public ParseUpdate; typed dispatcher deferred). The
//   tests need to prove the gateway pipeline runs end-to-end, NOT the
//   typed-update conversion (which Plan 01-04 / Plan 02 will own when
//   STS infra is ready). We inject a fakeIcebergAdapter that records
//   every call so the test can assert (a) gateway forwarded, (b) the
//   right ref / commit reached the adapter, (c) the result echoed back
//   to the client.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	workosauth "github.com/neksur-com/neksur/internal/auth/workos"
	"github.com/neksur-com/neksur/internal/catalog"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/ingest"
	celpolicy "github.com/neksur-com/neksur/internal/policy/cel"
	policystore "github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

// gatewayHarness wires the L1 gateway against a Phase1Fixture-backed
// substrate plus a fakeIcebergAdapter (Plan 01-06 Task 3 — Polaris
// CreateTable + STS deferred).
type gatewayHarness struct {
	t          *testing.T
	fx         *Phase1Fixture
	tenantUUID uuid.UUID
	tenantStr  string
	gc         *graph.GraphClient
	deps       iceberggw.Deps
	fake       *fakeIcebergAdapter
	srv        *httptest.Server
}

// fakeIcebergAdapter is a recording stub that the gateway forwards to
// instead of the live polaris/nessie adapter. CommitTable returns a
// canned successful result so the gateway's downstream emission paths
// (audit + ingest + echo) all execute. Configure FailWith to simulate
// upstream errors.
type fakeIcebergAdapter struct {
	mu sync.Mutex

	// Static returns:
	Tables          map[string]*iceberg.TableMetadata // key = ref.Name (single-namespace tests)
	NextResult      iceberg.CommitResult
	FailLoad        error
	FailCommit      error
	CommitsReceived []recordedCommit
	LoadsReceived   []iceberg.TableRef
}

type recordedCommit struct {
	Ref iceberg.TableRef
	Req iceberg.CommitRequest
}

func newFakeAdapter() *fakeIcebergAdapter {
	return &fakeIcebergAdapter{
		Tables: make(map[string]*iceberg.TableMetadata),
		NextResult: iceberg.CommitResult{
			NewMetadataLocation: "s3://test-bucket/orders/metadata/00099-uuid.metadata.json",
			NewSnapshotID:       9099,
			AcceptedAt:          time.Now().UTC(),
		},
	}
}

func (f *fakeIcebergAdapter) ListTables(_ context.Context, _ string) ([]iceberg.TableRef, error) {
	return nil, nil
}

func (f *fakeIcebergAdapter) GetTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return f.LoadTable(ctx, ref)
}

func (f *fakeIcebergAdapter) LoadTable(_ context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LoadsReceived = append(f.LoadsReceived, ref)
	if f.FailLoad != nil {
		return nil, f.FailLoad
	}
	if t, ok := f.Tables[ref.Name]; ok {
		return t, nil
	}
	// Default table: orders schema with order_id + customer_email.
	return &iceberg.TableMetadata{
		UUID:             "00000000-0000-0000-0000-" + ref.Name,
		MetadataLocation: "s3://test-bucket/" + ref.Name + "/metadata/00001-uuid.metadata.json",
		Schema: iceberg.Schema{Fields: []iceberg.SchemaField{
			{ID: 1, Name: "order_id", Type: "long", Required: true},
			{ID: 2, Name: "customer_email", Type: "string"},
		}},
	}, nil
}

func (f *fakeIcebergAdapter) CommitTable(_ context.Context, ref iceberg.TableRef, req iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CommitsReceived = append(f.CommitsReceived, recordedCommit{Ref: ref, Req: req})
	if f.FailCommit != nil {
		return nil, f.FailCommit
	}
	r := f.NextResult
	return &r, nil
}

func (f *fakeIcebergAdapter) ExpireSnapshots(_ context.Context, _ iceberg.TableRef, _ time.Time) error {
	return errors.New("fake: not used in tests")
}

func (f *fakeIcebergAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{Name: "fake", MaxNamespaceDepth: 1}
}

// startGatewayHarness boots a Phase1Fixture, provisions a tenant, and
// wires the gateway handlers against a fake adapter. Returns a fully-
// constructed harness; callers MUST defer h.terminate() via t.Cleanup
// inside the test.
//
// The httptest server is started here; tests POST to h.srv.URL +
// "/v1/iceberg/{prefix}/namespaces/{ns}/tables/{table}".
func startGatewayHarness(t *testing.T, tenantStr string) *gatewayHarness {
	t.Helper()
	fx := StartPhase1Fixture(t)
	t.Cleanup(fx.Terminate)

	tenantUUID := uuid.MustParse(tenantStr)
	_ = fx.ProvisionTenant(t, tenantStr)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	t.Cleanup(gc.Close)

	pool := newTenantPool(t, fx.Container.SuperuserDSN)
	t.Cleanup(pool.Close)

	celEnv, err := celpolicy.NewEnv()
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	celCompiler, err := celpolicy.NewCompiler(celEnv, 16)
	if err != nil {
		t.Fatalf("cel.NewCompiler: %v", err)
	}

	fake := newFakeAdapter()

	deps := iceberggw.Deps{
		Pool:        pool,
		Graph:       gc,
		CredStore:   catalog.NewRepo(pool),
		PolicyStore: policystore.NewAGEStore(gc),
		Evaluator:   celpolicy.NewEvaluator(celCompiler),
		IngestSvc:   ingest.NewService(gc),
		AdapterFactory: func(_ context.Context, _ *catalog.Credentials) (iceberg.IcebergCatalogClient, error) {
			return fake, nil
		},
	}

	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.Handler {
		// Tenant injection shim — TenantMiddleware is exercised by
		// workos_session_test.go; we attach the tenant directly here.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.WithID(r.Context(), tenantUUID)
			h(w, r.WithContext(ctx))
		})
	}
	mux.Handle("POST /v1/iceberg/{prefix}/namespaces/{namespace}/tables/{table}",
		wrap(iceberggw.CommitHandler(deps)))
	mux.Handle("POST /v1/iceberg/{prefix}/transactions/commit",
		wrap(iceberggw.MultiTableCommitHandler(deps)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Compile-time guard: workosauth.TenantMiddleware is the production
	// shape — the harness uses a hand-rolled shim so we don't pull
	// WorkOS / WorkOSMock here. workos_session_test.go covers the full
	// middleware path.
	var _ = workosauth.TenantMiddleware

	return &gatewayHarness{
		t: t, fx: fx, tenantUUID: tenantUUID, tenantStr: tenantStr,
		gc: gc, deps: deps, fake: fake, srv: srv,
	}
}

// postCommit POSTs body to /v1/iceberg/{prefix}/namespaces/{ns}/tables/{table}
// and returns (status, body).
func (h *gatewayHarness) postCommit(t *testing.T, prefix, ns, table string, body []byte) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/iceberg/%s/namespaces/%s/tables/%s",
		h.srv.URL, prefix, ns, table)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// postCommitWithBearer adds an Authorization: Bearer <token> header.
func (h *gatewayHarness) postCommitWithBearer(t *testing.T, prefix, ns, table string, body []byte, bearer string) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/iceberg/%s/namespaces/%s/tables/%s",
		h.srv.URL, prefix, ns, table)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// postMultiTable POSTs to /v1/iceberg/{prefix}/transactions/commit.
func (h *gatewayHarness) postMultiTable(t *testing.T, prefix string, body []byte) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/iceberg/%s/transactions/commit", h.srv.URL, prefix)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// validCommitBody returns a minimal-but-valid CommitRequest JSON body.
// Empty Requirements + one set-properties Update — keeps the wire body
// non-trivial without depending on the typed-dispatch path.
func validCommitBody() []byte {
	return []byte(`{"requirements":[],"updates":[{"action":"set-properties","updates":{"x":"y"}}]}`)
}

// queryAuditLogDecision SELECTs the most recent decision /
// principal_source / commit_request_hash for the calling tenant.
// Returns ("", "", nil) if no row.
func queryAuditLogDecision(t *testing.T, ctx context.Context, h *gatewayHarness) (string, string, []byte) {
	t.Helper()
	tctx := tenant.WithID(ctx, h.tenantUUID)
	var decision, source string
	var hashBytes []byte
	err := tenant.WithTenantTx(tctx, h.deps.Pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT decision, principal_source, commit_request_hash
			   FROM audit_log
			  WHERE event_type = 'iceberg_commit'
			  ORDER BY occurred_at DESC LIMIT 1`)
		return row.Scan(&decision, &source, &hashBytes)
	})
	if err != nil {
		// pgx.ErrNoRows is acceptable for callers that want "is empty"
		t.Logf("queryAuditLogDecision: %v", err)
		return "", "", nil
	}
	return decision, source, hashBytes
}

// extractDecodedResult decodes the gateway's 200 response into
// iceberg.CommitResult. Returns nil if body is unparseable.
func extractDecodedResult(body string) *iceberg.CommitResult {
	var r iceberg.CommitResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return nil
	}
	return &r
}
