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
	// the query (HTTP 422 mapping). Phase 2 splicer (Plan 02-12) uses
	// this for malformed SQL only; grammar-mismatch and column-mask
	// authoring issues get the distinct labels below.
	ReasonSqlProxyInjectionFailed = "injection_failed"
	// ReasonSqlProxyUnsupportedQueryShape — splicer rejected the query
	// shape (JOIN, subquery, CTE, set operation, non-SELECT DML —
	// Plan 02-12 CR-A3). HTTP 422 mapping. Distinct from
	// `injection_failed` so SREs can dashboard "Phase 3 extension
	// surface signal" separately from "malformed SQL signal".
	ReasonSqlProxyUnsupportedQueryShape = "unsupported_query_shape"
	// ReasonSqlProxySpliceMismatch — column-mask splicer rejected the
	// request because the user's projection used `*` (Phase 2 cannot
	// expand schema-less SELECT) OR a masked column is absent from
	// the projection list (Plan 02-12 CR-A3). HTTP 422 mapping.
	ReasonSqlProxySpliceMismatch = "splice_mismatch"
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
			"engine_not_supported | injection_failed | " +
			"unsupported_query_shape | splice_mismatch.",
	},
	[]string{"engine", "reason"},
)

// ==========================================================================
// Phase 3 metric / label declarations — B-1 intra-wave overlap mitigation.
//
// All Phase 3 downstream Prometheus metric symbols are pre-declared here
// in a SINGLE Wave-0 edit so Plans 03-09, 03-11, and 03-12 (all in Wave 3)
// can run in parallel without metrics.go file-write contention.
//
// Plans 03-09/11/12 list metrics.go in their read_first ONLY — they do NOT
// modify this file. They reference the symbols declared below verbatim.
//
// Cardinality discipline (Pitfall 11): the `table` / `table_id_short`
// labels on 03-11 + 03-12 metrics MUST be populated via TableIDShort()
// to bound cardinality to 8-char prefixes. See TableIDShort below.
// ==========================================================================

// ---- Phase 3 reason-label string constants (consumed by Plan 03-09 gateway) ----

const (
	// ReasonPolicyPartitionSpecMismatch is the commit_rejected_total reason
	// when the write-coordinator rejects a commit because the engine's
	// partition spec does not match the table's canonical spec (P7 policy,
	// D-3.05). Maps to HTTP 403.
	ReasonPolicyPartitionSpecMismatch = "policy_partition_spec_mismatch"

	// ReasonPolicyWriteConflict is the commit_rejected_total reason
	// when the write-coordinator rejects a commit due to a write-conflict
	// policy (D-3.05 lww/abort/retry-with-backoff; abort path maps HTTP 409).
	ReasonPolicyWriteConflict = "policy_write_conflict"

	// ReasonPolicyEngineDivergent is the commit_rejected_total reason
	// when the SQL proxy or L1 gateway rejects a request because the engine's
	// CompiledPolicy.status = divergent_suspended (D-3.05). Maps to HTTP 503.
	// Distinct from ReasonPolicyEngineUnavailable so SREs can page on
	// divergence separately from engine-down events.
	ReasonPolicyEngineDivergent = "policy_engine_divergent"

	// ReasonPolicyEngineUnavailable is an alias for the Phase 1 constant
	// ReasonPolicyEngineUnavailable defined above, re-exported in the Phase 3
	// const block so Plan 03-09 can import a single contiguous block. The
	// value is identical — this is NOT a new label value, only a new alias
	// for the same string so code using the Phase 3 block stays consistent.
	// NOTE: the original const already exists in this file; this is a
	// documentation alias only — do NOT redeclare. Plan 03-09 MUST use
	// observability.ReasonPolicyEngineUnavailable (the original const above).
	// ReasonPolicyEngineUnavailable = "policy_engine_unavailable" -- ALREADY DECLARED ABOVE
)

// ---- Plan 03-11: continuous cross-engine consistency verifier metrics ----

// CrossEngineDivergenceTotal counts detected cross-engine policy
// divergences, labeled by {engine, table, severity}. Severity is one of
// "mismatch" (result differs) or "timeout" (probe did not complete in
// budget). The `table` label MUST be populated via TableIDShort() to
// bound cardinality (Pitfall 11).
//
// Plan 03-11 (verifier/sampler.go + verifier/mirror.go) increments this
// counter from the divergence detection path. SREs page on
// cross_engine_divergence_total{severity="mismatch"} > 0.
var CrossEngineDivergenceTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cross_engine_divergence_total",
		Help: "Cross-engine policy divergences detected by the continuous " +
			"consistency verifier (D-3.05). Labels: engine, table (8-char prefix), " +
			"severity={mismatch,timeout}.",
	},
	[]string{"engine", "table", "severity"},
)

