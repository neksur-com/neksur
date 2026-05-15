package detect

import (
	"strings"
	"testing"
)

// TestStratifiedSamplingBucketBoundaries proves classifyBucket places
// edge-case file sizes into the correct ADR-003 §5.4 buckets.
//
//   - 9 MiB  → BucketTiny    (just under 10MB cap; 100% sampled)
//   - 10 MiB → BucketSmall   (boundary belongs to larger bucket)
//   - 100 MiB → BucketSmall   (10% sampled)
//   - 1 GiB  → BucketMedium  (boundary)
//   - 5 GiB  → BucketMedium  (1% sampled)
//   - 10 GiB → BucketLarge   (boundary)
//   - 50 GiB → BucketLarge   (0.1% sampled)
func TestStratifiedSamplingBucketBoundaries(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want Bucket
	}{
		{"9MB tiny", 9 * 1024 * 1024, BucketTiny},
		{"10MB small (boundary)", 10 * 1024 * 1024, BucketSmall},
		{"100MB small", 100 * 1024 * 1024, BucketSmall},
		{"1GB medium (boundary)", 1024 * 1024 * 1024, BucketMedium},
		{"5GB medium", 5 * 1024 * 1024 * 1024, BucketMedium},
		{"10GB large (boundary)", 10 * 1024 * 1024 * 1024, BucketLarge},
		{"50GB large", 50 * 1024 * 1024 * 1024, BucketLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyBucket(c.size)
			if got != c.want {
				t.Errorf("classifyBucket(%d) = %d; want %d", c.size, got, c.want)
			}
		})
	}
}

// TestStratifiedSampleAllTinyAlwaysIncluded — when every file is
// BucketTiny the stratified sample always includes 100% (after the
// 80% scale, BucketTiny's adjusted rate is 0.8, but the >=1.0 short-
// circuit means BucketTiny still goes to 100% only when the raw rate
// is >=1.0). Test that the sample size is strictly positive (deflakes
// the random pass with a high-probability assertion: with 100 files
// each at adjusted rate 0.8, the expected sample size is 80; we assert
// > 60 to allow slack).
func TestStratifiedSampleAllTinyHighRate(t *testing.T) {
	files := make([]FileMeta, 100)
	for i := range files {
		files[i] = FileMeta{
			Path:      paths(i),
			SizeBytes: 1024 * 1024, // 1MB → tiny
		}
	}
	picked := StratifiedSample(files, nil)
	if len(picked) < 60 {
		t.Errorf("stratified sample of 100 tiny files returned only %d; want >60 (BucketTiny 80%% adjusted)",
			len(picked))
	}
}

// TestAdversarialBudgetSplit80_20 — the adversarial pass adds files
// whose column names match adversarialHints regardless of bucket rate.
// We seed 100 BucketLarge files (0.1% sampling rate; expected random
// pick is ~0.08 files) plus 5 files whose ColumnNames include the
// `email` hint. The result MUST include all 5 hinted files.
func TestAdversarialBudgetSplit80_20(t *testing.T) {
	files := make([]FileMeta, 105)
	for i := 0; i < 100; i++ {
		files[i] = FileMeta{
			Path:        paths(i),
			SizeBytes:   50 * 1024 * 1024 * 1024, // 50GB → large
			ColumnNames: []string{"plain_int", "plain_float"},
		}
	}
	for i := 100; i < 105; i++ {
		files[i] = FileMeta{
			Path:        paths(i),
			SizeBytes:   50 * 1024 * 1024 * 1024,
			ColumnNames: []string{"customer_email"},
		}
	}

	picked := StratifiedSample(files, []string{"email"})

	// Confirm all 5 hinted files made the sample.
	hits := 0
	for _, p := range picked {
		for _, c := range p.ColumnNames {
			if strings.Contains(strings.ToLower(c), "email") {
				hits++
			}
		}
	}
	if hits != 5 {
		t.Errorf("adversarial pass picked %d email-hinted files; want 5", hits)
	}
}

// TestStratifiedSampleEmptyInput returns nil for empty input.
func TestStratifiedSampleEmptyInput(t *testing.T) {
	got := StratifiedSample(nil, nil)
	if got != nil {
		t.Errorf("StratifiedSample(nil) = %v; want nil", got)
	}
}

// TestDefaultAdversarialHintsIncludesPIITerms — the default list
// MUST cover the 5 Phase 1 PII patterns (ssn / email / credit card /
// phone / iban) so the adversarial pass actually targets them.
func TestDefaultAdversarialHintsIncludesPIITerms(t *testing.T) {
	hints := DefaultAdversarialHints()
	required := []string{"ssn", "email", "credit", "phone", "iban"}
	for _, r := range required {
		found := false
		for _, h := range hints {
			if h == r {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultAdversarialHints missing %q (5 Phase 1 PII patterns)", r)
		}
	}
}

// paths returns a stable s3:// path for index i — keeps test files'
// Path strings deterministic so the dedup-by-path semantic is testable.
func paths(i int) string {
	return "s3://test-bucket/data/" + itoa(i) + ".parquet"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	negative := false
	if i < 0 {
		i = -i
		negative = true
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+(i%10))) + digits
		i /= 10
	}
	if negative {
		digits = "-" + digits
	}
	return digits
}
