//go:build integration

// Plan 01-07 Task 3 [BLOCKING] — regex classifier basic round-trip.
//
// Loads the pii_orders parquet fixture (Plan 01-01) and exercises the
// classifier against representative columns:
//
//   - email     → combined match → confidence ≥ 0.85.
//   - ssn       → combined match → confidence ≥ 0.85.
//   - customer_id → no SSN finding ≥ 0.85 (Pitfall 11 mitigation).
//
// The fixture file is read via stdlib `os` for the file's existence
// check; column-name + sample-value pairs are hand-curated to match
// the gen_pii_parquet.py distribution (the parquet itself isn't read
// — Phase 1 doesn't yet include a parquet reader at the test layer).
// The classifier surface is the same code path that the production
// regexScanner exercises in cmd/neksur-server.

package integration

import (
	"os"
	"testing"

	"github.com/neksur-com/neksur/internal/detect/regex"
)

// TestDetectRegexBasic — the BLOCKING gate for the Pitfall 11
// confidence-scoring contract.
func TestDetectRegexBasic(t *testing.T) {
	// Confirm the fixture exists; the fixture is committed by Plan 01-01.
	const fixturePath = "fixtures/pii_orders.parquet"
	if _, err := os.Stat(fixturePath); err != nil {
		t.Fatalf("missing PII fixture %s: %v (Plan 01-01 should have committed it)", fixturePath, err)
	}

	c := regex.NewRegexClassifier()

	// Test 1 — email column with email-shaped values → combined match.
	emailFindings := c.Classify("email", []string{
		"user1@acme.example",
		"user2@example.com",
		"user3@test.example",
	})
	emailHit := findFindingByTagID(emailFindings, "pii-email")
	if emailHit == nil {
		t.Fatalf("expected pii-email finding for combined match; got %+v", emailFindings)
	}
	if emailHit.Confidence < regex.AlertThreshold {
		t.Errorf("email combined Confidence = %f; want ≥ %f (Slack alert threshold)",
			emailHit.Confidence, regex.AlertThreshold)
	}
	if emailHit.MatchType != regex.MatchNameAndValue {
		t.Errorf("email MatchType = %q; want %q", emailHit.MatchType, regex.MatchNameAndValue)
	}

	// Test 2 — ssn column with SSN-shaped values → combined match.
	ssnFindings := c.Classify("ssn", []string{
		"123-45-6789",
		"987654321",
		"234-56-7890",
	})
	ssnHit := findFindingByTagID(ssnFindings, "pii-ssn-us")
	if ssnHit == nil {
		t.Fatalf("expected pii-ssn-us finding; got %+v", ssnFindings)
	}
	if ssnHit.Confidence < regex.AlertThreshold {
		t.Errorf("ssn combined Confidence = %f; want ≥ %f", ssnHit.Confidence, regex.AlertThreshold)
	}

	// Test 3 — Pitfall 11 mitigation: customer_id with 5-digit integer
	// values MUST NOT produce an SSN finding ≥ 0.85.
	custFindings := c.Classify("customer_id", []string{
		"12345", "67890", "11111",
	})
	for _, f := range custFindings {
		if f.TagID == "pii-ssn-us" && f.Confidence >= regex.AlertThreshold {
			t.Errorf("Pitfall 11 false-positive: customer_id produced SSN finding %+v", f)
		}
	}
}

// findFindingByTagID — local helper, mirrors the unit-test helper in
// internal/detect/regex/classifier_test.go.
func findFindingByTagID(findings []regex.ColumnFinding, tagID string) *regex.ColumnFinding {
	for i, f := range findings {
		if f.TagID == tagID {
			return &findings[i]
		}
	}
	return nil
}
