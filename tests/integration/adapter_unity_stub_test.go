// adapter_unity_stub_test.go — unit-only tests for the Unity
// stub adapter (mirror of adapter_glue_stub_test.go). NO build
// tag — runs on every `go test ./...`.
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg/unity_stub"
)

// TestUnityStubAdapterLoadTableReturnsErrAdapterStub: D-1.02
// canonical acceptance — LoadTable on the stub MUST return an
// error wrapping iceberg.ErrAdapterStub so callers can branch
// via errors.Is.
func TestUnityStubAdapterLoadTableReturnsErrAdapterStub(t *testing.T) {
	t.Parallel()

	cat, err := unity_stub.New(context.Background(), unity_stub.Config{
		WorkspaceURL: "https://acme.cloud.databricks.com",
		AccessToken:  "test-token",
	})
	if err != nil {
		t.Fatalf("unity_stub.New: want nil error from stub constructor, got %v", err)
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

// TestUnityStubCapabilitiesShape asserts the stub publishes
// realistic Unity values. Critical: SupportsCredVend=true (Unity
// vends per-table STS-style credentials) — the only bool true on
// Unity stub Capabilities. Without this realistic shape, Plan
// 01-06 / Phase 3 code that special-cases credential-vending
// catalogs would silently drift between stub-time and live-time.
func TestUnityStubCapabilitiesShape(t *testing.T) {
	t.Parallel()

	cat, err := unity_stub.New(context.Background(), unity_stub.Config{
		WorkspaceURL: "https://acme.cloud.databricks.com",
		AccessToken:  "test-token",
	})
	if err != nil {
		t.Fatalf("unity_stub.New: %v", err)
	}

	caps := cat.Capabilities()
	if caps.Name != "unity-stub" {
		t.Errorf("Capabilities.Name: want %q, got %q", "unity-stub", caps.Name)
	}
	if caps.SupportsBranches {
		t.Error("Capabilities.SupportsBranches: want false (Unity is non-branching), got true")
	}
	// The single true cap on Unity Phase 3 — credential vending
	// is THE Unity differentiator vs Glue.
	if !caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want true (Unity vends per-table STS), got false")
	}
	if !caps.SupportsWebhooks {
		t.Error("Capabilities.SupportsWebhooks: want true (Unity emits Iceberg events via subscriptions), got false")
	}
	if caps.MaxNamespaceDepth != 1 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 1 (Unity exposes flat ns through Iceberg REST), got %d", caps.MaxNamespaceDepth)
	}
}

// TestUnityStubAdapterCommitTableReturnsErrAdapterStub mirror of
// the Glue test — the state-mutating CommitTable path also
// returns the stub sentinel.
func TestUnityStubAdapterCommitTableReturnsErrAdapterStub(t *testing.T) {
	t.Parallel()

	cat, err := unity_stub.New(context.Background(), unity_stub.Config{
		WorkspaceURL: "https://acme.cloud.databricks.com",
	})
	if err != nil {
		t.Fatalf("unity_stub.New: %v", err)
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

// TestUnityStubAllStateMutatingMethodsReturnErrAdapterStub
// blanket-verifies every state-mutating method returns the
// stub sentinel — same regression guard as the Glue version.
func TestUnityStubAllStateMutatingMethodsReturnErrAdapterStub(t *testing.T) {
	t.Parallel()

	cat, err := unity_stub.New(context.Background(), unity_stub.Config{})
	if err != nil {
		t.Fatalf("unity_stub.New: %v", err)
	}
	ref := iceberg.TableRef{Namespace: []string{"test"}, Name: "orders"}
	ctx := context.Background()

	if _, err := cat.ListTables(ctx, "test"); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("ListTables: want ErrAdapterStub, got %v", err)
	}
	if _, err := cat.GetTable(ctx, ref); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("GetTable: want ErrAdapterStub, got %v", err)
	}
	if _, err := cat.LoadTable(ctx, ref); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("LoadTable: want ErrAdapterStub, got %v", err)
	}
	if _, err := cat.CommitTable(ctx, ref, iceberg.CommitRequest{}); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("CommitTable: want ErrAdapterStub, got %v", err)
	}
	if err := cat.ExpireSnapshots(ctx, ref, time.Now()); !errors.Is(err, iceberg.ErrAdapterStub) {
		t.Errorf("ExpireSnapshots: want ErrAdapterStub, got %v", err)
	}
}
