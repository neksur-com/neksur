// Package ingest holds the catalog-metadata-to-graph MERGE primitives —
// Snapshot, Column, HAS_COLUMN edge (D-1.05), and the LINEAGE_OF edge
// with cycle pre-check (D-1.06) + advisory lock serialization
// (Pitfall 4) + bounded `*1..5` traversal (D-001.08).
//
// All MERGE templates use ON CREATE / ON MATCH split per Pitfall 5 so
// at-least-once retries (Spark OpenLineage transport) do NOT overwrite
// the original `committed_at` timestamp.
//
// Sentinel + typed-error pattern (PATTERNS CC5):
//   - LineageCycleError is a typed struct carrying SourceID/TargetID/Cycle
//     so callers + operators can read the offending lineage chain.
//   - ErrLineageCycle is a bare sentinel that LineageCycleError.Is matches,
//     so callers can write `errors.Is(err, ErrLineageCycle)` for boolean
//     branching without losing the typed cycle path on errors.As.
//   - ErrSnapshotNotFound / ErrColumnSnapshotMismatch / ErrLineageBatchEmpty
//     are bare sentinels for the simple validation paths.
package ingest

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSnapshotNotFound is returned by MergeColumns when the referenced
// Snapshot (matched by metadata_location) does not exist in the graph.
// Callers should ingest the Snapshot first via MergeSnapshot.
var ErrSnapshotNotFound = errors.New("ingest: snapshot not found")

// ErrColumnSnapshotMismatch is returned when a Column row's
// snapshot_loc property does not match the Snapshot it is being
// attached to. This indicates a programming error in the calling code,
// not a recoverable runtime condition.
var ErrColumnSnapshotMismatch = errors.New("ingest: column snapshot_loc does not match")

// ErrLineageBatchEmpty is returned when MergeLineageEdge or the OpenLineage
// translator receives a batch with no source/target pairs to merge.
var ErrLineageBatchEmpty = errors.New("ingest: lineage batch empty")

// ErrLineageCycle is the sentinel a *LineageCycleError matches via Is.
// Callers that only care WHETHER a cycle was detected (and not the
// path) write `errors.Is(err, ingest.ErrLineageCycle)`. Callers that
// want to surface the cycle path (e.g., 422 responses on the L1
// gateway) use `errors.As(err, &cyc)` to recover the typed struct.
var ErrLineageCycle = errors.New("ingest: lineage cycle")

// LineageCycleError reports a lineage cycle detected at ingest time.
// Cycle is the ordered URI path that closes the cycle (e.g., A → B →
// C → A) so operators can debug the offending chain. Per CONTEXT
// specifics line 171: "format matches Phase 0's LineageCycleError"
// (Phase 0 introduced the placeholder; Phase 1 ships the concrete type).
type LineageCycleError struct {
	SourceID string   // The source URI that would have closed the cycle.
	TargetID string   // The target URI that would have received the edge.
	Cycle    []string // Ordered URI path (e.g., A → B → C → A).
}

// Error implements the error interface. The format prints the cycle
// path joined by " -> " plus the source/target URIs for unambiguous
// debugging (RESEARCH §Pattern 3 lines 765-774).
func (e *LineageCycleError) Error() string {
	return fmt.Sprintf(
		"ingest: lineage cycle detected: %s (source=%s, target=%s)",
		strings.Join(e.Cycle, " -> "), e.SourceID, e.TargetID,
	)
}

// Is supports `errors.Is(err, ErrLineageCycle)` boolean discrimination
// while preserving the typed-struct field access via errors.As.
func (e *LineageCycleError) Is(target error) bool {
	return target == ErrLineageCycle
}
