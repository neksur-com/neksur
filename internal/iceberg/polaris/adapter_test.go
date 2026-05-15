// Unit tests for the polaris adapter — config validation +
// capabilities shape. The live testcontainer round-trip lives in
// tests/integration/adapter_polaris_test.go behind the
// `integration && polaris` build tags (and exercises the OAuth
// + LoadTable wire path against a real Polaris 1.4.0 container).
//
// These tests run unconditionally on every `go test ./...` —
// they don't require Docker.
package polaris

import (
	"context"
	"errors"
	"testing"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// TestNewValidatesConfig asserts New rejects an empty Config with
// a wrapped ErrInvalidConfig, so callers can branch on errors.Is
// without paying for a network call up front.
func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Config{})
	if err == nil {
		t.Fatal("New(empty Config): want error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(empty Config): want errors.Is(ErrInvalidConfig), got %v", err)
	}
}

// TestNewValidatesEachRequiredField walks the four required fields
// and confirms each one's absence is independently detected. This
// guards against a future refactor accidentally letting one field
// silently default through.
func TestNewValidatesEachRequiredField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing endpoint", Config{Warehouse: "w", ClientID: "c", ClientSecret: "s"}},
		{"missing warehouse", Config{Endpoint: "https://x", ClientID: "c", ClientSecret: "s"}},
		{"missing client id", Config{Endpoint: "https://x", Warehouse: "w", ClientSecret: "s"}},
		{"missing client secret", Config{Endpoint: "https://x", Warehouse: "w", ClientID: "c"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.name)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("%s: want ErrInvalidConfig, got %v", tc.name, err)
			}
		})
	}
}

// TestCapabilitiesShape asserts the static-facts Capabilities()
// returns match the documented Polaris values per RESEARCH lines
// 583-588. The Phase 1 gateway / scheduler branches on these so a
// silent regression here would change cross-engine policy
// enforcement behavior — the assert is BLOCKING.
func TestCapabilitiesShape(t *testing.T) {
	t.Parallel()

	// We can't go through New (it would attempt a real network
	// call), so construct the adapter struct directly. This is the
	// only test that needs to.
	a := &polarisAdapter{}
	caps := a.Capabilities()

	if caps.Name != "polaris" {
		t.Errorf("Capabilities.Name: want %q, got %q", "polaris", caps.Name)
	}
	if caps.SupportsBranches {
		t.Errorf("Capabilities.SupportsBranches: want false (Polaris is non-branching), got true")
	}
	if !caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want true (Polaris STS), got false")
	}
	if !caps.SupportsWebhooks {
		t.Error("Capabilities.SupportsWebhooks: want true (Polaris emits Iceberg events), got false")
	}
	if caps.MaxNamespaceDepth != 100 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 100 (Polaris doc), got %d", caps.MaxNamespaceDepth)
	}
}

// TestAdapterImplementsIcebergCatalogClient is a compile-time
// assertion via interface-conversion. If a future refactor
// removes one of the IcebergCatalogClient methods, this test
// fails to compile — blocking the change with a clear signal.
func TestAdapterImplementsIcebergCatalogClient(t *testing.T) {
	t.Parallel()

	var _ iceberg.IcebergCatalogClient = (*polarisAdapter)(nil)
}
