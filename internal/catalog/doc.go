// Package catalog hosts the catalog-adapter abstraction. Per
// docs/phase-0-stack.md §6 this package will contain adapter.go (the
// interface every adapter implements), polaris/ (Apache Polaris adapter
// — P1, Phase 0 only catalog), and generic_rest/ (the Iceberg REST
// fallback for Polaris-compatible deployments). Other catalogs (Unity,
// Glue, Snowflake) land in Phase 1 per the constraint document §2.7.
//
// Phase 0 status: placeholder. M1 lands the Polaris read-only adapter.
package catalog
