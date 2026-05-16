//go:build integration

// credvend_unity_stub_test.go — validates that the Unity and Glue stub
// adapters return iceberg.ErrAdapterStub on IssueScopedSTSCredentials
// (D-2.09 defense-in-depth on top of the CR-03 boot-time guard).
//
// These tests do NOT require Docker / testcontainers — the stub adapters
// are pure in-process and succeed at construction with any config.
package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/neksur-com/neksur/internal/iceberg"
	gluestub "github.com/neksur-com/neksur/internal/iceberg/glue_stub"
	unitystub "github.com/neksur-com/neksur/internal/iceberg/unity_stub"
)

// TestCredvend_UnityStub asserts that unityStubAdapter.IssueScopedSTSCredentials
// returns errors.Is(err, iceberg.ErrAdapterStub) — the Phase 2 defense-in-depth
// pattern on top of CR-03 boot-time assertNoUnsupportedCatalogs.
//
// Phase 3 replaces the stub with a live Unity STS implementation; at that
// point this test will need to be updated to assert a real creds response
// (or removed if a separate live-unity test covers it).
func TestCredvend_UnityStub(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tableRef := iceberg.TableRef{
		Namespace: []string{"prod"},
		Name:      "orders",
	}
	const region = "us-east-1"

	t.Run("unity_stub_returns_ErrAdapterStub", func(t *testing.T) {
		t.Parallel()

		adapter, err := unitystub.New(ctx, unitystub.Config{})
		if err != nil {
			t.Fatalf("unitystub.New: unexpected error: %v", err)
		}

		creds, err := adapter.IssueScopedSTSCredentials(ctx, tableRef, region)
		if creds != nil {
			t.Errorf("unity_stub: expected nil creds, got %+v", creds)
		}
		if !errors.Is(err, iceberg.ErrAdapterStub) {
			t.Errorf("unity_stub: expected errors.Is(err, iceberg.ErrAdapterStub), got: %v", err)
		}
	})

	t.Run("glue_stub_returns_ErrAdapterStub", func(t *testing.T) {
		t.Parallel()

		adapter, err := gluestub.New(ctx, gluestub.Config{})
		if err != nil {
			t.Fatalf("gluestub.New: unexpected error: %v", err)
		}

		creds, err := adapter.IssueScopedSTSCredentials(ctx, tableRef, region)
		if creds != nil {
			t.Errorf("glue_stub: expected nil creds, got %+v", creds)
		}
		if !errors.Is(err, iceberg.ErrAdapterStub) {
			t.Errorf("glue_stub: expected errors.Is(err, iceberg.ErrAdapterStub), got: %v", err)
		}
	})
}
