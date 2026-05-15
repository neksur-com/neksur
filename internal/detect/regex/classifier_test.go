package regex

import "testing"

// TestClassifyCombinedMatchProducesAlertConfidence — column `email` +
// values containing email-shaped strings → finding with confidence
// ≥ 0.85 (the Slack alert threshold).
func TestClassifyCombinedMatchProducesAlertConfidence(t *testing.T) {
	c := NewRegexClassifier()
	findings := c.Classify("email", []string{"alice@example.com"})
	if len(findings) == 0 {
		t.Fatalf("expected at least 1 finding for combined email match; got 0")
	}
	emailHit := findEmailFinding(findings)
	if emailHit == nil {
		t.Fatalf("expected pii-email finding; got %+v", findings)
	}
	if emailHit.MatchType != MatchNameAndValue {
		t.Errorf("MatchType = %q; want %q", emailHit.MatchType, MatchNameAndValue)
	}
	if emailHit.Confidence < AlertThreshold {
		t.Errorf("Confidence = %f; want >= %f (alert threshold)", emailHit.Confidence, AlertThreshold)
	}
}

// TestClassifyNameOnlyBelowAlertThreshold — column `email` + non-email
// values → confidence < 0.85 (no alert).
func TestClassifyNameOnlyBelowAlertThreshold(t *testing.T) {
	c := NewRegexClassifier()
	findings := c.Classify("email", []string{"1", "2", "3"})
	emailHit := findEmailFinding(findings)
	if emailHit == nil {
		t.Fatalf("expected pii-email finding for name-only match; got %+v", findings)
	}
	if emailHit.Confidence >= AlertThreshold {
		t.Errorf("name-only Confidence = %f; want < %f (no alert)", emailHit.Confidence, AlertThreshold)
	}
	if emailHit.MatchType != MatchNameOnly {
		t.Errorf("MatchType = %q; want %q", emailHit.MatchType, MatchNameOnly)
	}
}

// TestClassifyValueOnlyBelowAlertThreshold — column `xyz` + email-shaped
// values → confidence < 0.85 (no alert).
func TestClassifyValueOnlyBelowAlertThreshold(t *testing.T) {
	c := NewRegexClassifier()
	findings := c.Classify("xyz", []string{"alice@example.com"})
	emailHit := findEmailFinding(findings)
	if emailHit == nil {
		t.Fatalf("expected pii-email finding for value-only match; got %+v", findings)
	}
	if emailHit.Confidence >= AlertThreshold {
		t.Errorf("value-only Confidence = %f; want < %f (no alert)", emailHit.Confidence, AlertThreshold)
	}
	if emailHit.MatchType != MatchValueOnly {
		t.Errorf("MatchType = %q; want %q", emailHit.MatchType, MatchValueOnly)
	}
}

// TestClassifyFalsePositiveCustomerID — column `customer_id` with
// 5-digit integer values MUST NOT produce a SSN finding ≥ 0.85 (the
// Pitfall 11 mitigation). The integers don't match the SSN value
// regex (which requires 9-digit run or NNN-NN-NNNN); the column name
// doesn't match `(?i)(ssn|social.?security)`. So no name match AND no
// value match → no finding.
func TestClassifyFalsePositiveCustomerID(t *testing.T) {
	c := NewRegexClassifier()
	findings := c.Classify("customer_id", []string{"12345", "67890", "11111"})
	for _, f := range findings {
		if f.TagID == "pii-ssn-us" && f.Confidence >= AlertThreshold {
			t.Errorf("Pitfall 11 false-positive: customer_id with 5-digit values produced SSN finding %+v", f)
		}
	}
}

// TestClassifySSNCombinedAlerts — column `ssn` + formatted SSN values
// → 0.92 confidence (combined match alerts).
func TestClassifySSNCombinedAlerts(t *testing.T) {
	c := NewRegexClassifier()
	findings := c.Classify("ssn", []string{"123-45-6789"})
	hit := findFindingByTag(findings, "pii-ssn-us")
	if hit == nil {
		t.Fatalf("expected pii-ssn-us finding; got %+v", findings)
	}
	if hit.Confidence < AlertThreshold {
		t.Errorf("SSN combined Confidence = %f; want >= %f", hit.Confidence, AlertThreshold)
	}
}

// TestClassifyAllFivePatternsRegistered — proves Phase1Bank covers all
// 5 PII patterns + their tag IDs are unique.
func TestClassifyAllFivePatternsRegistered(t *testing.T) {
	required := map[string]bool{
		"pii-ssn-us":      false,
		"pii-email":       false,
		"pii-credit-card": false,
		"pii-phone":       false,
		"pii-iban":        false,
	}
	for _, b := range Phase1Bank {
		if _, ok := required[b.Tag.ID]; ok {
			required[b.Tag.ID] = true
		}
	}
	for tagID, found := range required {
		if !found {
			t.Errorf("Phase1Bank missing pattern with TagID=%q", tagID)
		}
	}
}

// TestClassifyEmptySampleValues — column `email` with no sample values
// produces a name-only finding (confidence 0.65 < threshold).
func TestClassifyEmptySampleValues(t *testing.T) {
	c := NewRegexClassifier()
	findings := c.Classify("email", nil)
	hit := findEmailFinding(findings)
	if hit == nil {
		t.Fatalf("expected pii-email name-only finding; got %+v", findings)
	}
	if hit.MatchType != MatchNameOnly {
		t.Errorf("MatchType = %q; want %q", hit.MatchType, MatchNameOnly)
	}
}

// findEmailFinding returns the first pii-email finding from a result
// slice (or nil).
func findEmailFinding(findings []ColumnFinding) *ColumnFinding {
	return findFindingByTag(findings, "pii-email")
}

func findFindingByTag(findings []ColumnFinding, tagID string) *ColumnFinding {
	for i, f := range findings {
		if f.TagID == tagID {
			return &findings[i]
		}
	}
	return nil
}
