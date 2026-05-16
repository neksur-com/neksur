//go:build integration

// credvend_handler_status_test.go — Plan 02-10 WR-A2 regression coverage.
//
// Validates that credvend.Handler distinguishes the two Service.Issue
// failure sentinels at the HTTP layer:
//
//   - Service.Issue → credvend.ErrEngineNotSupported → HTTP 501
//     (configuration drift; warn-not-page).
//   - Service.Issue → credvend.ErrCredVendUnavailable → HTTP 503
//     (vending path broken; page).
//
// The earlier blanket-503 mapping collapsed both into a single channel,
// hiding the distinction operators need for triage; iteration-2 review
// WR-A2 documented the drift and this plan (02-10) closes it.
//
// Pattern: mirrors tests/integration/gateway_503_unavailable_test.go's
// in-process handler-test shape. Uses Phase1Fixture to satisfy the
// handler's CredStore lookup (real DB-backed pool with the seeded
// prod-polaris row); injects a stub AdapterBuilder so the test
// controls the Service.Issue failure mode without needing a live
// Polaris testcontainer.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/neksur-com/neksur/internal/catalog"
	"github.com/neksur-com/neksur/internal/credvend"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"
)

// Test tenants — distinct UUIDs to avoid cross-test interference inside
// Phase1Fixture's shared pool.
const (
	credvendHandlerStatusTenant501 = "10000010-0010-4010-8010-000000000501"
	credvendHandlerStatusTenant503 = "10000010-0010-4010-8010-000000000503"
)

// TestCredvendHandler_EngineNotSupported_Returns501 asserts the handler
// returns 501 when Service.Issue returns credvend.ErrEngineNotSupported.
//
// The Service maps the wrapped iceberg.ErrAdapterStub from the adapter's
// IssueScopedSTSCredentials to ErrEngineNotSupported (see service.go).
// We exercise that wrapper rather than constructing a sentinel directly
// so the test covers the production error-propagation path.
func TestCredvendHandler_EngineNotSupported_Returns501(t *testing.T) {
	fx := StartPhase1Fixture(t)
	t.Cleanup(fx.Terminate)
	_ = fx.ProvisionTenant(t, credvendHandlerStatusTenant501)

	pool := newTenantPool(t, fx.Container.SuperuserDSN)
	t.Cleanup(pool.Close)

	// Stub adapter: returns iceberg.ErrAdapterStub from IssueScopedSTSCredentials,
	// which credvend.Service.Issue maps to ErrEngineNotSupported.
	stubAdapter := &credvendHandlerStubAdapter{
		name:             "polaris", // matches the prod-polaris row seeded by Phase1Fixture
		supportsCredVend: true,      // ensures Service.Issue calls IssueScopedSTSCredentials
		issueErr:         iceberg.ErrAdapterStub,
	}

	resp := postCredvendIssueSTS(t, pool, credvendHandlerStatusTenant501, stubAdapter)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501 (NotImplemented). body=%s", resp.StatusCode, resp.Body)
	}
	if !bytes.Contains([]byte(resp.Body), []byte("engine not supported")) {
		t.Errorf("body should contain 'engine not supported'; got: %s", resp.Body)
	}
}

// TestCredvendHandler_CredVendUnavailable_Returns503 asserts the handler
// returns 503 when Service.Issue returns credvend.ErrCredVendUnavailable.
//
// Mechanism: stub adapter returns a generic non-stub error;
// credvend.Service wraps any non-ErrAdapterStub upstream error as
// ErrCredVendUnavailable (fail-closed contract — see service.go).
func TestCredvendHandler_CredVendUnavailable_Returns503(t *testing.T) {
	fx := StartPhase1Fixture(t)
	t.Cleanup(fx.Terminate)
	_ = fx.ProvisionTenant(t, credvendHandlerStatusTenant503)

	pool := newTenantPool(t, fx.Container.SuperuserDSN)
	t.Cleanup(pool.Close)

	stubAdapter := &credvendHandlerStubAdapter{
		name:             "polaris",
		supportsCredVend: true,
		issueErr:         errors.New("polaris: timeout reaching STS endpoint"),
	}

	resp := postCredvendIssueSTS(t, pool, credvendHandlerStatusTenant503, stubAdapter)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (ServiceUnavailable). body=%s", resp.StatusCode, resp.Body)
	}
	if !bytes.Contains([]byte(resp.Body), []byte("credential vending unavailable")) {
		t.Errorf("body should contain 'credential vending unavailable'; got: %s", resp.Body)
	}
}

