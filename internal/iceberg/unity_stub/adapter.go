// Unity stub adapter — Null-Object pattern (mirror of
// glue_stub). Every state-mutating method returns
// iceberg.ErrAdapterStub wrapped with a per-method context
// string. Capabilities() reports realistic Unity values
// (SupportsCredVend=true via Unity's STS-equivalent token
// vending; SupportsWebhooks=true via Unity's event
// subscriptions; MaxNamespaceDepth=1 — Unity's three-level
// namespace `<catalog>.<schema>.<table>` is exposed by Iceberg
// REST as a single namespace segment per request).
package unity_stub

import (
	"context"
	"fmt"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// unityAdapter is the unexported concrete type. Callers obtain an
// iceberg.IcebergCatalogClient interface from New; they never see
// or depend on this struct.
type unityAdapter struct {
	cfg Config
}

// New constructs a Unity stub adapter. Stubs always succeed at
// construction — the only error path is the runtime
// state-mutating methods, each of which returns
// iceberg.ErrAdapterStub wrapped with the operation name.
//
// Phase 3 will replace this constructor (and the file at large)
// with a real Unity client wrapper; callers will not change
// because the returned type is the stable IcebergCatalogClient
// interface.
func New(_ context.Context, cfg Config) (iceberg.IcebergCatalogClient, error) {
	return &unityAdapter{cfg: cfg}, nil
}

// ListTables: not implemented in Phase 1 — returns ErrAdapterStub.
func (u *unityAdapter) ListTables(_ context.Context, _ string) ([]iceberg.TableRef, error) {
	const op = "ListTables"
	return nil, fmt.Errorf("iceberg.unity_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// GetTable: not implemented in Phase 1 — returns ErrAdapterStub.
func (u *unityAdapter) GetTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	const op = "GetTable"
	return nil, fmt.Errorf("iceberg.unity_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// LoadTable: not implemented in Phase 1 — returns ErrAdapterStub.
func (u *unityAdapter) LoadTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	const op = "LoadTable"
	return nil, fmt.Errorf("iceberg.unity_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// CommitTable: not implemented in Phase 1 — returns ErrAdapterStub.
func (u *unityAdapter) CommitTable(_ context.Context, _ iceberg.TableRef, _ iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	const op = "CommitTable"
	return nil, fmt.Errorf("iceberg.unity_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// ExpireSnapshots: not implemented in Phase 1 — returns ErrAdapterStub.
func (u *unityAdapter) ExpireSnapshots(_ context.Context, _ iceberg.TableRef, _ time.Time) error {
	const op = "ExpireSnapshots"
	return fmt.Errorf("iceberg.unity_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// Capabilities returns Unity's documented catalog facts so
// downstream code that branches on Capabilities() sees realistic
// values. SupportsCredVend=true (Unity's per-table STS-style
// credential vending — the only Phase 3 cap that's true).
// SupportsWebhooks=true (Unity emits Iceberg events through
// Databricks subscriptions). MaxNamespaceDepth=1 — Unity's
// three-level namespace is exposed flat through Iceberg REST.
func (u *unityAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:              "unity-stub",
		SupportsBranches:  false,
		SupportsCredVend:  true,
		SupportsWebhooks:  true,
		MaxNamespaceDepth: 1,
	}
}
