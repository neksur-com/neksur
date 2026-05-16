// Phase 1 L1 Catalog Gateway metrics — registered alongside the Phase 0
// graph collectors in internal/graph/telemetry.go.
//
// CommitRejectedTotal is the load-bearing observable for the L1
// gateway's policy-decision path. Plan 01-06 (the gateway proper)
// increments it on every rejection; Phase 1 SREs dashboard the two
// reasons independently:
//
//   - reason="policy_engine_unavailable": D-1.09 fail-closed —
//     the policy engine threw (compile error / eval error / panic /
//     graph fetch failure). The gateway returns HTTP 503; this is
//     OPERATIONALLY actionable (the engine is down or the policy text
//     is broken). Alert via AlertManager severity=page.
//
//   - reason="policy_denied": normal policy rejection — at least one
//     P1/P2/P3 policy returned ActionDeny. The gateway returns HTTP
//     403; this is BUSINESS-AS-USUAL signal (a customer's policy
//     correctly blocked a non-conforming commit). Alert via dashboard
//     trend, NOT via page.
//
// Documented allowed values for the `reason` label are exactly two:
// "policy_engine_unavailable" + "policy_denied". Any other value is a
// bug — the cardinality cap (T-1-prometheus-label-cardinality-explosion
// mitigation) prevents user-controlled label values from reaching the
// metric.
//
// Registered via promauto so import-side-effect registration matches
// the Phase 0 internal/graph/telemetry.go pattern. The HTTP /metrics
// endpoint (StartMetricsServer in graph/telemetry.go) automatically
// serves these collectors alongside the cypher_* histograms.

package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Reason label values. Exported so callers (Plan 01-06 gateway) use
// these constants instead of stringly-typed literals — typos surface at
// compile time.
const (
	// ReasonPolicyEngineUnavailable is the label value for D-1.09
	// fail-closed rejections (compile error / eval error / panic /
	// graph fetch failure). Maps to HTTP 503.
	ReasonPolicyEngineUnavailable = "policy_engine_unavailable"

	// ReasonPolicyDenied is the label value for normal policy
	// rejections (a P1/P2/P3 policy returned ActionDeny). Maps to
	// HTTP 403.
	ReasonPolicyDenied = "policy_denied"
)

// CommitRejectedTotal counts Iceberg commits the L1 gateway rejected,
// labeled by reason. See the package-doc-comment block above for the
// allowed values.
//
// Plan 01-06 increments this counter from the gateway's policy
// decision path:
//
//	if err != nil {
//	    observability.CommitRejectedTotal.WithLabelValues(
//	        observability.ReasonPolicyEngineUnavailable).Inc()
//	    http.Error(w, "policy engine unavailable", http.StatusServiceUnavailable)
//	    return
//	}
//	if decision.Action == cel.ActionDeny {
//	    observability.CommitRejectedTotal.WithLabelValues(
//	        observability.ReasonPolicyDenied).Inc()
//	    http.Error(w, decision.Reason, http.StatusForbidden)
//	    return
//	}
//
// WR-A3 contract: increment site is L1 catalog gateway ONLY
// (internal/gateway/iceberg/handler.go + multi_table.go). Phase 2
// sqlproxy uses sql_proxy_inject_failures_total for the analogous
// signal — see WR-A3 in 02-REVIEW.md. Any new sqlproxy-side
// CommitRejectedTotal.Inc() call must be rejected at code review
// (it would re-introduce the paging-semantics drift WR-A3 closed).
var CommitRejectedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "commit_rejected_total",
		Help: "Iceberg commits rejected by the L1 gateway, by reason. " +
			"Allowed reasons: policy_engine_unavailable | policy_denied.",
	},
	[]string{"reason"},
)

// ---------------------------------------------------------------------
// Wave 2 / Plan 02-05 sqlproxy metrics — registered alongside the
// Phase 1 commit_rejected_total counter. Three observables let SREs
// dashboard the SQL-layer enforcement proxy independently of the
// catalog gateway:
//
//   - SqlProxyOverheadMs (histogram, labeled by engine) — end-to-end
//     handler latency in milliseconds. Buckets are tuned for the
//     <100ms warm-path target (RESEARCH §Pattern 7 line 1184) with
//     long-tail buckets so Phase 2 SREs can spot pathological cold-
//     compile spikes (>1s = wrong; >10s = page).
//   - SqlProxyLookupTotal (counter, labeled by engine + cache_status)
//     — request count by cache outcome. cache_status is one of
//     {hit, miss, error} — Injector implementations report verbatim
//     via the CacheStatus* constants in internal/sqlproxy/injector.go.
//     The 3-value cardinality is enforced at the call site (Go's type
//     system can't constrain string labels); any drift surfaces as a
//     new label-value time series on the dashboard.
//   - SqlProxyInjectFailuresTotal (counter, labeled by engine + reason)
//     — failure count by reason. reason is one of
//     {policy_engine_unavailable, engine_not_supported, injection_failed}
//     — mirrors the sqlproxy.Err* sentinels (errors.go) so the
//     dashboard alert routing matches the HTTP status code mapping.
//
// Registration shape mirrors CommitRejectedTotal above: promauto
// against the default registry so the existing /metrics endpoint
// (graph/telemetry.go) serves the new families with no extra wiring.
// ---------------------------------------------------------------------

