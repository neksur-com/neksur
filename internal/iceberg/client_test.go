// Tests for the package-level surface of internal/iceberg.
//
// Two contracts:
//  1. The four sentinel errors are observably distinct strings — so
//     callers using errors.Is can rely on each branch resolving to
//     exactly one outcome (and humans reading logs can tell them
//     apart). A regression here would silently collapse two error
//     paths into one.
//  2. Capabilities{} is constructible without panic and round-trips
//     through the IcebergCatalogClient interface — the zero value
//     is documented as valid in client.go so test fixtures and stub
//     adapters can use it freely.
package iceberg

import (
	"context"
	"testing"
	"time"
)

// TestIcebergSentinelErrorsAreUniqueStrings asserts the four
// package sentinels have distinct .Error() strings. A
// map[string]struct{} membership check fails if two sentinels
// collapse to the same message — which would break callers that
// rely on the message text for debugging output.
func TestIcebergSentinelErrorsAreUniqueStrings(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrTableNotFound,
		ErrCommitConflict,
		ErrCredentialsExpired,
		ErrAdapterStub,
	}

	seen := make(map[string]struct{}, len(sentinels))
	for _, err := range sentinels {
		msg := err.Error()
		if _, dup := seen[msg]; dup {
			t.Fatalf("duplicate sentinel error message: %q", msg)
		}
		seen[msg] = struct{}{}
	}

	// Defensive belt-and-suspenders: the count must equal the
	// number of inputs. If a future refactor accidentally dedupes
	// the source slice (e.g., copy-paste), this still catches it.
	if got, want := len(seen), len(sentinels); got != want {
		t.Fatalf("expected %d distinct sentinel messages, got %d", want, got)
	}
}

// TestCapabilitiesZeroValueIsValid asserts Capabilities{} is
// constructible without panic and that an IcebergCatalogClient
// returning it doesn't trip up any happy-path call. We exercise
// this with a tiny in-test stub (NOT one of the production
// adapters — those land in Tasks 2 + 3) to keep this test
// hermetic and dependency-free.
func TestCapabilitiesZeroValueIsValid(t *testing.T) {
	t.Parallel()

	var c Capabilities // explicit zero value
	if c.Name != "" {
		t.Fatalf("Capabilities{}.Name: want empty, got %q", c.Name)
	}
	if c.SupportsBranches || c.SupportsCredVend || c.SupportsWebhooks {
		t.Fatalf("Capabilities{}: want all bools false, got %+v", c)
	}
	if c.MaxNamespaceDepth != 0 {
		t.Fatalf("Capabilities{}.MaxNamespaceDepth: want 0, got %d", c.MaxNamespaceDepth)
	}

	// Round-trip through the interface: build a tiny in-test stub
	// that returns Capabilities{} and verify Capabilities() doesn't
	// panic. This exercises the assignment + return-value plumbing
	// the IcebergCatalogClient contract requires.
	var client IcebergCatalogClient = &capabilitiesZeroStub{}
	got := client.Capabilities()
	if got != (Capabilities{}) {
		t.Fatalf("zero-value round-trip: want Capabilities{}, got %+v", got)
	}
}

// capabilitiesZeroStub is a test-only IcebergCatalogClient whose
// only purpose is to return the zero Capabilities value through the
// interface. All other methods panic — they're never called by the
// zero-value test.
type capabilitiesZeroStub struct{}

func (s *capabilitiesZeroStub) ListTables(_ context.Context, _ string) ([]TableRef, error) {
	panic("unused in zero-value test")
}
func (s *capabilitiesZeroStub) GetTable(_ context.Context, _ TableRef) (*TableMetadata, error) {
	panic("unused in zero-value test")
}
func (s *capabilitiesZeroStub) LoadTable(_ context.Context, _ TableRef) (*TableMetadata, error) {
	panic("unused in zero-value test")
}
func (s *capabilitiesZeroStub) CommitTable(_ context.Context, _ TableRef, _ CommitRequest) (*CommitResult, error) {
	panic("unused in zero-value test")
}
func (s *capabilitiesZeroStub) ExpireSnapshots(_ context.Context, _ TableRef, _ time.Time) error {
	panic("unused in zero-value test")
}
func (s *capabilitiesZeroStub) Capabilities() Capabilities { return Capabilities{} }