// credvendHandlerResponse captures HTTP response state for assertions.
type credvendHandlerResponse struct {
	StatusCode int
	Body       string
}

// postCredvendIssueSTS wires the credvend.Handler with the stub adapter,
// attaches a tenant context (matching the workosauth.TenantMiddleware
// shape used in production), and POSTs a valid /v1/credvend/sts request
// body. Returns the captured status code + body.
func postCredvendIssueSTS(t *testing.T, pool *pgxpool.Pool, tenantStr string, stub *credvendHandlerStubAdapter) credvendHandlerResponse {
	t.Helper()

	tenantUUID := uuid.MustParse(tenantStr)

	// Phase1Fixture seeded a prod-polaris row for this tenant during
	// ProvisionTenant; the handler will find it via the real CredStore.
	credStore := catalog.NewRepo(pool)

	cache, err := credvend.NewCache(0)
	if err != nil {
		t.Fatalf("credvend.NewCache: %v", err)
	}
	// Use fresh local counters to avoid cross-test contamination on the
	// shared default registry.
	issued := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_credvend_handler_status_issued_total",
		Help: "test counter",
	}, []string{"engine", "region"})
	refresh := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_credvend_handler_status_refresh_total",
		Help: "test counter",
	}, []string{"engine"})
	svc := credvend.NewService(cache, issued, refresh)

	handler := credvend.Handler(credvend.Deps{
		Service:   svc,
		CredStore: credStore,
		AdapterBuilder: func(_ context.Context, _ *catalog.Credentials) (iceberg.IcebergCatalogClient, error) {
			return stub, nil
		},
	})

	body, _ := json.Marshal(map[string]any{
		"catalog_nickname": "prod-polaris",
		"table_namespace":  "test",
		"table_name":       "orders",
		"region":           "us-east-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/credvend/sts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Attach tenant ctx (production wires this via workosauth.TenantMiddleware).
	req = req.WithContext(tenant.WithID(req.Context(), tenantUUID))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	rb, _ := io.ReadAll(rr.Body)
	return credvendHandlerResponse{
		StatusCode: rr.Code,
		Body:       string(rb),
	}
}

// credvendHandlerStubAdapter is a minimal iceberg.IcebergCatalogClient
// stub configured per-test with a target IssueScopedSTSCredentials error.
// Mirrors the failingCredVendAdapter shape from credvend_attempt_bypass_test.go,
// extended with supportsCredVend so we can exercise the
// SupportsCredVend=true → IssueScopedSTSCredentials → error mapping path.
type credvendHandlerStubAdapter struct {
	name             string
	supportsCredVend bool
	issueErr         error
}

func (a *credvendHandlerStubAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:             a.name,
		SupportsCredVend: a.supportsCredVend,
	}
}

func (a *credvendHandlerStubAdapter) GetTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return nil, a.issueErr
}

func (a *credvendHandlerStubAdapter) LoadTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return nil, a.issueErr
}

func (a *credvendHandlerStubAdapter) ListTables(_ context.Context, _ string) ([]iceberg.TableRef, error) {
	return nil, a.issueErr
}

func (a *credvendHandlerStubAdapter) CommitTable(_ context.Context, _ iceberg.TableRef, _ iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	return nil, a.issueErr
}

func (a *credvendHandlerStubAdapter) ExpireSnapshots(_ context.Context, _ iceberg.TableRef, _ time.Time) error {
	return a.issueErr
}

func (a *credvendHandlerStubAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	return nil, a.issueErr
}

// Compile-time assertion.
var _ iceberg.IcebergCatalogClient = (*credvendHandlerStubAdapter)(nil)