// Allowed sql_proxy_lookup_total cache_status label values. Re-exported
// from internal/sqlproxy/injector.go's CacheStatus* constants so
// non-sqlproxy callers (test code, ops tooling) can use the
// observability package as the single import path for label values.
const (
	// CacheStatusHit — artifact served from the process-local LRU.
	CacheStatusHit = "hit"
	// CacheStatusMiss — artifact fetched from the CompiledStore.
	CacheStatusMiss = "miss"
	// CacheStatusError — cache layer threw; request proceeded
	// against the store directly (best-effort fallback).
	CacheStatusError = "error"
)

// Allowed sql_proxy_inject_failures_total reason label values.
// Mirrors the sqlproxy.Err* sentinels (internal/sqlproxy/errors.go).
const (
	// ReasonSqlProxyPolicyEngineUnavailable — store fetch failed
	// or artifact malformed (HTTP 503 mapping).
	ReasonSqlProxyPolicyEngineUnavailable = "policy_engine_unavailable"
	// ReasonSqlProxyEngineNotSupported — dialect dispatch returned
	// ErrEngineNotSupported (HTTP 501 mapping).
	ReasonSqlProxyEngineNotSupported = "engine_not_supported"
	// ReasonSqlProxyInjectionFailed — per-dialect rewriter rejected
	// the query (HTTP 422 mapping).
	ReasonSqlProxyInjectionFailed = "injection_failed"
)

// SqlProxyOverheadMs is the end-to-end sqlproxy handler latency
// histogram in milliseconds, labeled by engine. Buckets cover the
// <100ms warm-path target (RESEARCH §Pattern 7) with long-tail
// 1s / 5s / 10s buckets for pathological cold-compile detection.
var SqlProxyOverheadMs = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "sql_proxy_overhead_ms",
		Help: "End-to-end sqlproxy handler latency in milliseconds, " +
			"by engine. Warm-path target <100ms; >1s buckets surface " +
			"pathological cold-compile or store-fetch stalls.",
		Buckets: []float64{5, 10, 25, 50, 100, 200, 500, 1000, 5000, 10000},
	},
	[]string{"engine"},
)

// SqlProxyLookupTotal counts sqlproxy handler dispatches by engine
// + cache_status. cache_status MUST be one of CacheStatusHit /
// CacheStatusMiss / CacheStatusError; callers use the constants above.
var SqlProxyLookupTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sql_proxy_lookup_total",
		Help: "sqlproxy handler dispatches by engine + cache_status. " +
			"Allowed cache_status: hit | miss | error.",
	},
	[]string{"engine", "cache_status"},
)

// SqlProxyInjectFailuresTotal counts sqlproxy injection failures by
// engine + reason. reason MUST be one of the ReasonSqlProxy* constants
// above (mirrors the sqlproxy.Err* sentinels).
var SqlProxyInjectFailuresTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sql_proxy_inject_failures_total",
		Help: "sqlproxy injection failures by engine + reason. " +
			"Allowed reasons: policy_engine_unavailable | " +
			"engine_not_supported | injection_failed.",
	},
	[]string{"engine", "reason"},
)

// ---------------------------------------------------------------------
// Wave 3 / Plan 02-07 L4 credential vending metrics.
//
// Three metrics let SREs dashboard the L4 STS vending gate independently
// of the sqlproxy and L1 gateway metrics:
//
//   - L4TokenIssuedTotal (counter, labeled by engine + region) — counts
//     successful STS issuances on the cache-miss path. engine is the
//     catalog name (e.g. "polaris"); region is the AWS region. A high
//     rate indicates cache churn (check TTL/2 refresh threshold).
//   - L4TokenRefreshTotal (counter, labeled by engine) — counts cache-miss
//     events that trigger an upstream IssueScopedSTSCredentials call. This
//     is a superset of L4TokenIssuedTotal (includes failures).
//   - KmsGenerateDataKeyTotal (counter, labeled by cache_status) — counts
//     Go-side GenerateDataKey calls. cache_status ∈ {hit, miss, error}.
//     A high miss rate indicates batch IDs are not being reused correctly
//     (Pitfall 10 metric).
//   - L4TokenFailuresTotal (counter, labeled by engine + reason) — counts
//     errors on the Issue path. reason is free-form but bounded (e.g.
//     "issue_failed", "engine_not_supported").
// ---------------------------------------------------------------------

// L4TokenIssuedTotal counts successful L4 STS credential issuances on the
// cache-miss path. Labels: engine (catalog name), region (AWS region).
var L4TokenIssuedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "l4_token_issued_total",
		Help: "Successful L4 STS credential issuances by catalog engine " +
			"and AWS region (cache-miss path only).",
	},
	[]string{"engine", "region"},
)

// L4TokenRefreshTotal counts cache-miss events that trigger an upstream
// IssueScopedSTSCredentials call. Label: engine (catalog name).
var L4TokenRefreshTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "l4_token_refresh_total",
		Help: "L4 STS credential cache-miss events that trigger an upstream " +
			"IssueScopedSTSCredentials call.",
	},
	[]string{"engine"},
)

// KmsGenerateDataKeyTotal counts Go-side GenerateDataKey calls.
// cache_status ∈ {hit, miss, error}.
var KmsGenerateDataKeyTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "kms_generate_data_key_total",
		Help: "Go-side KMS GenerateDataKey calls by cache_status. " +
			"Allowed cache_status: hit | miss | error.",
	},
	[]string{"cache_status"},
)

// L4TokenFailuresTotal counts errors on the credvend.Service.Issue path.
// Labels: engine (catalog name), reason (short string describing failure).
var L4TokenFailuresTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "l4_token_failures_total",
		Help: "L4 STS credential issuance failures by engine + reason.",
	},
	[]string{"engine", "reason"},
)
