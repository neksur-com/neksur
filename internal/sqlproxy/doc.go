// Package sqlproxy hosts Wave 2's SQL-layer enforcement proxy — the
// HTTPS endpoint engines hit when they need a policy-rewritten SQL
// statement (row-filter / column-mask injection) before they execute
// it against the underlying lakehouse. Per D-2.08 the proxy lives
// between the engine and the catalog, terminates TLS 1.3 with
// mTLS-required client auth, and dispatches per-dialect Injector
// implementations that consume the CompiledPolicy artifacts produced
// by Plan 02-04's cross-engine compiler.
//
// CompiledPolicy contract (Plan 02-04 + D-2.08):
//
//   - The CompiledStore (internal/policy/store/compiled.go) is the
//     read-path source of truth. For each (tenant, table, engine)
//     tuple it returns zero or more CompiledPolicy nodes; the proxy
//     filters to {Status == CompiledPolicyStatusActive} entries whose
//     EngineKind matches the request path's `{engine}` segment.
//   - The ArtifactBody is opaque to the proxy core: dialect Injector
//     implementations (internal/sqlproxy/dialect/*) parse it as their
//     engine-specific shape (SQL fragment for Trino/Spark, JSON
//     CELArtifact for stub paths).
//   - Compile artifacts are cached process-local in an LRU keyed on
//     (TenantID, Namespace, Table, Engine); the SourceChecksum lets
//     LISTEN/NOTIFY-driven invalidation (Plan 02-04 trigger.go)
//     evict stale entries without restarting the proxy.
//
// Dispatch boundary: this file + server.go + tls.go + cert_watcher.go
// + injector.go + errors.go land in Wave 2 plan 02-05 dispatch A. The
// concrete per-dialect Injector implementations land in dispatch B
// (internal/sqlproxy/dialect/{trino,spark,bigquery,databricks}/).
// neksur-server main.go wiring lands in dispatch C; integration tests
// in dispatch D.
//
// Pitfall 11 (Phase 2 RESEARCH) reminder: NEVER log query bodies or
// response bodies. The proxy handler emits structured slog records
// with metadata only — engine name, table identifier, status code,
// latency_ms. Query text is sensitive (may contain literals from PII
// columns); response body is the rewritten query (may reveal policy
// structure to an attacker who can read logs).
package sqlproxy
