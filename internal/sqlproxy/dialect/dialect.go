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
	"fmt"
	"os"

	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// allowNoopEnvVar gates the Phase 2 no-op structural rewrite. The Phase
// 2 rewrite appends a base64 SQL comment carrying the compiled-policy
// artifact but does NOT splice the policy's row-filter into the WHERE
// clause — the engine's parser drops the comment before planning, so
// the policy is silently NOT enforced (CR-01).
//
// Until the Phase 3 SQL-parser-backed splicer lands, callers MUST set
// `NEKSUR_SQLPROXY_PHASE2_ALLOW_NOOP=1` explicitly. Any production
// listener mounted without this env-gate will fail closed with
// ErrInjectionFailed → HTTP 422 + commit_rejected_total metric, so
// SREs cannot mistake "comment appended" for "policy applied".
const allowNoopEnvVar = "NEKSUR_SQLPROXY_PHASE2_ALLOW_NOOP"

// rewriteWithBody is the Phase 2 structural rewrite shared by every
// dialect that ships a real artifact (Trino, Spark). When the
// allowNoopEnvVar env-gate is set to "1" it appends a
// `/* neksur-policy: <base64(body)> */` comment to the query — the
// base64 wrapper preserves any SQL-unsafe characters in the artifact
// body (e.g., embedded `*/` sequences would otherwise prematurely
// close the comment).
//
// When the env-gate is NOT set the function returns
// sqlproxy.ErrInjectionFailed so the proxy fails closed (CR-01): a
// comment-only "rewrite" never enforces the policy, and shipping a
// 200-OK response that pretends it did is a security false-positive.
//
// The principal argument is accepted for signature parity with the
// Phase 3 splicer (which WILL bind principal.sub / principal.roles
// into the WHERE clause); the structural rewrite does not consume it.
func rewriteWithBody(query string, body []byte, _ sqlproxy.Claims) (string, error) {
	if os.Getenv(allowNoopEnvVar) != "1" {
		return "", fmt.Errorf(
			"sqlproxy/dialect: Phase 2 structural rewrite is a no-op "+
				"(policy not spliced into WHERE) — refusing to ship policy as a SQL "+
				"comment; set %s=1 to acknowledge the non-production stub: %w",
			allowNoopEnvVar, sqlproxy.ErrInjectionFailed,
		)
	}
	return query + " /* neksur-policy: " + base64.StdEncoding.EncodeToString(body) + " */", nil
}
