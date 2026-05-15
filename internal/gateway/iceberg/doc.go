// Package iceberg implements the Phase 1 L1 Catalog Gateway — the
// per-commit policy validation pipeline (D-1.09 fail-closed) that sits
// in front of the upstream Iceberg REST catalog (Polaris / Nessie /
// Glue / Unity).
//
// Wire layer: handlers mounted in cmd/neksur-server/main.go behind
// `workosauth.TenantMiddleware` at:
//
//   - POST /v1/iceberg/{prefix}/namespaces/{namespace}/tables/{table}
//     — single-table commit (CommitHandler in handler.go).
//   - POST /v1/iceberg/{prefix}/transactions/commit
//     — multi-table commit with Pitfall 6 Reject-All semantics
//     (MultiTableCommitHandler in multi_table.go).
//
// 10-step pipeline (handler.go):
//   1. Parse path + tenant ctx assertion.
//   2. Load tenant catalog credentials from V0060 (catalog.Repo).
//   3. Build adapter (forwarder.go::BuildAdapter).
//   4. Load current metadata via the adapter.
//   5. Fetch policies via Plan 01-05's store.AGEStore.
//   6. Evaluate via Plan 01-05's cel.Evaluator (fail-closed).
//   7. Forward commit to upstream via the adapter.
//   8. Emit WriteEvent + INTENDED_WRITE + ACTUAL_WRITE per D-003.06
//      (audit.go::EmitWriteEvent — graph node + relational audit_log
//      row in SAME tenant transaction per Open Question 4).
//   9. Increment commit_rejected_total{reason} on any reject path.
//  10. Echo upstream response to client.
//
// Pitfall 8 (principal extraction): mTLS SAN > Authorization bearer >
// WorkOS session — see principal.go::ExtractPrincipal.
//
// All AGE access goes through `gc.ExecuteInTenant` so:
//   - Layer 3 RLS scopes graph queries to the calling tenant.
//   - The pgxpool BeforeAcquire DISCARD ALL hook (Plan 01-04
//     deviation #2/#3) keeps tenant context clean across requests.
//   - D-001.14 telemetry collectors emit on every Cypher round trip.
//
// CC3: this package consumes the existing pool — DO NOT introduce a
// second pool. The Deps struct (handler.go) takes the pool by reference.
package iceberg
