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
var CommitRejectedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "commit_rejected_total",
		Help: "Iceberg commits rejected by the L1 gateway, by reason. " +
			"Allowed reasons: policy_engine_unavailable | policy_denied.",
	},
	[]string{"reason"},
)
