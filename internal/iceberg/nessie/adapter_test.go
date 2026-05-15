// Unit tests for the nessie adapter — config validation + defaults
// + capabilities shape + compile-time interface assertion. The live
// testcontainer round-trip lives in
// tests/integration/adapter_nessie_test.go behind the
// `integration && nessie` build tags (and exercises the iceberg-go
// REST catalog wire path against a real Project Nessie 0.100+
// container on the `neksur-test` branch).
//
// These tests run unconditionally on every `go test ./...` — they
// don't require Docker.
//
// Note on TestNewAppliesDefaults: iceberg-go's rest.NewCatalog calls
// fetchConfig (`<endpoint>/v1/config`) at construction time, so
// calling New against a stub endpoint will fail at the network
// probe — not at Validate. We therefore split the test surface:
//   - Validate / withDefaults tested directly on the Config value.
//   - New rejects mis-config BEFORE the network probe (errors.Is
//     ErrInvalidConfig).
//   - Capabilities tested on a directly-constructed nessieAdapter
//     struct (same convention as polaris/adapter_test.go's
//     TestCapabilitiesShape).
package nessie

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// TestConfigWithDefaultsAppliesBranchAndAuthMode asserts the
// withDefaults helper populates DefaultBranch and AuthMode when
// caller leaves them empty. Pure value-test — no network.
func TestConfigWithDefaultsAppliesBranchAndAuthMode(t *testing.T) {
	t.Parallel()

	in := Config{Endpoint: "http://stub"}
	got := in.withDefaults()
	if got.DefaultBranch != DefaultBranch {
		t.Errorf("withDefaults.DefaultBranch: want %q, got %q", DefaultBranch, got.DefaultBranch)
	}
	if got.AuthMode != AuthModeNone {
		t.Errorf("withDefaults.AuthMode: want %q, got %q", AuthModeNone, got.AuthMode)
	}
	// Endpoint is not defaulted (it's a required field; Validate
	// rejects empty endpoints).
	if got.Endpoint != "http://stub" {
		t.Errorf("withDefaults.Endpoint: want preservation, got %q", got.Endpoint)
	}
	// Validate against the defaulted Config — must succeed for the
	// minimal-valid input shape (Endpoint + defaulted everything).
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate(defaulted minimal Config): want nil error, got %v", err)
	}
}

// TestNewRejectsMissingEndpoint asserts New surfaces a wrapped
// ErrInvalidConfig BEFORE any network call. The empty Endpoint
// case is the most common config bug (callers wire up the Nessie
// endpoint from a JSONB blob and forget to validate it).
func TestNewRejectsMissingEndpoint(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Config{})
	if err == nil {
		t.Fatal("New(empty Config): want error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(empty Config): want errors.Is(ErrInvalidConfig), got %v", err)
	}
	if !strings.Contains(err.Error(), "Endpoint") {
		t.Errorf("New(empty Config): want error message mentions Endpoint, got %q", err.Error())
	}
}

// TestNewRejectsAWSIAM asserts the surface-declared but-not-yet-
// implemented aws-iam mode fails fast at construction with a
// wrapped ErrInvalidConfig. Phase 3 lands the actual implementation.
func TestNewRejectsAWSIAM(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Config{
		Endpoint: "http://stub",
		AuthMode: AuthModeAWSIAM,
	})
	if err == nil {
		t.Fatal("New(AuthMode=aws-iam): want error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(AuthMode=aws-iam): want errors.Is(ErrInvalidConfig), got %v", err)
	}
	if !strings.Contains(err.Error(), "aws-iam") {
		t.Errorf("New(AuthMode=aws-iam): want error mentions aws-iam, got %q", err.Error())
	}
}

// TestNewRejectsMissingBearerToken asserts bearer mode without a
// BearerToken fails Validate with a wrapped ErrInvalidConfig.
// Catches the common deployment bug where a tenant's
// `catalog_credentials.config_json` declares AuthMode=bearer but
// leaves the token field null.
func TestNewRejectsMissingBearerToken(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Config{
		Endpoint: "http://stub",
		AuthMode: AuthModeBearer,
		// BearerToken intentionally empty
	})
	if err == nil {
		t.Fatal("New(AuthMode=bearer, no token): want error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(AuthMode=bearer, no token): want errors.Is(ErrInvalidConfig), got %v", err)
	}
	if !strings.Contains(err.Error(), "BearerToken") {
		t.Errorf("New(AuthMode=bearer, no token): want error mentions BearerToken, got %q", err.Error())
	}
}

// TestNewRejectsUnknownAuthMode asserts an unsupported AuthMode
// value (typo or future mode not yet recognized) fails Validate
// with a wrapped ErrInvalidConfig. Defends against silent
// misconfigurations where a JSONB blob carries a mistyped value.
func TestNewRejectsUnknownAuthMode(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Config{
		Endpoint: "http://stub",
		AuthMode: "kerberos", // not in the supported set
	})
	if err == nil {
		t.Fatal("New(AuthMode=kerberos): want error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(AuthMode=kerberos): want errors.Is(ErrInvalidConfig), got %v", err)
	}
}

// TestCapabilitiesShape asserts the static-facts Capabilities()
// returns match the documented Nessie values per RESEARCH lines
// 583-588:
//
//   - Name=nessie
//   - SupportsBranches=true (THE Nessie differentiator — D-1.02)
//   - SupportsCredVend=false (Nessie has no STS vending)
//   - SupportsWebhooks=false (no native webhook surface)
//   - MaxNamespaceDepth=1 (Phase 1 ingestion + L1 gateway scope)
//
// The Phase 1 gateway / scheduler branches on these so a silent
// regression here would change cross-engine policy enforcement
// behavior — the assert is BLOCKING.
func TestCapabilitiesShape(t *testing.T) {
	t.Parallel()

	// Construct the adapter struct directly — New would attempt a
	// real network call. Only the Capabilities method is exercised.
	a := &nessieAdapter{}
	caps := a.Capabilities()

	if caps.Name != "nessie" {
		t.Errorf("Capabilities.Name: want %q, got %q", "nessie", caps.Name)
	}
	if !caps.SupportsBranches {
		t.Error("Capabilities.SupportsBranches: want true (Nessie's branching model is the entire reason this adapter exists), got false")
	}
	if caps.SupportsCredVend {
		t.Error("Capabilities.SupportsCredVend: want false (Nessie does not vend STS subscoped credentials), got true")
	}
	if caps.SupportsWebhooks {
		t.Error("Capabilities.SupportsWebhooks: want false (Nessie has no native webhook surface in 0.100; Phase 1 L3 uses polling + S3 events), got true")
	}
	if caps.MaxNamespaceDepth != 1 {
		t.Errorf("Capabilities.MaxNamespaceDepth: want 1 (Phase 1 single-segment-namespace scope), got %d", caps.MaxNamespaceDepth)
	}
}

// TestAdapterImplementsIcebergCatalogClient is a compile-time
// assertion via interface-conversion. If a future refactor
// removes one of the IcebergCatalogClient methods, this test
// fails to compile — blocking the change with a clear signal.
//
// Belt-and-suspenders alongside the package-level `var _
// iceberg.IcebergCatalogClient = (*nessieAdapter)(nil)` declaration
// in adapter.go: the test surfaces the same constraint inside the
// test binary so it shows up in `go test ./...` output, not only
// `go build`.
func TestAdapterImplementsIcebergCatalogClient(t *testing.T) {
	t.Parallel()

	var _ iceberg.IcebergCatalogClient = (*nessieAdapter)(nil)
}
