//go:build integration

// Plan 01-07 Task 3 [BLOCKING] — ADR-007 emission shape proof.
//
// Calls regex.EmitDetectionResults with one finding {ColumnName: "email",
// TagID: "pii-email", Confidence: 0.92}. Then queries the graph to
// confirm the canonical ADR-007 + CONTEXT line 85 emission shape:
//
//   - DetectionRun vlabel exists with the run_id.
//   - VIOLATION_DETECTED_BY edge from Snapshot to DetectionRun (V0010
//     preexisting elabel).
//   - CLASSIFIED_AS edge from Column to Tag with confidence ≥ 0.85
//     (V0010 preexisting elabel).
//   - DETECTED_BY edge from Classification to DetectionRun (V0030 NEW
//     elabel per ADR-007 §6.5).
//
// The Classification node is the NEW V0030 vlabel that distinguishes
// per-finding instances from the long-lived Tag dictionary node.

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/neksur-com/neksur/internal/detect/regex"
	"github.com/neksur-com/neksur/internal/graph"
)

const detectADR007Tenant = "66666666-6666-4666-8666-666666666666"

// TestDetectADR007EmissionShape — the BLOCKING gate proving the
// ADR-007 graph shape is what gets emitted (NOT the generic Policy
// shape).
func TestDetectADR007EmissionShape(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()
	_ = fx.ProvisionTenant(t, detectADR007Tenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const snapLoc = "s3://adr007-test/snap-uuid/metadata.json"

	finding := regex.ColumnFinding{
		ColumnName:  "email",
		TagID:       "pii-email",
		TagName:     "Email",
		TagCategory: "PII",
		Confidence:  0.92,
		MatchType:   regex.MatchNameAndValue,
	}

	runID, err := regex.EmitDetectionResults(
		context.Background(), gc, detectADR007Tenant,
		snapLoc, "regex", 1, []regex.ColumnFinding{finding})
	if err != nil {
		t.Fatalf("EmitDetectionResults: %v", err)
	}
	if runID == "" {
		t.Fatalf("expected non-empty runID; got '' (cross-replica dedup unexpected on fresh tenant)")
	}

	// Assertion 1 — DetectionRun vertex exists.
	drCount := countMatching(t, fx.ctx, gc, detectADR007Tenant,
		fmt.Sprintf("MATCH (dr:DetectionRun {run_id: '%s'}) RETURN count(dr)", runID))
	if drCount != 1 {
		t.Errorf("DetectionRun count for run_id=%s = %d; want 1", runID, drCount)
	}

	// Assertion 2 — VIOLATION_DETECTED_BY edge from Snapshot to DetectionRun
	// (V0010 preexisting; D-003.06).
	violationCount := countMatching(t, fx.ctx, gc, detectADR007Tenant,
		fmt.Sprintf(
			"MATCH (s:Snapshot {metadata_location: '%s'})-[:VIOLATION_DETECTED_BY]->(dr:DetectionRun {run_id: '%s'}) RETURN count(s)",
			snapLoc, runID))
	if violationCount != 1 {
		t.Errorf("VIOLATION_DETECTED_BY edge count = %d; want 1 (D-003.06)", violationCount)
	}

	// Assertion 3 — CLASSIFIED_AS edge from Column to Tag with
	// confidence ≥ 0.85 (V0010 preexisting).
	classifiedAsCount := countMatching(t, fx.ctx, gc, detectADR007Tenant,
		`MATCH (c:Column)-[r:CLASSIFIED_AS]->(tag:Tag {id: 'pii-email'}) WHERE r.confidence >= 0.85 RETURN count(r)`)
	if classifiedAsCount < 1 {
		t.Errorf("CLASSIFIED_AS edge count for pii-email with confidence ≥ 0.85 = %d; want ≥ 1",
			classifiedAsCount)
	}

	// Assertion 4 — DETECTED_BY edge from Classification to DetectionRun
	// (V0030 NEW per ADR-007 §6.5). This is the load-bearing assertion
	// for the ADR-007 emission shape — IF this fails, the plan is
	// emitting generic Policy nodes instead of Classification nodes.
	classificationCount := countMatching(t, fx.ctx, gc, detectADR007Tenant,
		fmt.Sprintf(
			`MATCH (cl:Classification {tag_id: 'pii-email', detection_run: '%s'})-[:DETECTED_BY]->(dr:DetectionRun {run_id: '%s'}) RETURN count(cl)`,
			runID, runID))
	if classificationCount != 1 {
		t.Errorf("DETECTED_BY edge count from Classification(tag_id=pii-email,detection_run=%s) to DetectionRun = %d; want 1 (ADR-007 §6.5)",
			runID, classificationCount)
	}
}
