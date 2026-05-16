// Package dialect houses the per-engine sqlproxy.Injector implementations
// (Trino, Spark, Dremio in Wave 2 Plan 02-05; BigQuery + Databricks +
// Snowflake light up in Phase 3). The Injector interface itself lives
// in the parent `sqlproxy` package — this package's types implement it.
//
// The factory entry point is dialect.BuildInjector (see builder.go),
// which the neksur-server wiring layer calls once per supported engine
// kind at startup. The factory was hoisted out of `sqlproxy/injector.go`
// to break the import cycle that would otherwise form between the
// parent package (which would need to import `dialect` to construct
// concrete injectors) and this package (which imports the parent for
// the Injector interface, TableRef / Claims / CacheKey types, and the
// sentinel errors).
//
// # Phase 2 splicer shape (CR-A3 closure — Plan 02-12)
//
// The InjectPolicy implementations parse the inbound user query against
// a minimal single-table SELECT grammar (splice.go) and splice the
// active CompiledPolicy.ArtifactBody into the query at the AST-correct
// position — WHERE-clause conjunction for row-filter artifacts;
// projection-list substitution for column-mask artifacts. Unsupported
// shapes (JOIN / subquery / CTE / non-SELECT) return
// sqlproxy.ErrUnsupportedQueryShape → HTTP 422 +
// sql_proxy_inject_failures_total{reason='unsupported_query_shape'}.
// The splicer implementation lives in splice.go; the per-dialect files
// (trino.go / spark.go) call SpliceArtifact with the kind discriminator
// from the CompiledPolicy.ArtifactKind field.
//
// Plan 02-12 deleted the previous Phase 2 no-op "structural rewrite"
// (comment-appending) and its `NEKSUR_SQLPROXY_PHASE2_ALLOW_NOOP`
// env-gate — the splicer is now fail-closed by construction (parser
// failure → ErrInjectionFailed; unsupported shape →
// ErrUnsupportedQueryShape).
//
// # Pitfall 11 (no query body logging)
//
// None of the InjectPolicy paths in this package log the query string
// or the artifact body. All error returns wrap a sentinel from the
// parent `sqlproxy` package; callers branch via errors.Is.
package dialect
