// Unit tests for CompiledPolicyStatus.IsValid — Plan 03-01 Task 1.
//
// Validates that the Phase 3 divergent_suspended enum extension (D-3.05)
// is accepted by IsValid() and that unknown values are rejected. No
// integration tag — these run in the fast unit-test path.

package store

import "testing"

func TestCompiledPolicyStatusIsValid(t *testing.T) {
	valid := []CompiledPolicyStatus{
		CompiledPolicyStatusPending,
		CompiledPolicyStatusActive,
		CompiledPolicyStatusProbeFailed,
		CompiledPolicyStatusCompileFailed,
		CompiledPolicyStatusDivergentSuspended, // Phase 3 D-3.05 addition
	}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("IsValid(%q) = false; want true", s)
		}
	}

	invalid := []CompiledPolicyStatus{
		"",
		"unknown",
		"PENDING",             // case-sensitive
		"divergent_suspended_", // trailing underscore
	}
	for _, s := range invalid {
		if s.IsValid() {
			t.Errorf("IsValid(%q) = true; want false", s)
		}
	}
}
