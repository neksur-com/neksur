// Phase 1 regex-based PII classifier per ADR-007 §2.7 + Pitfall 11
// confidence scoring. Five tag patterns: SSN / email / credit card /
// phone / IBAN.
//
// Pitfall 11 — combined name+value scoring is the load-bearing
// false-positive mitigation. Column-name match alone (e.g., a column
// literally named `email` carrying integer values) yields confidence
// 0.65 (below the 0.85 alert threshold). Value-pattern match alone
// (e.g., a column named `note` containing one stray email-shaped
// string) yields confidence 0.55. Only the COMBINED match — name AND
// value — yields 0.92, the only path to a Slack alert. This prevents
// an attacker (or a poorly-named column) from triggering false alerts
// while correctly flagging real PII.
//
// Phase 2+ may add ML-based classification (ADR-007 §1 Phase 6); the
// graph emission shape (Tag + Classification + edges) is identical so
// downstream consumers don't change.

package regex

import "regexp"

// Tag is the Phase 1 PII tag definition (one per regex pattern). ID is
// the canonical tag identifier (e.g., `pii-ssn-us`); Name + Category
// are the human-readable labels surfaced in audit and Slack alerts.
type Tag struct {
	ID       string
	Name     string
	Category string
}

// PatternBank is one entry in the classifier's pattern list — a column-
// name regex (matched against the column identifier) plus a value regex
// (matched against sampled cell values) plus the Tag this pattern
// emits when a hit is recorded.
type PatternBank struct {
	NameRegex  *regexp.Regexp
	ValueRegex *regexp.Regexp
	Tag        Tag
}

// Phase1Bank is the canonical Phase 1 PII pattern set per ADR-007 §2.7.
// Five patterns:
//
//   - pii-ssn-us       — US Social Security Number (NNN-NN-NNNN or
//                        unformatted 9-digit run).
//   - pii-email        — RFC-5322-shaped email address.
//   - pii-credit-card  — 13-19 digit run with optional spaces / dashes.
//   - pii-phone        — phone number with optional `+` prefix and
//                        digit/space/dash/dot/paren punctuation.
//   - pii-iban         — IBAN (2 letters + 2 digits + alphanumeric).
//
// Regex shapes are intentionally LOOSE on the value side to maximize
// recall; the combined name+value scoring (Pitfall 11) is what
// suppresses false positives. The `(?i)` prefix on every name regex
// makes the column-name match case-insensitive (production data sees
// `SSN` / `ssn` / `Ssn` interchangeably).
var Phase1Bank = []PatternBank{
	{
		NameRegex:  regexp.MustCompile(`(?i)(ssn|social.?security)`),
		ValueRegex: regexp.MustCompile(`^\d{3}-\d{2}-\d{4}$|^\d{9}$`),
		Tag:        Tag{ID: "pii-ssn-us", Name: "US SSN", Category: "PII"},
	},
	{
		NameRegex:  regexp.MustCompile(`(?i)(email|e[-_]?mail)`),
		ValueRegex: regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`),
		Tag:        Tag{ID: "pii-email", Name: "Email", Category: "PII"},
	},
	{
		NameRegex:  regexp.MustCompile(`(?i)(credit.?card|cc|pan)`),
		ValueRegex: regexp.MustCompile(`^[\d\s-]{13,19}$`),
		Tag:        Tag{ID: "pii-credit-card", Name: "Credit Card", Category: "PCI"},
	},
	{
		NameRegex:  regexp.MustCompile(`(?i)(phone|tel)`),
		ValueRegex: regexp.MustCompile(`^\+?[\d\s\-().]{7,}$`),
		Tag:        Tag{ID: "pii-phone", Name: "Phone", Category: "PII"},
	},
	{
		NameRegex:  regexp.MustCompile(`(?i)(iban|account.?number)`),
		ValueRegex: regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{1,30}$`),
		Tag:        Tag{ID: "pii-iban", Name: "IBAN", Category: "PII"},
	},
}

