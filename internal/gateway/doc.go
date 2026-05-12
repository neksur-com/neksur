// Package gateway hosts the L1 Catalog Gateway — the read-path policy
// validation pipeline that sits in front of the Iceberg REST Catalog
// (Polaris in Phase 0). Per docs/phase-0-stack.md §6 this package will
// eventually contain iceberg_proxy.go (transparent REST forwarder),
// validation.go (per-table-operation policy gate), and policy_inject.go
// (row-filter / column-mask injection).
//
// Phase 0 status: placeholder. M1 lands the proxy skeleton; M3 fills in
// the full validation pipeline per ADR-003 §3.
package gateway
