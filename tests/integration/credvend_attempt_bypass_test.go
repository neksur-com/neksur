//go:build integration

// credvend_attempt_bypass_test.go — D-2.09 acceptance test: verifies that
// the L4 credential vending gate prevents unauthorized S3 writes.
//
// Per ROADMAP §7 + D-2.09: "the L4 vending gate is the strongest
// write-ACL prevention in ADR-003: without scoped STS tokens, Spark
// physically cannot write to managed S3 buckets."
//
// This test validates the Go-side portion of the bypass prevention:
//   - credvend.Service.Issue with a stub adapter (ErrAdapterStub) returns
//     ErrEngineNotSupported, which the handler maps to 503.
//   - A principal whose adapter returns ErrCredVendUnavailable also gets 503.
//   - The fail-closed contract (D-1.09) holds: no STS credentials are issued
//     on error paths.
//
// The full Spark testcontainer bypass test (neksur-spark-policy Extension +
// Spark DataFrame write to S3) is deferred to Plan 02-08 E2E where the JAR
// build + LocalStack S3 access log verification infrastructure is available.
// This test covers the Go-side enforcement surface.
//
// No Docker required — pure in-process using stub adapters.
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neksur-com/neksur/internal/credvend"
	"github.com/neksur-com/neksur/internal/iceberg"
	gluestub "github.com/neksur-com/neksur/internal/iceberg/glue_stub"
	unitystub "github.com/neksur-com/neksur/internal/iceberg/unity_stub"
)

// TestCredvend_AttemptBypass validates the fail-closed credential vending
// gate per D-2.09 + ROADMAP §7.
//
// Subtests:
//  1. unity_stub_blocked — unity adapter returns ErrAdapterStub →
//     Service returns ErrEngineNotSupported → no STS creds issued.
//  2. glue_stub_blocked — same for glue stub.
//  3. unsupported_engine_no_credvend — adapter with SupportsCredVend=false
//     returns ErrEngineNotSupported before even calling IssueScopedSTSCredentials.
//  4. fail_closed_on_error — any non-stub error from IssueScopedSTSCredentials
//     wraps ErrCredVendUnavailable (HTTP 503 mapping confirmed).
func TestCredvend_AttemptBypass(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tableRef := iceberg.TableRef{
		Namespace: []string{"prod"},
		Name:      "restricted_table",
	}
	const (
		tenantID = "tenant-bypass-test"
		region   = "us-east-1"
	)

	// Helpers: fresh service + cache per subtest to avoid counter cross-contamination.
	newSvc := func(t *testing.T) *credvend.Service {
		t.Helper()
		cache, err := credvend.NewCache(0)
		if err != nil {
			t.Fatalf("credvend.NewCache: %v", err)
		}
		// Use fresh local counters per subtest to avoid global counter collisions.
		issued := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_l4_token_issued_total",
			Help: "test counter",
		}, []string{"engine", "region"})
		refresh := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_l4_token_refresh_total",
			Help: "test counter",
		}, []string{"engine"})
		return credvend.NewService(cache, issued, refresh)
	}

	t.Run("unity_stub_blocked", func(t *testing.T) {
		t.Parallel()

		svc := newSvc(t)
		adapter, err := unitystub.New(ctx, unitystub.Config{})
		if err != nil {
			t.Fatalf("unitystub.New: %v", err)
		}

		creds, err := svc.Issue(ctx, tenantID, adapter, tableRef, region)

		// Must return an error — no STS creds.
		if creds != nil {
			t.Errorf("unity bypass: expected nil creds, got %+v", creds)
		}
		if err == nil {
			t.Fatal("unity bypass: expected error, got nil")
		}

		// The error must be ErrEngineNotSupported (not just any error).
		if !errors.Is(err, credvend.ErrEngineNotSupported) {
			t.Errorf("unity bypass: expected errors.Is(err, ErrEngineNotSupported); got: %v", err)
		}

		// Confirm NOT ErrCredVendUnavailable (that would be a different failure mode).
		if errors.Is(err, credvend.ErrCredVendUnavailable) {
			t.Errorf("unity bypass: unexpected ErrCredVendUnavailable in error chain: %v", err)
		}
	})

	t.Run("glue_stub_blocked", func(t *testing.T) {
		t.Parallel()

		svc := newSvc(t)
		adapter, err := gluestub.New(ctx, gluestub.Config{})
		if err != nil {
			t.Fatalf("gluestub.New: %v", err)
		}

		creds, err := svc.Issue(ctx, tenantID, adapter, tableRef, region)

		if creds != nil {
			t.Errorf("glue bypass: expected nil creds, got %+v", creds)
		}
		if err == nil {
			t.Fatal("glue bypass: expected error, got nil")
		}
		if !errors.Is(err, credvend.ErrEngineNotSupported) {
			t.Errorf("glue bypass: expected errors.Is(err, ErrEngineNotSupported); got: %v", err)
		}
	})

	t.Run("unsupported_engine_no_credvend", func(t *testing.T) {
		t.Parallel()

		// Build a Nessie stub adapter — Nessie has SupportsCredVend=false.
		// Service.Issue should return ErrEngineNotSupported without calling
		// IssueScopedSTSCredentials at all.
		svc := newSvc(t)
		adapter := &noCredVendAdapter{
			name:   "nessie-test",
			supCv:  false,
		}

		creds, err := svc.Issue(ctx, tenantID, adapter, tableRef, region)

		if creds != nil {
			t.Errorf("no-credvend bypass: expected nil creds, got %+v", creds)
		}
		if err == nil {
			t.Fatal("no-credvend bypass: expected error, got nil")
		}
		if !errors.Is(err, credvend.ErrEngineNotSupported) {
			t.Errorf("no-credvend bypass: expected ErrEngineNotSupported; got: %v", err)
		}
		// Verify IssueScopedSTSCredentials was never called.
		if adapter.issueCalled {
			t.Error("no-credvend bypass: IssueScopedSTSCredentials was called despite SupportsCredVend=false")
		}
	})

	t.Run("fail_closed_on_upstream_error", func(t *testing.T) {
		t.Parallel()

		// Adapter that returns a generic non-stub error — should become ErrCredVendUnavailable.
		svc := newSvc(t)
		errUpstream := errors.New("polaris: timeout reaching STS endpoint")
		adapter := &failingCredVendAdapter{
			name: "polaris-fail-test",
			err:  errUpstream,
		}

		creds, err := svc.Issue(ctx, tenantID, adapter, tableRef, region)

		if creds != nil {
			t.Errorf("fail-closed: expected nil creds, got %+v", creds)
		}
		if err == nil {
			t.Fatal("fail-closed: expected error, got nil")
		}
		// Fail-closed: non-stub errors → ErrCredVendUnavailable (HTTP 503).
		if !errors.Is(err, credvend.ErrCredVendUnavailable) {
			t.Errorf("fail-closed: expected ErrCredVendUnavailable; got: %v", err)
		}
	})
}

