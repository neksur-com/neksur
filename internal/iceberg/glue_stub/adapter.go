// Glue stub adapter — Null-Object pattern (Phase 0.5
// internal/billing/noop.go is the in-repo precedent). Every
// state-mutating method returns iceberg.ErrAdapterStub wrapped
// with a per-method context string so logs disambiguate which
// call hit the stub. Capabilities() returns the realistic Glue
// catalog facts (no branches, no credential vending, no
// webhooks, single-segment namespaces) so downstream code that
// branches on Capabilities() doesn't see all-defaults-zero from
// the stub and assume the live adapter would also report nothing
// — which would silently drift Phase 3's live behavior away from
// what Plans 01-06 / 01-07 hardcoded against.
package glue_stub

import (
	"context"
	"fmt"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// glueAdapter is the unexported concrete type. Callers obtain an
// iceberg.IcebergCatalogClient interface from New; they never see
// or depend on this struct.
type glueAdapter struct {
	cfg Config
}

// New constructs a Glue stub adapter. Stubs always succeed at
// construction (no Validate) — the only error path is the runtime
// state-mutating methods, each of which returns
// iceberg.ErrAdapterStub wrapped with the operation name.
//
// Phase 3 will replace this constructor (and the file at large)
// with a real Glue client wrapper; callers will not change
// because the returned type is the stable IcebergCatalogClient
// interface.
func New(_ context.Context, cfg Config) (iceberg.IcebergCatalogClient, error) {
	return &glueAdapter{cfg: cfg}, nil
}

// ListTables: not implemented in Phase 1 — returns ErrAdapterStub.
func (g *glueAdapter) ListTables(_ context.Context, _ string) ([]iceberg.TableRef, error) {
	const op = "ListTables"
	return nil, fmt.Errorf("iceberg.glue_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// GetTable: not implemented in Phase 1 — returns ErrAdapterStub.
func (g *glueAdapter) GetTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	const op = "GetTable"
	return nil, fmt.Errorf("iceberg.glue_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// LoadTable: not implemented in Phase 1 — returns ErrAdapterStub.
func (g *glueAdapter) LoadTable(_ context.Context, _ iceberg.TableRef) (*iceberg.TableMetadata, error) {
	const op = "LoadTable"
	return nil, fmt.Errorf("iceberg.glue_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// CommitTable: not implemented in Phase 1 — returns ErrAdapterStub.
func (g *glueAdapter) CommitTable(_ context.Context, _ iceberg.TableRef, _ iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	const op = "CommitTable"
	return nil, fmt.Errorf("iceberg.glue_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// ExpireSnapshots: not implemented in Phase 1 — returns ErrAdapterStub.
func (g *glueAdapter) ExpireSnapshots(_ context.Context, _ iceberg.TableRef, _ time.Time) error {
	const op = "ExpireSnapshots"
	return fmt.Errorf("iceberg.glue_stub: %s: %w", op, iceberg.ErrAdapterStub)
}

// Capabilities returns Glue's documented catalog facts so
// downstream code that branches on Capabilities() sees realistic
// values from the stub. Critically, MaxNamespaceDepth=1 — Glue
// databases are flat, no nesting (vs Polaris's 100). Without this
// realistic shape, Plan 01-06 code that special-cases catalogs
// with MaxNamespaceDepth >1 would behave correctly against
// Polaris but incorrectly against a future live Glue.
func (g *glueAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:              "glue-stub",
		SupportsBranches:  false,
		SupportsCredVend:  false,
		SupportsWebhooks:  false,
		MaxNamespaceDepth: 1,
	}
}

// IssueScopedSTSCredentials: not implemented in Phase 2 — returns
// ErrAdapterStub per D-2.09. Glue's STS shape differs from Polaris
// (different token format, different API). Phase 3+ lights Glue live.
// Defense-in-depth on top of the CR-03 boot-time guard.
func (g *glueAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	const op = "IssueScopedSTSCredentials"
	return nil, fmt.Errorf("iceberg.glue_stub: %s: %w", op, iceberg.ErrAdapterStub)
}
