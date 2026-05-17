// Unit tests for the Glue adapter — config validation + capabilities shape.
// The live testcontainer round-trip lives in
// tests/integration/adapter_glue_test.go behind the `integration` build tag.
//
// These tests run unconditionally on every `go test ./...` —
// they don't require Docker or AWS credentials.
package glue

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

// TestNewValidatesEachRequiredField walks the required fields and confirms
// each one's absence is independently detected.
func TestNewValidatesEachRequiredField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing region", Config{CatalogID: "123456789012"}},
		{"missing catalog id", Config{Region: "us-east-1"}},
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
// returns match the documented Glue values per plan 03-04 must_haves §4.
// The Phase 1 gateway / scheduler branches on these so a silent regression
// here would change cross-engine policy enforcement behavior — the assert
// is BLOCKING.
func TestCapabilitiesShape(t *testing.T) {
	t.Parallel()

	// Construct the adapter struct directly since we can't call New
	// (it would attempt AWS credential loading + a real network call).
	a := &glueAdapter{}
	caps := a.Capabilities()

	if caps.Name != "glue" {
		t.Errorf("Capabilities.Name: want %q, got %q", "glue", caps.Name)
	}
	if caps.SupportsBranches {
		t.Errorf("Capabilities.SupportsBranches: want false (Glue is non-branching), got true")
	}
	if !caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want true (Glue Iceberg REST supports STS vending), got false")
	}
	if caps.SupportsWebhooks {
		t.Errorf("Capabilities.SupportsWebhooks: want false (Glue uses CloudWatch Events, not Iceberg webhooks), got true")
	}
	if caps.MaxNamespaceDepth != 2 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 2 (Glue catalog.database.table = 2 levels), got %d", caps.MaxNamespaceDepth)
	}
}

// TestAdapterImplementsIcebergCatalogClient is a compile-time
// assertion via interface-conversion. If a future refactor
// removes one of the IcebergCatalogClient methods, this test
// fails to compile — blocking the change with a clear signal.
func TestAdapterImplementsIcebergCatalogClient(t *testing.T) {
	t.Parallel()

	var _ iceberg.IcebergCatalogClient = (*glueAdapter)(nil)
}

// TestWithDefaultsCredentialMode asserts that withDefaults sets
// CredentialMode to "vended-credentials" when left empty.
func TestWithDefaultsCredentialMode(t *testing.T) {
	t.Parallel()

	cfg := Config{Region: "us-east-1", CatalogID: "123456789012"}
	result := cfg.withDefaults()
	if result.CredentialMode != "vended-credentials" {
		t.Errorf("withDefaults: CredentialMode: want %q, got %q", "vended-credentials", result.CredentialMode)
	}
}
