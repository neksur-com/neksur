package graph

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// ValidateTraversalDepth — D-001.08 depth-cap pre-parser (no DB needed).
//
// Five rejection / acceptance cases, matching the Python-tier coverage of
// tests/security/test_depth_cap.py exactly:
//   - Bare `*` rejected
//   - `*1..` rejected (lower-only, open upper)
//   - `*..` rejected (no bounds)
//   - `*1..5` accepted (both bounds)
//   - `*1..3` accepted (default depth per D-001.08)
//
// Plus extra coverage cases inherited from the Python tier:
//   - `*..3` accepted (upper-only, lower defaults to 1)
//   - `*3` accepted (exact length — bounded by definition)
//   - Plain MATCH with no VLP accepted
//   - Multi-line query with bare `*` still rejected (MULTILINE-safe regex)
//   - `*10..` rejected (high lower bound, still open upper)
// ---------------------------------------------------------------------------

func TestRejectsBareAsterisk(t *testing.T) {
	err := ValidateTraversalDepth("MATCH p=(a)-[*]->(b) RETURN p")
	if !errors.Is(err, ErrUnboundedTraversal) {
		t.Fatalf("expected ErrUnboundedTraversal, got %v", err)
	}
}

func TestRejectsOpenStart(t *testing.T) {
	err := ValidateTraversalDepth("MATCH p=(a)-[*1..]->(b) RETURN p")
	if !errors.Is(err, ErrUnboundedTraversal) {
		t.Fatalf("expected ErrUnboundedTraversal for *1.., got %v", err)
	}
	// Also a high lower bound — same rejection.
	err = ValidateTraversalDepth("MATCH p=(a)-[*10..]->(b) RETURN p")
	if !errors.Is(err, ErrUnboundedTraversal) {
		t.Fatalf("expected ErrUnboundedTraversal for *10.., got %v", err)
	}
}

func TestRejectsOpenEnd(t *testing.T) {
	err := ValidateTraversalDepth("MATCH p=(a)-[*..]->(b) RETURN p")
	if !errors.Is(err, ErrUnboundedTraversal) {
		t.Fatalf("expected ErrUnboundedTraversal for *.., got %v", err)
	}
}

func TestAcceptsBoundedRange(t *testing.T) {
	if err := ValidateTraversalDepth("MATCH p=(a)-[*1..5]->(b) RETURN p"); err != nil {
		t.Fatalf("*1..5 must be accepted, got %v", err)
	}
	if err := ValidateTraversalDepth("MATCH p=(a)-[*2..4]->(b) RETURN p"); err != nil {
		t.Fatalf("*2..4 must be accepted, got %v", err)
	}
}

func TestAcceptsDefault(t *testing.T) {
	// D-001.08 default depth 3 — *1..3 is the canonical bounded form.
	if err := ValidateTraversalDepth("MATCH p=(a)-[*1..3]->(b) RETURN p"); err != nil {
		t.Fatalf("*1..3 (default depth) must be accepted, got %v", err)
	}
}

func TestAcceptsUpperOnly(t *testing.T) {
	// Per D-001.08: `*..M` is bounded — lower defaults to 1, upper is M.
	if err := ValidateTraversalDepth("MATCH p=(a)-[*..3]->(b) RETURN p"); err != nil {
		t.Fatalf("*..3 (upper-only bounded) must be accepted, got %v", err)
	}
}

func TestAcceptsExactLength(t *testing.T) {
	// `*N` is exact length — bounded by definition.
	if err := ValidateTraversalDepth("MATCH p=(a)-[*3]->(b) RETURN p"); err != nil {
		t.Fatalf("*3 (exact length) must be accepted, got %v", err)
	}
}

func TestAcceptsQueryWithoutVLP(t *testing.T) {
	// A plain MATCH with no variable-length traversal is untouched.
	if err := ValidateTraversalDepth(
		"MATCH (t:Table {uri: 'iceberg://x/y/z'}) RETURN t",
	); err != nil {
		t.Fatalf("plain MATCH must be accepted, got %v", err)
	}
}

func TestRejectsMultilineUnbounded(t *testing.T) {
	// Multi-line query containing a bare `*` is still rejected.
	q := "MATCH p=(a)-[*]->(b)\nWHERE a.tenant_id = 'X'\nRETURN p"
	if err := ValidateTraversalDepth(q); !errors.Is(err, ErrUnboundedTraversal) {
		t.Fatalf("multi-line bare * must be rejected, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// LabelWhitelist — D-001.05/.06 amended by D-003.06 (43 identifiers).
//
// Verifies the count, identity, and IsAllowedLabel behaviour. Includes
// the negative case (`Table; DROP TABLE foo --`) for parity with the
// Python tier's TestStringConcatRejectedByWhitelist.
// ---------------------------------------------------------------------------

func TestLabelWhitelistCount43(t *testing.T) {
	if len(LabelWhitelist) != 43 {
		t.Fatalf("LabelWhitelist len = %d; D-001.05/.06 amended by D-003.06 requires 43",
			len(LabelWhitelist))
	}
}

func TestLabelWhitelistAcceptsCanonical(t *testing.T) {
	// Spot-check one identifier from each slice to make sure the init
	// hook merged all three correctly.
	for _, want := range []string{
		"Table", "Snapshot", "DetectionRun", // NodeLabels
		"LINEAGE_OF", "VIOLATION_DETECTED_BY", // MandatoryEdgeLabels
		"BELONGS_TO", "GOVERNS", // SupplementEdgeLabels
	} {
		if !IsAllowedLabel(want) {
			t.Errorf("IsAllowedLabel(%q) = false; expected true", want)
		}
	}
}

func TestLabelWhitelistRejectsLowercase(t *testing.T) {
	// Cypher labels are case-sensitive (PascalCase for vlabels).
	if IsAllowedLabel("table") {
		t.Errorf("IsAllowedLabel(\"table\") = true; expected false (case-sensitive)")
	}
}

func TestLabelWhitelistRejectsInjection(t *testing.T) {
	// The smoking gun for the cypher_injection_test: an arbitrary
	// label string with a SQL payload must NOT be in the whitelist.
	malicious := "Table; DROP TABLE foo --"
	if IsAllowedLabel(malicious) {
		t.Fatalf("IsAllowedLabel(%q) = true; expected false (injection attempt)", malicious)
	}
}

func TestNodeLabelsLen19(t *testing.T) {
	if len(NodeLabels) != 19 {
		t.Fatalf("NodeLabels len = %d; D-001.05 amended by D-003.06 requires 19",
			len(NodeLabels))
	}
}

func TestEdgeLabelsLen24(t *testing.T) {
	got := len(MandatoryEdgeLabels) + len(SupplementEdgeLabels)
	if got != 24 {
		t.Fatalf("MandatoryEdgeLabels(%d) + SupplementEdgeLabels(%d) = %d; D-001.06 amended by D-003.06 requires 24",
			len(MandatoryEdgeLabels), len(SupplementEdgeLabels), got)
	}
}

// ---------------------------------------------------------------------------
// quoteSQLString — defence-in-depth for the AGE graph name interpolation.
// ---------------------------------------------------------------------------

func TestQuoteSQLStringEscapesSingleQuote(t *testing.T) {
	if got := quoteSQLString("neksur"); got != "'neksur'" {
		t.Errorf("quoteSQLString(\"neksur\") = %q; want %q", got, "'neksur'")
	}
	// Even though the graph name is application-fixed, defence-in-depth.
	if got := quoteSQLString("a'b"); got != "'a''b'" {
		t.Errorf("quoteSQLString(\"a'b\") = %q; want %q", got, "'a''b'")
	}
}
