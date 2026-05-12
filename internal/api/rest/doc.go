// Package rest hosts the REST API server. Per docs/phase-0-stack.md §6
// this package will contain server.go (net/http + chi router), and per-
// resource handler files (policies.go, catalogs.go, etc.). OpenAPI 3.0
// spec generated from the handler definitions per the constraint
// document §2.12. GraphQL is explicitly out of scope (Phase 2+ per §4).
//
// Phase 0 status: placeholder. M1 lands the skeleton + health endpoint;
// M2 fills in policy CRUD; M3 adds catalog / lineage read endpoints.
package rest
