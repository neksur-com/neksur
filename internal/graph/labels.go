package graph

// NodeLabels is the canonical ordered list of the 19 vertex labels per
// D-001.05 as AMENDED by D-003.06 (which adds WriteEvent + DetectionRun
// for the write-path enforcement architecture introduced by ADR-003).
// Order matches the V0010 migration for human-grep ergonomics; equality
// checks use the LabelWhitelist set built below.
var NodeLabels = []string{
	"Table", "Column", "Snapshot", "Metric", "Dimension",
	"View", "Dashboard", "Pipeline", "Query", "Person",
	"Team", "Policy", "GlossaryTerm", "Tag", "DataContract",
	"Engine", "Catalog", "WriteEvent", "DetectionRun",
}

// MandatoryEdgeLabels is the canonical ordered list of the 18 mandatory
// edge labels per D-001.06 as AMENDED by D-003.06 (which adds
// INTENDED_WRITE + ACTUAL_WRITE + VIOLATION_DETECTED_BY).
var MandatoryEdgeLabels = []string{
	"LINEAGE_OF", "OWNS", "MEMBER_OF", "DEPENDS_ON", "CLASSIFIED_AS",
	"APPLIES_TO", "DEFINED_BY", "WROTE", "READ", "PRODUCES",
	"CONSUMES", "GOVERNED_BY", "STORED_IN", "RUNS_ON", "SUPERSEDES",
	"INTENDED_WRITE", "ACTUAL_WRITE", "VIOLATION_DETECTED_BY",
}

// SupplementEdgeLabels is the 6 supplement edge labels per D-001.06.
// Combined with MandatoryEdgeLabels these total 24 edge labels.
var SupplementEdgeLabels = []string{
	"BELONGS_TO", "OF_TABLE", "USED_ENGINE", "USES_DIMENSION",
	"RAN_ON", "GOVERNS",
}

// LabelWhitelist is the O(1)-lookup set of all 43 allowed label
// identifiers (19 vlabels + 18 mandatory elabels + 6 supplement
// elabels). It is the Phase 0 floor for the D-OQ.03 / ADR-004 Phase 5
// MCP hardening contract.
//
// Built at init() time from the three exported slices above so any
// drift between the slices and this set fails fast at startup.
//
// To check membership use IsAllowedLabel; do not mutate this map.
var LabelWhitelist map[string]struct{}

func init() {
	LabelWhitelist = make(map[string]struct{},
		len(NodeLabels)+len(MandatoryEdgeLabels)+len(SupplementEdgeLabels))
	for _, n := range NodeLabels {
		LabelWhitelist[n] = struct{}{}
	}
	for _, e := range MandatoryEdgeLabels {
		LabelWhitelist[e] = struct{}{}
	}
	for _, e := range SupplementEdgeLabels {
		LabelWhitelist[e] = struct{}{}
	}

	// Guardrail: D-001.05/.06 amended by D-003.06 requires exactly 43
	// identifiers. If anyone edits the slices without updating both
	// counts they'll trip this at the first import (test runs, build
	// of any cmd binary, etc.).
	if len(LabelWhitelist) != 43 {
		panic("graph.LabelWhitelist count != 43 — D-001.05/.06 amended by D-003.06 requires exactly 43")
	}
}

// IsAllowedLabel reports whether name is one of the 43 canonical Neksur
// label identifiers. Case-sensitive — Cypher labels are PascalCase for
// vertex labels and SCREAMING_SNAKE_CASE for edge labels.
//
// Any code path that takes a label name as a parameter (which AGE
// cannot parameterise via bind variables — labels are identifiers, not
// values) MUST run it through this function before splicing it into a
// Cypher string. See the package doc and tests/security/
// cypher_injection_test.go::TestStringConcatRejectedByWhitelist.
func IsAllowedLabel(name string) bool {
	_, ok := LabelWhitelist[name]
	return ok
}
