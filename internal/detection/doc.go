// Package detection hosts the L3 post-commit detection worker. Per
// docs/phase-0-stack.md §6 this package will contain worker.go (poll
// loop over fresh catalog snapshots), classifier.go (regex + sampling
// PII classifier for Phase 0; ML anomaly detection is Phase 2
// per §2.9), and notifier.go (alert dispatch).
//
// Phase 0 status: placeholder. M3 lands the basic snapshot poll +
// regex classifier per ADR-003 §12 (L3 enforcement layer).
package detection
