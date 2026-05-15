// Package unity_stub is the Databricks Unity Catalog
// IcebergCatalogClient adapter STUB (D-1.02 — live tests defer to
// Phase 3 multi-engine validation). Mirror of glue_stub: every
// state-mutating method returns iceberg.ErrAdapterStub at runtime;
// the package satisfies IcebergCatalogClient at compile time so
// callers can branch on errors.Is + the typed sentinel.
//
// Phase 3 will replace this package wholesale with a real Unity
// adapter wrapping the Databricks REST + Iceberg metadata reader.
// Existing callers won't change — the IcebergCatalogClient
// interface stays stable.
//
// Plan 01-02 SPEC reference (Frame B / SPEC v0.7 §6.4): the
// April 2026 Databricks Unity gap (row filters + column masks
// not enforced through the Iceberg REST API) is the documented
// market wedge — a live Unity adapter is the surface where we
// PROVE the cross-engine policy enforcement story. Until Phase
// 3 ships that live adapter, this stub keeps the interface
// contract stable so Plan 01-06 gateway code doesn't have to
// branch on "Unity not yet wired" at runtime.
package unity_stub

// Config is the declarative shape for Unity's catalog connection
// (D-1.03 — no functional-options). Phase 3 live tests will
// populate WorkspaceURL with the customer's Databricks workspace
// hostname (e.g., `https://acme.cloud.databricks.com`) and
// AccessToken with a personal access token or service principal
// secret.
//
// No Validate method on this Config — stubs accept any input
// shape (including the zero value). Phase 3 will introduce the
// validation step alongside the live wire layer.
type Config struct {
	WorkspaceURL string
	AccessToken  string
}
