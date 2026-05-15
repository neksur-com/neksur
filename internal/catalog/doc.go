// Package catalog hosts the per-tenant catalog credentials store + the
// shared adapter discovery surface the L1 gateway (internal/gateway/iceberg)
// composes against.
//
// Phase 1 layout:
//
//   - errors.go — sentinel errors (ErrCredentialsNotFound,
//     ErrCatalogKindUnsupported, ErrConfigUnmarshal).
//   - creds.go  — Repo wrapping the per-tenant catalog_credentials table
//     (V0060) with a single-method surface: GetCatalogCredentials. Uses
//     tenant.WithTenantTx so the SELECT is RLS-scoped to the calling
//     tenant.
//   - creds_test.go — integration BLOCKING test asserting RLS isolation.
//
// Plan 01-06 (L1 Catalog Gateway) imports this package to look up the
// upstream Iceberg REST catalog endpoint + adapter-specific config for
// the requesting tenant; the gateway then dispatches to the correct
// per-catalog adapter (internal/iceberg/{polaris,nessie,glue_stub,unity_stub})
// via internal/gateway/iceberg/forwarder.go::BuildAdapter.
//
// Phase 0 placeholder (the original docs/phase-0-stack.md §6 plan
// envisaged this package as the home for adapter.go + polaris/ subpkg)
// has been superseded — the per-catalog adapter packages live under
// internal/iceberg/* (Plan 01-02 + 01-03), and this package is the
// per-tenant credential lookup surface only.
package catalog
