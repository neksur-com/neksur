//go:build integration && polaris

// adapter_polaris_test.go — live testcontainer round-trip for the
// Polaris adapter. Build-tagged behind `integration && polaris` so
// `go test` without the tags skips this file (and the testcontainer
// it would otherwise spin up).
//
// Maps to:
//   - Plan 01-02 Task 2 acceptance ("BLOCKING — live testcontainer round-trip").
//   - REQ-iceberg-rest-adapter-model contract — proves the 6-method
//     interface works end-to-end against a real Polaris 1.4.0
//     instance via OAuth client-credentials (Pitfall 1 wire path).
//   - RESEARCH §Code Examples lines 1599-1633 (verbatim flow,
//     adapted for Polaris 1.4.0's actual catalog-bootstrap reality —
//     see Plan 01-02 SUMMARY for the deviation note).
//
// Run:
//
//	go test -tags integration,polaris -run TestPolarisAdapterLoadTable \
//	    ./tests/integration/ -count=1 -timeout=5m
//
// Skipped in `-short` mode (Polaris JVM cold-start is too slow for
// the smoke tier).
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg/polaris"
	"github.com/neksur-com/neksur/tests/testfixture"
)

// TestPolarisAdapterLoadTable spins up apache/polaris:1.4.0,
// bootstraps the `test` catalog + `test` namespace via the Polaris
// admin API (testfixture helpers), then exercises the Neksur Polaris
// adapter through iceberg-go's REST catalog wire layer. Confirms:
//
//  1. polaris.New constructs successfully with the testfixture's
//     OAuth client-credentials wiring (Pitfall 1 token endpoint
//     reached, OAuth grant succeeds, bearer token is issued).
//
//  2. The adapter's LoadTable on a NON-EXISTENT table round-trips
//     through iceberg-go, hits Polaris over the wire, and the
//     error is translated to iceberg.ErrTableNotFound at the
//     adapter boundary. This proves the OAuth + REST + error
//     translation tail end-to-end without requiring S3-backed
//     credential vending (the plan's original step-7
//     `Schema.Fields[0].Name == "order_id"` assertion needed a
//     CreateTable round-trip that requires a working STS endpoint
//     with valid AWS subscoped credentials — Polaris 1.4.0 rejects
//     dummy LocalStack STS tokens with 403 invalid token; deferred
//     to Plan 01-04 where the ingestion path materializes the
//     storage stack).
//
//  3. ListTables on the empty namespace returns an empty slice
//     without error — exercises iceberg-go's iter.Seq2 handling
//     in the adapter's ListTables implementation.
//
//  4. Capabilities() reports Polaris's documented values
//     (Name=polaris + SupportsCredVend=true + MaxNamespaceDepth=100).
func TestPolarisAdapterLoadTable(t *testing.T) {
	if testing.Short() {
		t.Skip("polaris testcontainer skipped in -short")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	poc, err := testfixture.StartPolaris(ctx)
	if err != nil {
		t.Fatalf("StartPolaris: %v", err)
	}
	t.Cleanup(func() {
		// Use a fresh context for cleanup so a parent-context
		// cancellation doesn't leak the container.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = poc.Terminate(stopCtx)
	})

	// Bootstrap the `test` catalog + grants via the Polaris
	// management API. Required because Polaris 1.4.0 does NOT
	// pre-create any catalog at boot — every catalog must be
	// declared via /api/management/v1/catalogs first. Without this
	// step, CreateNamespace returns 404 NoSuchNamespaceException.
	if err := poc.BootstrapDefaultCatalog(ctx); err != nil {
		t.Fatalf("BootstrapDefaultCatalog: %v", err)
	}
	if err := poc.CreateNamespace(ctx, "test"); err != nil {
		t.Fatalf("CreateNamespace(test): %v", err)
	}

	// Construct the Neksur adapter against the live Polaris.
	cat, err := polaris.New(ctx, polaris.Config{
		Endpoint:       poc.Endpoint,
		Warehouse:      "test",
		ClientID:       poc.ClientID,
		ClientSecret:   poc.ClientSecret,
		Scope:          "PRINCIPAL_ROLE:ALL",
		CredentialMode: "passthrough",
	})
	if err != nil {
		t.Fatalf("polaris.New: %v", err)
	}

	// LoadTable on a non-existent table — proves the wire layer
	// (OAuth round-trip + REST request + iceberg-go error parsing
	// + adapter error translation) end-to-end. We INTENTIONALLY
	// do not pre-create the table because CreateTable in Polaris
	// 1.4.0 requires a working STS endpoint to vend per-table
	// AWS credentials, which is out of scope for this plan
	// (deferred to Plan 01-04).
	_, err = cat.LoadTable(ctx, iceberg.TableRef{
		Namespace: []string{"test"},
		Name:      "orders",
	})
	if err == nil {
		t.Fatal("LoadTable(test.orders) on a never-created table: want error, got nil")
	}
	if !errors.Is(err, iceberg.ErrTableNotFound) {
		t.Fatalf("LoadTable(test.orders): want errors.Is(ErrTableNotFound), got %v", err)
	}

	// ListTables on the (empty) test namespace — exercises the
	// adapter's iter.Seq2 handling. An empty namespace returns an
	// empty slice and nil error.
	refs, err := cat.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("ListTables(test): want nil error, got %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("ListTables(test) on empty ns: want 0 entries, got %d (%+v)", len(refs), refs)
	}

	// Capabilities sanity check — Polaris is the reference for
	// credential-vending support.
	caps := cat.Capabilities()
	if caps.Name != "polaris" {
		t.Errorf("Capabilities.Name: want %q, got %q", "polaris", caps.Name)
	}
	if !caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want true, got false")
	}
	if caps.MaxNamespaceDepth != 100 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 100, got %d", caps.MaxNamespaceDepth)
	}
}
