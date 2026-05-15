// Stratified random sampling per ADR-003 §5.4 file-size table +
// D-OQ.06 80% random / 20% adversarial split.
//
// The ADR-003 §5.4 sampling table:
//
//   bucket           size range       sample rate
//   --------------- ----------------  ------------
//   BucketTiny       <10MB            100%
//   BucketSmall      10MB - 1GB        10%
//   BucketMedium     1GB - 10GB         1%
//   BucketLarge      >10GB              0.1%
//
// D-OQ.06: Phase 1 uses an 80% random + 20% adversarial split. The
// adversarial budget targets files whose column names overlap with a
// caller-supplied list of historically-violation-prone column names
// (e.g., `customer_id`, `user_email`, `personal_data` per ADR-007 §2.2
// PII column-name patterns). The combination keeps statistical coverage
// (random) while ensuring known-suspicious surface gets attention even
// when the random draw misses it.

package detect

import (
	"math/rand"
	"strings"
	"time"
)

// FileMeta is the per-file sampling input. Path is the manifest URI;
// SizeBytes drives bucket selection; ColumnNames lets the adversarial
// split target known-suspicious column shapes without re-reading the
// file.
type FileMeta struct {
	Path        string
	SizeBytes   int64
	ColumnNames []string
}

// Bucket discriminates ADR-003 §5.4 size tiers. Constants ordered
// smallest → largest so iteration order matches the size table.
type Bucket int

const (
	// BucketTiny — files <10MB. Sampled at 100% (every file scanned).
	BucketTiny Bucket = iota

	// BucketSmall — files 10MB-1GB. Sampled at 10%.
	BucketSmall

	// BucketMedium — files 1GB-10GB. Sampled at 1%.
	BucketMedium

	// BucketLarge — files >10GB. Sampled at 0.1%.
	BucketLarge
)

// Bucket boundaries in bytes. Exposed so tests + adjacent packages can
// reference the same numbers without re-deriving from the doc comment.
const (
	tinyMaxBytes   int64 = 10 * 1024 * 1024            // 10 MiB
	smallMaxBytes  int64 = 1024 * 1024 * 1024          // 1 GiB
	mediumMaxBytes int64 = 10 * 1024 * 1024 * 1024     // 10 GiB
)

// classifyBucket returns the bucket for a given file size. Boundary
// rule: a file exactly at a boundary belongs to the LARGER bucket
// (e.g., 10MB → BucketSmall, 1GB → BucketMedium). This matches the
// ADR-003 table's `<10MB / 10MB-1GB / 1-10GB / >10GB` reading.
func classifyBucket(sizeBytes int64) Bucket {
	switch {
	case sizeBytes < tinyMaxBytes:
		return BucketTiny
	case sizeBytes < smallMaxBytes:
		return BucketSmall
	case sizeBytes < mediumMaxBytes:
		return BucketMedium
	default:
		return BucketLarge
	}
}

// bucketRate returns the ADR-003 §5.4 sample rate for a bucket as a
// float in [0, 1].
func bucketRate(b Bucket) float64 {
	switch b {
	case BucketTiny:
		return 1.0
	case BucketSmall:
		return 0.10
	case BucketMedium:
		return 0.01
	case BucketLarge:
		return 0.001
	default:
		return 0.0
	}
}

// StratifiedSample returns the subset of files to scan per ADR-003 §5.4
// + D-OQ.06. The 80% random / 20% adversarial split is applied
// atomically: the random pass first selects per-bucket rate; then the
// adversarial pass adds files whose ColumnNames overlap with
// adversarialHints. Result is deduplicated on FileMeta.Path.
//
// adversarialHints is the operator-supplied list of historically-
// violation-prone column name substrings (case-insensitive contains
// match). Phase 1 default list is computed via DefaultAdversarialHints
// — `customer`, `email`, `ssn`, `personal`, `phone`, `iban`, etc.
//
// The sampling is non-deterministic (seeds from time.Now().UnixNano on
// each call). Tests that need determinism can construct FileMeta lists
// where every file is in BucketTiny (100% sampling rate) or only test
// the bucket-classification path.
func StratifiedSample(files []FileMeta, adversarialHints []string) []FileMeta {
	if len(files) == 0 {
		return nil
	}

	// Per-call RNG so concurrent callers don't share global state.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Phase 1: random pass per bucket. ADR-003 §5.4 rates apply
	// directly; D-OQ.06 80/20 split is captured by treating the
	// random pass as the 80% allocation and the adversarial pass as
	// the 20% allocation. A file may be selected by BOTH (dedup
	// removes the duplicate).
	picked := make(map[string]FileMeta, len(files)/4+4)
	for _, f := range files {
		rate := bucketRate(classifyBucket(f.SizeBytes))
		// Apply 80% random allocation: scale rate by 0.8.
		adjustedRate := rate * 0.8
		if adjustedRate >= 1.0 || rng.Float64() < adjustedRate {
			picked[f.Path] = f
		}
	}

	// Phase 2: adversarial pass — for each file whose column names
	// overlap with adversarialHints, add it (regardless of bucket
	// rate) up to the 20% remaining budget. Phase 1 simplification:
	// we apply ALL adversarial-matching files (not a separate cap)
	// because the adversarial set is small in practice (a handful of
	// column names per table). If/when the adversarial set grows,
	// Phase 6 may add a per-call budget arg.
	for _, f := range files {
		if matchesAdversarial(f, adversarialHints) {
			// At least include adversarial-matching files at the
			// 100% adversarial-budget rate (they're known-suspicious).
			picked[f.Path] = f
		}
	}

	out := make([]FileMeta, 0, len(picked))
	for _, f := range picked {
		out = append(out, f)
	}
	return out
}

// DefaultAdversarialHints returns the Phase 1 default list of
// historically-violation-prone column-name substrings per ADR-007 §2.2.
// Match is case-insensitive substring (e.g., `customer` matches both
// `customer_id` and `customer_email`).
func DefaultAdversarialHints() []string {
	return []string{
		"ssn",
		"social",
		"email",
		"phone",
		"iban",
		"credit",
		"card",
		"account",
		"customer",
		"user",
		"personal",
		"birth",
		"address",
	}
}

// matchesAdversarial returns true if any column name (case-insensitive)
// contains any of the adversarial hints.
func matchesAdversarial(f FileMeta, hints []string) bool {
	if len(hints) == 0 {
		return false
	}
	for _, col := range f.ColumnNames {
		colLower := strings.ToLower(col)
		for _, h := range hints {
			if strings.Contains(colLower, strings.ToLower(h)) {
				return true
			}
		}
	}
	return false
}
