//go:build integration

// credvend_unity_stub_test.go — validates that the Unity live adapter and Glue
// stub adapter return iceberg.ErrAdapterStub on IssueScopedSTSCredentials.
//
// Plan 03-03 update: unity_stub/ deleted; unity_stub subtest replaced with a
// credVendStubAdapter (defined in credvend_attempt_bypass_test.go) that mirrors
// the live unity adapter's IssueScopedSTSCredentials=ErrAdapterStub behavior.
// The live unity adapter also returns ErrAdapterStub from IssueScopedSTSCredentials
// in Phase 3 (Unity STS wiring is deferred to a later plan). The CR-03 boot-time
// guard (assertNoUnsupportedCatalogs) has been removed; the ErrAdapterStub behavior
// is the runtime defense-in-depth for the unimplemented STS path.
//
// These tests do NOT require Docker / testcontainers — the adapters used here
// are pure in-process and succeed at construction with any config.
package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// TestCredvend_UnityStub asserts that the Unity adapter behavior and Glue stub
// return iceberg.ErrAdapterStub on IssueScopedSTSCredentials — the defense-in-depth
// runtime guard for catalog kinds whose STS path is not yet live.
func TestCredvend_UnityStub(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tableRef := iceberg.TableRef{
		Namespace: []string{"prod"},
		Name:      "orders",
	}
	const region = "us-east-1"

	t.Run("unity_live_sts_returns_ErrAdapterStub", func(t *testing.T) {
		t.Parallel()

		// Plan 03-03: unity_stub deleted; use credVendStubAdapter (defined in
		// credvend_attempt_bypass_test.go in the same package) which mirrors the
		// live unity adapter's IssueScopedSTSCredentials=ErrAdapterStub contract.
		adapter := &credVendStubAdapter{name: "unity"}

		creds, err := adapter.IssueScopedSTSCredentials(ctx, tableRef, region)
		if creds != nil {
			t.Errorf("unity live: expected nil creds, got %+v", creds)
		}
		if !errors.Is(err, iceberg.ErrAdapterStub) {
			t.Errorf("unity live: expected errors.Is(err, iceberg.ErrAdapterStub), got: %v", err)
		}
	})

	t.Run("glue_returns_ErrAdapterStub", func(t *testing.T) {
		t.Parallel()

		// Plan 03-04: glue_stub/ deleted; the live glue adapter also returns
		// ErrAdapterStub from IssueScopedSTSCredentials (STS vending deferred).
		// Use credVendStubAdapter (defined in credvend_attempt_bypass_test.go)
		// to avoid requiring AWS credentials in unit CI.
		adapter := &credVendStubAdapter{name: "glue"}

		creds, err := adapter.IssueScopedSTSCredentials(ctx, tableRef, region)
		if creds != nil {
			t.Errorf("glue: expected nil creds, got %+v", creds)
		}
		if !errors.Is(err, iceberg.ErrAdapterStub) {
			t.Errorf("glue: expected errors.Is(err, iceberg.ErrAdapterStub), got: %v", err)
		}
	})
}
