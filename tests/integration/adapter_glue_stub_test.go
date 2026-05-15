// adapter_glue_stub_test.go — unit-only tests for the Glue stub
// adapter. NO build tag (per Plan 01-02 Task 3 spec) — these run
// on every `go test ./...` invocation; they don't need Docker or
// the Phase1Fixture.
//
// Maps to:
//   - Plan 01-02 Task 3 acceptance ("stubs satisfy the interface +
//     return ErrAdapterStub for state-mutating ops; D-1.02 contract").
//   - REQ-iceberg-rest-adapter-model — proves the IcebergCatalogClient
//     interface is satisfied by the stub (compile + runtime) and
//     callers can branch on errors.Is(err, iceberg.ErrAdapterStub)
//     without a per-catalog runtime sniff.
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg/glue_stub"
)

// TestGlueStubAdapterLoadTableReturnsErrAdapterStub: the
// canonical D-1.02 acceptance — LoadTable on the stub MUST
// return an error wrapping iceberg.ErrAdapterStub. Callers
// detect this via errors.Is and route around the stub
// (typically: refuse to enable Glue in deployment config until
// Phase 3 lands the live adapter).
func TestGlueStubAdapterLoadTableReturnsErrAdapterStub(t *testing.T) {
	t.Parallel()

	cat, err := glue_stub.New(context.Background(), glue_stub.Config{
		Region:    "us-east-1",
		CatalogID: "test",
	})
	if err != nil {
		t.Fatalf("glue_stub.New: want nil error from stub constructor, got %v", err)
	}

	_, err = cat.LoadTable(context.Background(), iceberg.TableRef{
		Namespace: []string{"test"},
		Name:      "orders",
	})
	if err == nil {
		t.Fatal("LoadTable on stub: want error, got nil")
	}
	if !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Fatalf("LoadTable: want errors.Is(ErrAdapterStub), got %v", err)
	}
}

// TestGlueStubCapabilitiesShape asserts the stub publishes
// realistic Glue values so downstream code that branches on
// Capabilities() doesn't silently drift between stub-time and
// Phase 3 live-time behavior. Critical: MaxNamespaceDepth=1
// (Glue databases are flat — no nesting like Polaris's 100).
func TestGlueStubCapabilitiesShape(t *testing.T) {
	t.Parallel()

	cat, err := glue_stub.New(context.Background(), glue_stub.Config{
		Region:    "us-east-1",
		CatalogID: "test",
	})
	if err != nil {
		t.Fatalf("glue_stub.New: %v", err)
	}

	caps := cat.Capabilities()
	if caps.Name != "glue-stub" {
		t.Errorf("Capabilities.Name: want %q, got %q", "glue-stub", caps.Name)
	}
	if caps.SupportsBranches {
		t.Error("Capabilities.SupportsBranches: want false (Glue is non-branching), got true")
	}
	if caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want false (Glue uses IAM directly, not vended), got true")
	}
	if caps.SupportsWebhooks {
		t.Error("Capabilities.SupportsWebhooks: want false (Glue uses CloudWatch Events, not webhooks), got true")
	}
	if caps.MaxNamespaceDepth != 1 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 1 (Glue databases are flat), got %d", caps.MaxNamespaceDepth)
	}
}

// TestGlueStubAdapterCommitTableReturnsErrAdapterStub: same
// shape as the LoadTable test — proves the state-mutating
// CommitTable path also returns the stub sentinel. Without this
// test, a regression that wired CommitTable to a real call
// would slip through the LoadTable-only check.
func TestGlueStubAdapterCommitTableReturnsErrAdapterStub(t *testing.T) {
	t.Parallel()

	cat, err := glue_stub.New(context.Background(), glue_stub.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("glue_stub.New: %v", err)
	}

	_, err = cat.CommitTable(context.Background(), iceberg.TableRef{
		Namespace: []string{"test"},
		Name:      "orders",
	}, iceberg.CommitRequest{})
	if err == nil {
		t.Fatal("CommitTable on stub: want error, got nil")
	}
	if !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Fatalf("CommitTable: want errors.Is(ErrAdapterStub), got %v", err)
	}
}

// TestGlueStubAllStateMutatingMethodsReturnErrAdapterStub
// blanket-verifies every state-mutating method (5 of the 6
// IcebergCatalogClient methods) returns the stub sentinel.
// Capabilities() is the one read-only method and is exercised
// by TestGlueStubCapabilitiesShape above.
//
// This test is the regression guard against a future refactor
// adding a new method to IcebergCatalogClient and forgetting to
// stub it on glueAdapter (which would compile only if the new
// method has a default implementation — which we DON'T want).
func TestGlueStubAllStateMutatingMethodsReturnErrAdapterStub(t *testing.T) {
	t.Parallel()

	cat, err := glue_stub.New(context.Background(), glue_stub.Config{})
	if err != nil {
		t.Fatalf("glue_stub.New: %v", err)
	}
	ref := iceberg.TableRef{Namespace: []string{"test"}, Name: "orders"}
	ctx := context.Background()

	// ListTables
	if _, err := cat.ListTables(ctx, "test"); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("ListTables: want ErrAdapterStub, got %v", err)
	}
	// GetTable
	if _, err := cat.GetTable(ctx, ref); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("GetTable: want ErrAdapterStub, got %v", err)
	}
	// LoadTable (also covered by dedicated test above; redundant for completeness)
	if _, err := cat.LoadTable(ctx, ref); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("LoadTable: want ErrAdapterStub, got %v", err)
	}
	// CommitTable (also covered above)
	if _, err := cat.CommitTable(ctx, ref, iceberg.CommitRequest{}); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("CommitTable: want ErrAdapterStub, got %v", err)
	}
	// ExpireSnapshots
	if err := cat.ExpireSnapshots(ctx, ref, time.Now()); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("ExpireSnapshots: want ErrAdapterStub, got %v", err)
	}
}
