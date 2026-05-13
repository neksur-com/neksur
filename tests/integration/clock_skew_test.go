//go:build integration

// Plan 00-05 Wave 4 — clock-skew alerting (C-TECH-10 / REQ-NFR-clock-skew).
//
// Three tests:
//
//   TestChronyRunning      — chronyc tracking exits 0 on the test host
//                            and reports a parseable Last offset.
//                            Skipped if chronyc is unavailable (typical
//                            on macOS dev hosts).
//   TestSkewWarnThreshold  — logical assertion: for a synthetic chrony
//                            tracking_last_offset of 0.15s, the
//                            ClockSkewWarn PromQL rule from Plan 01
//                            WOULD fire. Parses the YAML rule file via
//                            gopkg.in/yaml.v3 rather than spinning up
//                            a live Prometheus (which would require
//                            CHAOS=1 — see alerts_test.go for that
//                            tier).
//   TestSkewPageThreshold  — same shape as Warn, but offset=0.7 and
//                            target rule ClockSkewPage.
//
// Why the logical-assertion simplification?
// Spinning up Prometheus + scraping a mock chrony exporter, waiting
// for the 1-minute `for` window, and asserting via AlertManager is
// equivalent to the CypherP99LatencyBreach test in alerts_test.go —
// already expensive enough to be CHAOS=1-gated. The clock-skew alert
// rule is two-line PromQL (`abs(chrony_tracking_last_offset_seconds)
// > 0.X`) so a YAML-parse + arithmetic check covers the same correctness
// space without the 60+ second `for` window wait. The end-to-end
// firing path (chrony → Prometheus → AlertManager) is exercised by
// TestP99BreachPages's stack-up which uses the same prometheus.yml.

package integration

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

// alertRule mirrors the on-disk Prometheus rule YAML structure
// (the subset we care about).
type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type alertGroup struct {
	Name  string      `yaml:"name"`
	Rules []alertRule `yaml:"rules"`
}

type alertFile struct {
	Groups []alertGroup `yaml:"groups"`
}

// clockSkewRulePath is the on-disk location of the Plan 01 alert file.
// The relative path is from tests/integration/ to the repo root.
const clockSkewRulePath = "../../ops/prometheus/alerts/clock-skew.yaml"

// loadAlertRule parses clock-skew.yaml and returns the rule with the
// given alertname, or t.Fatal if not found.
func loadAlertRule(t *testing.T, alertname string) alertRule {
	t.Helper()
	raw, err := os.ReadFile(clockSkewRulePath)
	if err != nil {
		t.Fatalf("read %s: %v", clockSkewRulePath, err)
	}
	var f alertFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal %s: %v", clockSkewRulePath, err)
	}
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			if r.Alert == alertname {
				return r
			}
		}
	}
	t.Fatalf("alert %q not found in %s", alertname, clockSkewRulePath)
	return alertRule{}
}

// thresholdRegex extracts the right-hand-side numeric threshold from
// a `abs(metric) > X` PromQL expression. Group 1 captures the number.
var thresholdRegex = regexp.MustCompile(`>\s*([0-9.]+)`)

// extractThreshold pulls the comparison constant from a simple "expr > N"
// PromQL rule. The clock-skew rules are exactly this shape.
func extractThreshold(t *testing.T, expr string) float64 {
	t.Helper()
	m := thresholdRegex.FindStringSubmatch(expr)
	if len(m) < 2 {
		t.Fatalf("could not extract threshold from expr %q", expr)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parse threshold %q: %v", m[1], err)
	}
	return v
}

// TestChronyRunning verifies chronyd is operational on the test host
// (i.e. clock-discipline is in fact running). Skipped if chronyc is
// not on PATH — typical on macOS dev hosts which use timed/ntpd.
func TestChronyRunning(t *testing.T) {
	if _, err := exec.LookPath("chronyc"); err != nil {
		t.Skip("chronyc not on PATH — skipping (typical on macOS dev hosts)")
	}
	out, err := exec.Command("chronyc", "tracking").CombinedOutput()
	if err != nil {
		t.Fatalf("chronyc tracking exit non-zero: %v\n%s", err, out)
	}
	// Expect a "Last offset : <signed-float> seconds" line.
	re := regexp.MustCompile(`Last offset\s*:\s*([+-]?[0-9.]+)`)
	m := re.FindStringSubmatch(string(out))
	if len(m) < 2 {
		t.Fatalf("chronyc tracking output did not contain a parseable Last offset line:\n%s", out)
	}
	if _, err := strconv.ParseFloat(m[1], 64); err != nil {
		t.Fatalf("Last offset %q is not a number: %v", m[1], err)
	}
}

// TestSkewWarnThreshold asserts that the Plan 01 ClockSkewWarn rule
// (`abs(chrony_tracking_last_offset_seconds) > 0.1`) WOULD fire for an
// observed offset of 0.15 seconds. The assertion is logical (no live
// Prometheus); see file-level docs for the rationale.
func TestSkewWarnThreshold(t *testing.T) {
	rule := loadAlertRule(t, "ClockSkewWarn")
	if rule.Labels["severity"] != "warn" {
		t.Errorf("ClockSkewWarn severity = %q, want %q", rule.Labels["severity"], "warn")
	}
	if rule.For != "1m" {
		t.Errorf("ClockSkewWarn for = %q, want %q", rule.For, "1m")
	}
	threshold := extractThreshold(t, rule.Expr)
	const observedOffset = 0.15
	if !(observedOffset > threshold) {
		t.Errorf("observed offset %.2fs should exceed ClockSkewWarn threshold %.2fs but does not", observedOffset, threshold)
	}
	// And the threshold itself must be 100ms per C-TECH-10 / Pitfall 8.
	if threshold != 0.1 {
		t.Errorf("ClockSkewWarn threshold = %v, want 0.1 (per C-TECH-10 WARN tier)", threshold)
	}
}

// TestSkewPageThreshold is the PAGE-tier counterpart: 0.7s offset must
// cross the ClockSkewPage 0.5s threshold.
func TestSkewPageThreshold(t *testing.T) {
	rule := loadAlertRule(t, "ClockSkewPage")
	if rule.Labels["severity"] != "page" {
		t.Errorf("ClockSkewPage severity = %q, want %q", rule.Labels["severity"], "page")
	}
	if rule.For != "1m" {
		t.Errorf("ClockSkewPage for = %q, want %q", rule.For, "1m")
	}
	threshold := extractThreshold(t, rule.Expr)
	const observedOffset = 0.7
	if !(observedOffset > threshold) {
		t.Errorf("observed offset %.2fs should exceed ClockSkewPage threshold %.2fs but does not", observedOffset, threshold)
	}
	if threshold != 0.5 {
		t.Errorf("ClockSkewPage threshold = %v, want 0.5 (per C-TECH-10 PAGE tier)", threshold)
	}
}