// MatchType tags the kind of match a finding represents — one of the
// three Pitfall 11 confidence-scoring shapes.
const (
	MatchNameOnly     = "name_only"
	MatchValueOnly    = "value_only"
	MatchNameAndValue = "name_and_value"
)

// Confidence-score constants per Pitfall 11. Exposed so callers
// (gateway alerting, Slack threshold check) can branch on the same
// numeric values.
const (
	// ConfidenceNameAndValue — combined match (column name AND value
	// pattern both fire). Above the 0.85 alert threshold.
	ConfidenceNameAndValue = 0.92

	// ConfidenceNameOnly — column name matches but no value match.
	// Below 0.85 threshold; recorded for audit but no alert.
	ConfidenceNameOnly = 0.65

	// ConfidenceValueOnly — value pattern matches but column name
	// doesn't. Below 0.85; audit-only.
	ConfidenceValueOnly = 0.55

	// AlertThreshold — D-1.12 + D-OQ.07 — confidence ≥0.85 triggers
	// Slack alert. Phase 1 is fixed; Phase 6 may make per-tenant.
	AlertThreshold = 0.85
)

// ColumnFinding is the per-column classification result. One finding
// per column-tag pair (a column may produce multiple findings if it
// matches multiple PII tags simultaneously — uncommon in practice but
// supported).
type ColumnFinding struct {
	ColumnName  string
	TagID       string
	TagName     string
	TagCategory string
	Confidence  float64
	MatchType   string
}

// RegexClassifier is the Phase 1 regex-only classifier. Bank is the
// pattern list applied to each (column, sample-values) pair.
type RegexClassifier struct {
	bank []PatternBank
}

// NewRegexClassifier returns a classifier with the canonical Phase 1
// pattern bank. Production callers use this; tests may construct a
// classifier with a custom bank to exercise specific patterns.
func NewRegexClassifier() *RegexClassifier {
	return &RegexClassifier{bank: Phase1Bank}
}

// Classify runs every pattern in the bank against (columnName,
// sampleValues) and returns the resulting findings per Pitfall 11.
//
// For each pattern:
//
//  1. nameMatch := bank.NameRegex.MatchString(columnName)
//  2. valueMatch := any(sampleValues, bank.ValueRegex.MatchString)
//  3. confidence:
//     - nameMatch && valueMatch  → 0.92  (combined; alert)
//     - nameMatch && !valueMatch → 0.65  (name only; audit)
//     - !nameMatch && valueMatch → 0.55  (value only; audit)
//     - else                     → 0.0   (no finding emitted)
//
// Findings with confidence < 0.50 are not emitted (we drop the no-match
// case entirely). The 0.50 floor is the Phase 1 audit cutoff; below
// this we don't even record.
func (c *RegexClassifier) Classify(columnName string, sampleValues []string) []ColumnFinding {
	out := make([]ColumnFinding, 0, len(c.bank))
	for _, p := range c.bank {
		nameMatch := p.NameRegex.MatchString(columnName)
		valueMatch := false
		for _, v := range sampleValues {
			if p.ValueRegex.MatchString(v) {
				valueMatch = true
				break
			}
		}

		var confidence float64
		var matchType string
		switch {
		case nameMatch && valueMatch:
			confidence = ConfidenceNameAndValue
			matchType = MatchNameAndValue
		case nameMatch && !valueMatch:
			confidence = ConfidenceNameOnly
			matchType = MatchNameOnly
		case !nameMatch && valueMatch:
			confidence = ConfidenceValueOnly
			matchType = MatchValueOnly
		default:
			continue
		}

		if confidence < 0.50 {
			continue
		}

		out = append(out, ColumnFinding{
			ColumnName:  columnName,
			TagID:       p.Tag.ID,
			TagName:     p.Tag.Name,
			TagCategory: p.Tag.Category,
			Confidence:  confidence,
			MatchType:   matchType,
		})
	}
	return out
}
