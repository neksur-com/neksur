//go:build integration

// Plan 01-09 Task 3 [BLOCKING] — commit_rejected_total 503 metric/alert
// wiring end-to-end.
//
// Two-part assertion:
//
//   (1) Drive 5 fail-closed commits through the gateway (CEL eval
//       failure path → 503 + counter increment per Plan 01-06 step 10).
//       Assert observability.CommitRejectedTotal{reason="policy_engine_unavailable"}
//       incremented by exactly 5.
//
//   (2) Parse the Phase 1 PromQL rule file
//       (observability/rules/phase1-commit-rejected.yml — Plan 01-09
//       Task 2) and verify:
//       - PolicyEngineUnavailableAlert exists
//       - expr references commit_rejected_total{reason="policy_engine_unavailable"}
//       - severity=page label set
//       - runbook_url annotation points at commit-rejected-503-rate.md
//
// Why we don't run a real Prometheus instance:
//   The 503 metric/alert WIRING test verifies the counter (in-process)
//   AND the rule definition (file on disk). Verifying that Prometheus
//   actually fires the alert against the metric requires a live
//   Prometheus server which is the existing TestP99BreachPages
//   (alerts_test.go) chaos test — kept under CHAOS=1 because it costs
//   6+ minutes wall-clock. Plan 01-09's gate is the wiring contract,
//   not the live alert fire.

package integration

import (
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"

	"github.com/neksur-com/neksur/internal/observability"
)

const commit503AlertTenant = "10000009-0009-4009-8009-000000000001"

// promRuleFile is the projection of the rule YAML we need to inspect.
type promRuleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// TestCommitRejected503MetricAlert is the BLOCKING gate per Plan
// 01-09 Task 3 + Plan VALIDATION line 77.
func TestCommitRejected503MetricAlert(t *testing.T) {
	// ----- Part 1: Drive 5 fail-closed commits + assert counter -----

	h := startGatewayHarness(t, commit503AlertTenant)

	// Seed a policy whose CEL text evaluates to a runtime error inside
	// cel-go (typed-Value error on missing field) — same shape as
	// Plan 01-06's TestGateway503OnCELPanic but isolated to this test.
	const panicCEL = `int(table.no_such_field) / 0 > 0`
	seedSchemaPolicy(t, h.gc, h.tenantStr, "alert-test-panic", panicCEL,
		"orders", "test")

	beforeCount := testutil.ToFloat64(
		observability.CommitRejectedTotal.WithLabelValues(
			observability.ReasonPolicyEngineUnavailable))

	const repeats = 5
	for i := 0; i < repeats; i++ {
		status, body := h.postCommit(t, "prod-polaris", "test", "orders",
			validCommitBody())
		if status != 503 {
			t.Fatalf("iter %d: status=%d (want 503). body=%s", i, status, body)
		}
		if !strings.Contains(body, "policy-engine-unavailable") {
			t.Errorf("iter %d: body missing 'policy-engine-unavailable': %s",
				i, body)
		}
	}

	afterCount := testutil.ToFloat64(
		observability.CommitRejectedTotal.WithLabelValues(
			observability.ReasonPolicyEngineUnavailable))

	delta := afterCount - beforeCount
	if delta != float64(repeats) {
		t.Fatalf("commit_rejected_total{reason=policy_engine_unavailable} delta=%.0f; want %d",
			delta, repeats)
	}

	// ----- Part 2: Parse the rule file + verify alert definition -----

	// Resolve the rule file path. tests/integration/ → repo root via
	// "../../" prefix. The observability/ tree is the canonical Plan
	// 01-09 location; the mirror at ops/prometheus/alerts/ is checked
	// independently (the production deploy uses the mirror; this test
	// validates the canonical source-of-truth file).
	const ruleFilePath = "../../observability/rules/phase1-commit-rejected.yml"
	raw, err := os.ReadFile(ruleFilePath)
	if err != nil {
		t.Fatalf("read %s: %v", ruleFilePath, err)
	}

	var rf promRuleFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		t.Fatalf("parse rule yaml: %v", err)
	}

	// Find PolicyEngineUnavailableAlert.
	var found *struct {
		Alert       string            `yaml:"alert"`
		Expr        string            `yaml:"expr"`
		For         string            `yaml:"for"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	}
	for gi := range rf.Groups {
		for ri := range rf.Groups[gi].Rules {
			r := &rf.Groups[gi].Rules[ri]
			if r.Alert == "PolicyEngineUnavailableAlert" {
				found = r
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		t.Fatalf("PolicyEngineUnavailableAlert not found in %s", ruleFilePath)
	}

	// expr references the counter + the policy_engine_unavailable reason.
	if !strings.Contains(found.Expr, "commit_rejected_total") {
		t.Errorf("alert expr does not reference commit_rejected_total: %q",
			found.Expr)
	}
	if !strings.Contains(found.Expr, `reason="policy_engine_unavailable"`) {
		t.Errorf("alert expr does not reference reason=policy_engine_unavailable: %q",
			found.Expr)
	}

	// severity=page label.
	if found.Labels["severity"] != "page" {
		t.Errorf("alert severity = %q; want %q",
			found.Labels["severity"], "page")
	}

	// runbook_url annotation points at commit-rejected-503-rate.md.
	rbURL := found.Annotations["runbook_url"]
	if !strings.Contains(rbURL, "commit-rejected-503-rate.md") {
		t.Errorf("alert runbook_url does not point at commit-rejected-503-rate.md: %q",
			rbURL)
	}

	// for: 1m — CONTEXT line 174 sustained-1min threshold.
	if found.For != "1m" {
		t.Errorf("alert for = %q; want %q (CONTEXT line 174 sustained threshold)",
			found.For, "1m")
	}

	// ----- Part 3: Verify the mirror at ops/prometheus/alerts/ matches  ---

	const mirrorPath = "../../ops/prometheus/alerts/phase1-commit-rejected.yaml"
	rawMirror, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatalf("read mirror %s: %v", mirrorPath, err)
	}
	var mirror promRuleFile
	if err := yaml.Unmarshal(rawMirror, &mirror); err != nil {
		t.Fatalf("parse mirror yaml: %v", err)
	}
	// Find PolicyEngineUnavailableAlert in mirror; expr MUST match
	// the canonical source so a drift between the two files would
	// surface a Prometheus loading the wrong rule.
	var mirrorFound bool
	for _, g := range mirror.Groups {
		for _, r := range g.Rules {
			if r.Alert == "PolicyEngineUnavailableAlert" {
				mirrorFound = true
				if r.Expr != found.Expr {
					t.Errorf("mirror alert expr does not match canonical:\n  canonical=%q\n  mirror   =%q",
						found.Expr, r.Expr)
				}
				if r.For != found.For {
					t.Errorf("mirror alert for=%q; canonical=%q", r.For, found.For)
				}
			}
		}
	}
	if !mirrorFound {
		t.Errorf("PolicyEngineUnavailableAlert not in mirror %s", mirrorPath)
	}
}
