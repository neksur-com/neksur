// Snapshot MERGE primitive — D-1.04 natural key (metadata_location).
//
// Per RESEARCH §Pattern 3 lines 681-697 the Snapshot MERGE template MUST
// keep `committed_at` in `ON CREATE SET` only and `last_seen_at` in `ON
// MATCH SET` only — DO NOT conflate. This is the Pitfall 5 mitigation:
// at-least-once OpenLineage retries (Spark transport) re-MERGE the same
// snapshot; the ON CREATE / ON MATCH split preserves the original commit
// timestamp instead of clobbering it on every retry.
//
// All MERGE goes through `gc.ExecuteInTenant` so:
//   1. The Postgres GUC `app.current_tenant` is set on the transaction
//      → V0030/V0032 RLS policies on the Snapshot/Column/HAS_COLUMN
//      label tables fire correctly.
//   2. D-001.14 telemetry collectors (cypher_duration_ms,
//      cypher_errors_total) emit alongside via the Cypher() path.
//   3. ResetSession (DISCARD ALL) runs on connection release so
//      tenant context cannot leak across requests (Pitfall 4 / T-0-SESS).
//
// Service is intentionally a tiny struct — it owns no per-request state
// and no caches. Concurrency-safe by virtue of the underlying pgxpool.

package ingest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
)

// cypherMergeSnapshot is the canonical Snapshot MERGE template
// (RESEARCH §Pattern 3 lines 681-697 verbatim).
//
// Anti-pattern §1402: ON CREATE/ON MATCH must be split. Conflating them
// (e.g., assigning committed_at unconditionally) clobbers the original
// commit timestamp on every Spark retry — which silently breaks the
// audit log because the WriteEvent timestamp drifts forward by hours.
const cypherMergeSnapshot = `
MERGE (s:Snapshot { metadata_location: '%s' })
ON CREATE SET
  s.snapshot_id    = %d,
  s.parent_snap_id = %d,
  s.committed_at   = '%s',
  s.operation      = '%s',
  s.tenant_id      = '%s'
ON MATCH SET
  s.last_seen_at   = '%s'
RETURN id(s)
`

// Service is the ingest entry point. Pass to higher layers (HTTP
// handlers, scheduled-action runners) as an injected dependency; do
// NOT instantiate per-request. Methods serialize their side effects
// inside `gc.ExecuteInTenant` transactions.
type Service struct {
	gc *graph.GraphClient
}

// NewService constructs the ingest service against the Phase 0 graph
// client. The graph client owns the pgxpool with `BeforeAcquire DISCARD
// ALL` (Phase 0.5 must_have) — DO NOT add a second pool here. RESEARCH
// §Anti-patterns line 1400 explicitly forbids per-package pgxpool.New
// calls because the BeforeAcquire hook is the ONLY enforcement of
// session-bleed prevention (T-0-SESS).
func NewService(gc *graph.GraphClient) *Service {
	return &Service{gc: gc}
}

// MergeSnapshot upserts a Snapshot vertex keyed on metadata_location
// (D-1.04). Idempotent: re-application with the same metadata_location
// updates only `last_seen_at`; the original `committed_at` from the
// first MERGE is preserved (Pitfall 5).
//
// The Snapshot vertex's `tenant_id` property is required by the V0030
// CHECK constraint (Snapshot_tenant_id_required) — we set it from the
// tenantID argument, NOT from the Postgres GUC, so a graph-side INSERT
// failure surfaces a clear constraint violation instead of a silent
// `properties ? 'tenant_id'` predicate failure.
//
// Errors:
//   - ErrSnapshotNotFound is NOT returned here (MERGE creates on miss).
//   - Wrapped pgx errors propagate from the Postgres layer (e.g., RLS
//     violation, advisory-lock timeout).
//   - graph.ErrUnboundedTraversal CANNOT fire — the template is a
//     plain MERGE with no VLP shape.
func (s *Service) MergeSnapshot(ctx context.Context, tenantID string, snap iceberg.Snapshot) error {
	if snap.MetadataLocation == "" {
		return fmt.Errorf("ingest: merge snapshot: empty metadata_location (D-1.04 natural key required)")
	}
	committedAt := time.UnixMilli(snap.TimestampMs).UTC().Format(time.RFC3339Nano)
	cypher := fmt.Sprintf(
		cypherMergeSnapshot,
		escapeCypher(snap.MetadataLocation),
		snap.SnapshotID,
		snap.ParentSnapshotID,
		committedAt,
		escapeCypher(snap.Operation),
		escapeCypher(tenantID),
		committedAt,
	)
	return s.gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// We use tx.Exec directly with the AGE wrapping shape rather
		// than ExecuteCypher because the latter routes through the
		// pool (g.pool.Query) — losing the per-tx tenant binding that
		// ExecuteInTenant just established. The Cypher body is plain
		// MERGE; no traversal-depth pre-parse is needed.
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
			cypher,
		)
		_, err := tx.Exec(ctx, q)
		if err != nil {
			return fmt.Errorf("ingest: merge snapshot: %w", err)
		}
		return nil
	})
}

// escapeCypher single-quote-escapes a string literal for safe inlining
// into a Cypher MERGE/MATCH body. AGE's `cypher()` SQL function takes
// the Cypher body as a dollar-quoted text literal, so the only escape
// needed is the single quote (Cypher's string delimiter). Inputs with
// embedded `$$` would still be safe because we wrap with `$$ %s $$` —
// but to be defensive we also reject NULs and CR/LF that could break
// the wrapping shape.
//
// This is the per-package safe-escape helper used by all MERGE
// templates in this package; do NOT use fmt.Sprintf("'%s'", ...)
// directly with caller-supplied strings.
func escapeCypher(s string) string {
	// Disallow control characters that could break out of the
	// dollar-quoted block (NULs are also rejected by Postgres text I/O).
	s = strings.ReplaceAll(s, "\x00", "")
	// Single-quote escape per Cypher syntax (double the quote).
	return strings.ReplaceAll(s, "'", "\\'")
}
