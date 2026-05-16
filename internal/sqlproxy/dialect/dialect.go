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
// # Phase 2 rewrite shape
//
// The InjectPolicy implementations in this package perform a STRUCTURAL
// rewrite only: they append a SQL comment carrying the base64-encoded
// CompiledPolicy artifact body to the query. This is intentional — it
// keeps Phase 2 dependency-free of a SQL parser while still giving
// integration tests a deterministic shape to assert against ("the
// artifact made it into the outbound query"). Phase 3 replaces the
// structural rewrite with real WHERE-clause splicing via a SQL parser.
//
// # Pitfall 11 (no query body logging)
//
// None of the InjectPolicy paths in this package log the query string
// or the artifact body. All error returns wrap a sentinel from the
// parent `sqlproxy` package; callers branch via errors.Is.
package dialect

import (
	"encoding/base64"

	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// rewriteWithBody is the Phase 2 structural rewrite shared by every
// dialect that ships a real artifact (Trino, Spark). It appends a
// `/* neksur-policy: <base64(body)> */` comment to the query — the
// base64 wrapper preserves any SQL-unsafe characters in the artifact
// body (e.g., embedded `*/` sequences would otherwise prematurely
// close the comment).
//
// The principal argument is accepted for signature parity with the
// Phase 3 splicer (which WILL bind principal.sub / principal.roles
// into the WHERE clause); the structural rewrite does not consume it.
func rewriteWithBody(query string, body []byte, _ sqlproxy.Claims) string {
	return query + " /* neksur-policy: " + base64.StdEncoding.EncodeToString(body) + " */"
}
