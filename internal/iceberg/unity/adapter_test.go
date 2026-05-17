// Unit tests for the unity adapter — config validation + capabilities shape.
// The live testcontainer round-trip lives in
// tests/integration/adapter_unity_test.go behind the `integration` build tag
// (exercises the OAuth + LoadTable wire path against a real Databricks workspace
// or skips when credentials are absent per D-2.09 PENDING_FIRST_RUN).
//
// These tests run unconditionally on every `go test ./...` — they don't
// require Docker or live Unity credentials.
package unity

import (
	"context"
	"errors"
	"testing"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// TestNewValidatesConfig asserts New rejects an empty Config with a wrapped
// ErrInvalidConfig, so callers can branch on errors.Is without paying for a
// network call up front.
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

// TestNewValidatesEachRequiredField walks the five required fields and confirms
// each one's absence is independently detected. This guards against a future
// refactor accidentally letting one field silently default through.
func TestNewValidatesEachRequiredField(t *testing.T) {
	t.Parallel()

	// Base valid config — all five required fields present. Each sub-test
	// zeroes exactly one field.
	base := Config{
		WorkspaceHost:     "https://adb-12345.1.azuredatabricks.net",
		WorkspaceID:       "12345",
		OAuthClientID:     "client-id",
		OAuthClientSecret: "client-secret",
		CatalogName:       "main",
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{
			"missing WorkspaceHost",
			Config{WorkspaceID: base.WorkspaceID, OAuthClientID: base.OAuthClientID, OAuthClientSecret: base.OAuthClientSecret, CatalogName: base.CatalogName},
		},
		{
			"missing WorkspaceID",
			Config{WorkspaceHost: base.WorkspaceHost, OAuthClientID: base.OAuthClientID, OAuthClientSecret: base.OAuthClientSecret, CatalogName: base.CatalogName},
		},
		{
			"missing OAuthClientID",
			Config{WorkspaceHost: base.WorkspaceHost, WorkspaceID: base.WorkspaceID, OAuthClientSecret: base.OAuthClientSecret, CatalogName: base.CatalogName},
		},
		{
			"missing OAuthClientSecret",
			Config{WorkspaceHost: base.WorkspaceHost, WorkspaceID: base.WorkspaceID, OAuthClientID: base.OAuthClientID, CatalogName: base.CatalogName},
		},
		{
			"missing CatalogName",
			Config{WorkspaceHost: base.WorkspaceHost, WorkspaceID: base.WorkspaceID, OAuthClientID: base.OAuthClientID, OAuthClientSecret: base.OAuthClientSecret},
		},
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

// TestCapabilitiesShape asserts the static-facts Capabilities() returns match
// the documented Unity values per 03-PATTERNS §14 / Unity flat namespace
// constraint. The Phase 1 gateway / scheduler branches on these so a silent
// regression here would change cross-engine policy enforcement behavior.
func TestCapabilitiesShape(t *testing.T) {
	t.Parallel()

	// Construct the adapter struct directly (can't go through New without a
	// real Databricks workspace endpoint).
	a := &unityAdapter{}
	caps := a.Capabilities()

	if caps.Name != "unity" {
		t.Errorf("Capabilities.Name: want %q, got %q", "unity", caps.Name)
	}
	if caps.SupportsBranches {
		t.Errorf("Capabilities.SupportsBranches: want false (Unity is non-branching), got true")
	}
	if !caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want true (Unity STS vending), got false")
	}
	if !caps.SupportsWebhooks {
		t.Error("Capabilities.SupportsWebhooks: want true (Unity emits Iceberg events), got false")
	}
	if caps.MaxNamespaceDepth != 1 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 1 (Unity flat namespace per REST), got %d", caps.MaxNamespaceDepth)
	}
}

// TestAdapterImplementsIcebergCatalogClient is a compile-time assertion via
// interface-conversion. If a future refactor removes one of the
// IcebergCatalogClient methods, this test fails to compile — blocking the
// change with a clear signal.
func TestAdapterImplementsIcebergCatalogClient(t *testing.T) {
	t.Parallel()

	var _ iceberg.IcebergCatalogClient = (*unityAdapter)(nil)
}
