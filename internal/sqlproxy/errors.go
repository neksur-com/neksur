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