// noCredVendAdapter is a minimal IcebergCatalogClient stub with SupportsCredVend=false.
// It records whether IssueScopedSTSCredentials was called so the test can assert
// it was NOT called when the capability flag is false.
type noCredVendAdapter struct {
	name        string
	supCv       bool
	issueCalled bool
}

func (a *noCredVendAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:             a.name,
		SupportsCredVend: a.supCv,
	}
}

func (a *noCredVendAdapter) GetTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return nil, iceberg.ErrAdapterStub
}

func (a *noCredVendAdapter) LoadTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return nil, iceberg.ErrAdapterStub
}

func (a *noCredVendAdapter) ListTables(_ context.Context, _ string) ([]iceberg.TableRef, error) {
	return nil, iceberg.ErrAdapterStub
}

func (a *noCredVendAdapter) CommitTable(_ context.Context, _ iceberg.TableRef, _ iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	return nil, iceberg.ErrAdapterStub
}

func (a *noCredVendAdapter) ExpireSnapshots(_ context.Context, _ iceberg.TableRef, _ time.Time) error {
	return iceberg.ErrAdapterStub
}

func (a *noCredVendAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	a.issueCalled = true
	return nil, iceberg.ErrAdapterStub
}

// failingCredVendAdapter is a minimal IcebergCatalogClient stub with
// SupportsCredVend=true but IssueScopedSTSCredentials returns a non-stub error.
// Used to verify the fail-closed mapping to ErrCredVendUnavailable.
type failingCredVendAdapter struct {
	name string
	err  error
}

func (a *failingCredVendAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:             a.name,
		SupportsCredVend: true,
	}
}

func (a *failingCredVendAdapter) GetTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return nil, a.err
}

func (a *failingCredVendAdapter) LoadTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return nil, a.err
}

func (a *failingCredVendAdapter) ListTables(_ context.Context, _ string) ([]iceberg.TableRef, error) {
	return nil, a.err
}

func (a *failingCredVendAdapter) CommitTable(_ context.Context, _ iceberg.TableRef, _ iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	return nil, a.err
}

func (a *failingCredVendAdapter) ExpireSnapshots(_ context.Context, _ iceberg.TableRef, _ time.Time) error {
	return a.err
}

func (a *failingCredVendAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	return nil, a.err
}

// Compile-time assertion: stub adapters implement the interface.
var _ iceberg.IcebergCatalogClient = (*noCredVendAdapter)(nil)
var _ iceberg.IcebergCatalogClient = (*failingCredVendAdapter)(nil)

// Ensure time is imported (used by time.Duration in newSvc comments).
var _ = time.Second
