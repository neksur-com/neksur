// sqlproxy sentinel errors — exported so the HTTP handler (server.go)
// and dialect Injector implementations can branch via errors.Is. The
// convention mirrors internal/policy/compiler/errors.go: sentinels
// declared with errors.New (NOT fmt.Errorf) so equality is reference-
// stable across wrapping.

package sqlproxy

import "errors"

// ErrPolicyEngineUnavailable signals a fail-closed condition: the
// CompiledStore lookup failed (graph fetch error / agtype scan error)
// or the active CompiledPolicy artifact is malformed. The HTTP
// handler maps this to 503 + sql_proxy_inject_failures_total{reason=
// "policy_engine_unavailable"} (WR-A3: NOT commit_rejected_total —
// that counter is L1-catalog-gateway-only so Phase 1 paging rules
// stay honest). Dashboard the sqlproxy metric family in parallel
// with the L1 gateway's commit_rejected_total to recover the
// "policy engine outage spans both paths" view.
var ErrPolicyEngineUnavailable = errors.New("sqlproxy: policy engine unavailable")

// ErrEngineNotSupported signals the request path's `{engine}` segment
// does not resolve to a registered Injector. The HTTP handler maps
// this to 501 — a deterministic "stub" response that lets clients
// distinguish "we don't speak your dialect yet" from "transient
// failure". Phase 3 lights up the Dremio + Snowflake dialects; until
// then their Injector returns this sentinel wrapped with the engine
// name for audit log clarity.
var ErrEngineNotSupported = errors.New("sqlproxy: engine not supported")

// ErrInjectionFailed signals the per-dialect rewriter rejected the
// query — e.g., the parser could not produce a SELECT-shape AST,
// or the column-mask emitter encountered an unbound binding. The
// HTTP handler maps this to 422 (semantically invalid input) +
// sql_proxy_inject_failures_total{reason="injection_failed"}.
// Distinct from ErrPolicyEngineUnavailable so SREs can tell "policy
// store outage" from "client sent un-rewritable SQL" on the dashboard.
var ErrInjectionFailed = errors.New("sqlproxy: policy injection failed")

// ErrUnsupportedQueryShape signals the splicer recognized the query
// as well-formed SQL but rejected it because it falls outside the
// Phase 2 single-table SELECT grammar (JOIN, subquery, CTE,
// multi-table comma, set operation, non-SELECT DML). Distinct from
// ErrInjectionFailed so SREs can distinguish "policy author wrote
// a query shape we don't support yet" (Phase 3 extension surface)
// from "client sent un-parsable SQL" (likely a bug or attack).
// HTTP 422 + sql_proxy_inject_failures_total{reason="unsupported_query_shape"}.
var ErrUnsupportedQueryShape = errors.New("sqlproxy: unsupported query shape (Phase 2: single-table SELECT only)")

// ErrSpliceMismatch signals the column-mask splicer cannot apply the
// artifact: either the user's SELECT used `*` (Phase 2 cannot expand
// schema-less projections — Plan 02-13 / Phase 3 lifts this) OR the
// masked column is not present in the projection list (the policy
// author referenced a column the query does not select). HTTP 422 +
// sql_proxy_inject_failures_total{reason="splice_mismatch"}.
var ErrSpliceMismatch = errors.New("sqlproxy: splice mismatch (column-mask requires explicit column projection)")
