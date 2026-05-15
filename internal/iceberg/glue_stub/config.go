// Package glue_stub is the AWS Glue IcebergCatalogClient adapter
// STUB (D-1.02 — live tests defer to Phase 3 multi-engine
// validation). Every state-mutating method returns
// iceberg.ErrAdapterStub at runtime; the package satisfies the
// IcebergCatalogClient interface at compile time so callers can
// branch on `errors.Is(err, iceberg.ErrAdapterStub)` to detect
// the not-yet-live state without per-catalog runtime sniffing.
//
// Phase 3 will replace this package wholesale with a real Glue
// adapter wrapping the AWS SDK v2's glue client + the Iceberg
// metadata reader. Existing callers won't change — the
// IcebergCatalogClient interface stays stable.
package glue_stub

// Config is the declarative shape for Glue's catalog connection
// (D-1.03 — no functional-options). Phase 3 live tests will
// populate these from AWS env (typical: AWS_REGION + the IAM
// role ARN that grants Glue:GetTable + Glue:UpdateTable).
//
// No Validate method on this Config — stubs accept any input
// shape (including the zero value). Phase 3 will introduce the
// validation step alongside the live wire layer.
type Config struct {
	Region    string
	CatalogID string
}