// EngineProbeQueueDepth is the current depth of the verifier's probe
// queue, labeled by engine. A rising queue indicates the verifier is
// falling behind its 5-minute coverage budget.
//
// Plan 03-11 (verifier/sampler.go) sets this gauge from the probe
// scheduler goroutine.
var EngineProbeQueueDepth = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "engine_probe_queue_depth",
		Help: "Current depth of the cross-engine consistency verifier's " +
			"probe queue per engine. Rising queue = verifier falling behind " +
			"5-min coverage budget (D-3.05).",
	},
	[]string{"engine"},
)

// EngineProbeDurationSeconds is the histogram of per-probe round-trip
// latency, labeled by engine. Standard DefBuckets cover the sub-second
// warm-path; buckets above 10s surface timeout-adjacent probes.
//
// Plan 03-11 (verifier/sampler.go) observes this histogram per probe
// completion (success or failure).
var EngineProbeDurationSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "engine_probe_duration_seconds",
		Help:    "Cross-engine consistency probe round-trip latency in seconds, " +
			"by engine. Buckets cover sub-second warm-path + 10s timeout detection.",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"engine"},
)

// VerifierCoveragePairsPerCycle is the number of {table, policy} pairs
// probed in the most recent verifier cycle. Reported as a Gauge (not a
// counter) so SREs can spot a cycle where coverage dropped (e.g., probe
// budget exhausted).
//
// Plan 03-11 (verifier/sampler.go) sets this gauge at the end of each
// 5-minute coverage cycle.
var VerifierCoveragePairsPerCycle = promauto.NewGauge(
	prometheus.GaugeOpts{
		Name: "verifier_coverage_pairs_per_cycle",
		Help: "Number of {table, policy} pairs probed by the continuous " +
			"verifier in the most recent 5-min cycle (D-3.05 24h budget).",
	},
)

// VerifierUncoveredPairs is the number of {table, policy} pairs that
// were NOT probed in the most recent cycle due to budget exhaustion. A
// non-zero value means coverage is incomplete — SREs should alert if
// this remains non-zero for more than one cycle.
//
// Plan 03-11 (verifier/sampler.go) sets this gauge alongside
// VerifierCoveragePairsPerCycle.
var VerifierUncoveredPairs = promauto.NewGauge(
	prometheus.GaugeOpts{
		Name: "verifier_uncovered_pairs",
		Help: "Number of {table, policy} pairs skipped in the most recent " +
			"verifier cycle due to budget exhaustion. Non-zero = incomplete coverage.",
	},
)

// VerifierMirrorDroppedTotal counts differential-mirroring probe results
// that were dropped (e.g., queue full, non-deterministic query excluded,
// result too large to diff). A rising counter indicates the 1% mirror
// sample is being shed — SREs should investigate queue sizing.
//
// Plan 03-11 (verifier/mirror.go) increments this counter from the
// mirror probe result handler.
var VerifierMirrorDroppedTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "verifier_mirror_dropped_total",
		Help: "Differential-mirroring probe results dropped (queue full, " +
			"non-deterministic query, or result-size limit exceeded). " +
			"Rising counter = mirror shed rate increasing.",
	},
)

// ---- Plan 03-12: compaction coordinator metrics ----

// CompactionBlockedTotal counts compaction operations blocked because
// an active SnapshotPin prevented snapshot expiry, labeled by
// {reason, tenant_id, table_id_short}. The `table_id_short` label
// MUST be populated via TableIDShort() (Pitfall 11 cardinality discipline).
//
// Plan 03-12 (coordinator/coordinator.go) increments this counter from
// the compaction guard when it detects an active pin on the target snapshot.
var CompactionBlockedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "compaction_blocked_total",
		Help: "Compaction operations blocked by an active SnapshotPin " +
			"(Plan 03-12 snapshot-retention guard). Labels: reason, " +
			"tenant_id, table_id_short (8-char prefix per Pitfall 11).",
	},
	[]string{"reason", "tenant_id", "table_id_short"},
)

// ---- Cardinality-clamp helper (Pitfall 11 discipline) ----

// TableIDShort returns the first 8 characters of a table ID string for
// use as a Prometheus label value. Iceberg table IDs are UUIDs (36 chars)
// or arbitrary strings; including the full value as a label would cause
// unbounded cardinality explosion (Pitfall 11). The 8-char prefix provides
// enough entropy to identify a specific table in a dashboard query while
// keeping the time-series count bounded.
//
// Callers MUST use this function for any `table`, `table_id_short`, or
// similar label that would otherwise accept a raw table identifier.
// Plans 03-11 and 03-12 use this helper verbatim.
func TableIDShort(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

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
