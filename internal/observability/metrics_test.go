// Unit tests for the Phase 1 commit_rejected_total Prometheus counter
// — Plan 01-05 Task 2.
//
// Verifies:
//   - CommitRejectedTotal is registered (Inc() doesn't panic; the
//     metric appears in the default registry's Gather() output).
//   - Both documented label values (policy_engine_unavailable +
//     policy_denied) are observable.

package observability

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCommitRejectedTotalRegistered(t *testing.T) {
	// Inc both label values — must not panic.
	CommitRejectedTotal.WithLabelValues(ReasonPolicyEngineUnavailable).Inc()
	CommitRejectedTotal.WithLabelValues(ReasonPolicyDenied).Inc()

	// Gather from the default registry; assert the metric family
	// appears with the correct name + help text.
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "commit_rejected_total" {
			found = true
			if !strings.Contains(mf.GetHelp(), "policy_engine_unavailable") {
				t.Errorf("Help text missing policy_engine_unavailable: %q", mf.GetHelp())
			}
			if !strings.Contains(mf.GetHelp(), "policy_denied") {
				t.Errorf("Help text missing policy_denied: %q", mf.GetHelp())
			}
			// Confirm both label values are observed.
			labelValuesSeen := map[string]bool{}
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "reason" {
						labelValuesSeen[lp.GetValue()] = true
					}
				}
			}
			if !labelValuesSeen[ReasonPolicyEngineUnavailable] {
				t.Errorf("expected reason=policy_engine_unavailable in metric output")
			}
			if !labelValuesSeen[ReasonPolicyDenied] {
				t.Errorf("expected reason=policy_denied in metric output")
			}
		}
	}
	if !found {
		t.Errorf("commit_rejected_total not registered in default Prometheus registry")
	}
}

func TestReasonConstants(t *testing.T) {
	if ReasonPolicyEngineUnavailable != "policy_engine_unavailable" {
		t.Errorf("ReasonPolicyEngineUnavailable = %q; want policy_engine_unavailable",
			ReasonPolicyEngineUnavailable)
	}
	if ReasonPolicyDenied != "policy_denied" {
		t.Errorf("ReasonPolicyDenied = %q; want policy_denied", ReasonPolicyDenied)
	}
}
