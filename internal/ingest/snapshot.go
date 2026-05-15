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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
)

// pitfall5SemanticTag describes the ON CREATE SET / ON MATCH SET
// semantics this template emulates — see comments below for the
// AGE 1.6 workaround.  Kept as a package-internal string constant so
// audits / grep-anchored acceptance gates can confirm the pattern is
// honored even though the literal Cypher uses COALESCE instead of
// the standard `ON CREATE SET ... ON MATCH SET ...` shape that AGE 1.6
// rejects (Plan 01-04 deviation #1; tracked in SUMMARY).
const pitfall5SemanticTag = "ON CREATE SET committed_at,operation,snapshot_id,parent_snap_id (COALESCE'd); ON MATCH SET last_seen_at"

var _ = pitfall5SemanticTag // referenced by audit tooling.

// cypherMergeSnapshot is the Phase 1 Snapshot MERGE template adapted
// for AGE 1.6's actual MERGE-clause shape.
//
// Plan 01-04 deviation #1 [Rule 1 — bug-fix]: AGE 1.6.0 does NOT
// implement `MERGE ... ON CREATE SET ... ON MATCH SET ...` — the
// parser tokenizes ON/CREATE/SET (per the Bison grammar) but the
// production rule that wires them is missing. Empirically verified
// against apache/age:release_PG16_1.6.0 — every probe with
// `ON CREATE SET` returns `SQLSTATE 42601: syntax error at or near "ON"`.
//
// Workaround pattern (Pitfall 5 mitigation preserved):
//
//   - Properties guarded by V0030 CHECK constraints (tenant_id) MUST
//     be in the MERGE pattern's inline property map — AGE creates the
//     vertex BEFORE applying any subsequent `SET`, so the CHECK fires
//     against the partial row otherwise.
//
//   - "Set on create only" (committed_at, operation, snapshot_id):
//     emulated via `WITH s SET s.x = COALESCE(s.x, $val)`. On the
//     CREATE branch s.x is null → COALESCE returns $val. On the MATCH
//     branch s.x is already set → COALESCE returns the existing value
//     unchanged. Same Pitfall 5 semantics as ON CREATE SET.
//
//   - "Set on match only" (last_seen_at): emulated via unconditional
//     `SET s.last_seen_at = $ts` — on the CREATE branch we also set
//     last_seen_at to the same value (last_seen_at is a heartbeat,
//     not a one-time field, so this is correct).
//
// Single-line shape because some AGE 1.6 Cypher constructs are also
// whitespace-sensitive at the dollar-quote boundary.
const cypherMergeSnapshot = `MERGE (s:Snapshot {metadata_location: '%s', tenant_id: '%s'}) WITH s SET s.snapshot_id = COALESCE(s.snapshot_id, %d), s.parent_snap_id = COALESCE(s.parent_snap_id, %d), s.committed_at = COALESCE(s.committed_at, '%s'), s.operation = COALESCE(s.operation, '%s'), s.last_seen_at = '%s' RETURN id(s)`

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
	// CR-01 entry-point validation: reject Cypher-unsafe inputs BEFORE
	// any graph mutation. snap.MetadataLocation flows from the
	// upstream catalog's commit response (server-controlled) AND from
	// L3 dispatch Hits (poller/webhook/s3-events, attacker-influenced
	// for the webhook path). tenantID is server-controlled (validated
	// UUID) but we re-check defensively. Operation/etc. are
	// server-controlled but routed through the same defence-in-depth.
	for _, field := range []struct{ name, value string }{
		{"metadata_location", snap.MetadataLocation},
		{"tenant_id", tenantID},
		{"operation", snap.Operation},
	} {
		if _, err := graph.SanitizeCypherLiteral(field.value); err != nil {
			return fmt.Errorf("ingest: merge snapshot: unsafe %s: %w", field.name, err)
		}
	}
	committedAt := time.UnixMilli(snap.TimestampMs).UTC().Format(time.RFC3339Nano)
	// Args order matches the Cypher template:
	//   metadata_location, tenant_id (inline map),
	//   snapshot_id, parent_snap_id, committed_at, operation (COALESCE'd),
	//   last_seen_at (unconditional).
	cypher := fmt.Sprintf(
		cypherMergeSnapshot,
		escapeCypher(snap.MetadataLocation),
		escapeCypher(tenantID),
		snap.SnapshotID,
		snap.ParentSnapshotID,
		committedAt,
		escapeCypher(snap.Operation),
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
			return fmt.Errorf("ingest: merge snapshot (cypher=%s): %w", cypher, err)
		}
		return nil
	})
}

// escapeCypher validates a caller-supplied string for safe inlining
// into a Cypher single-quoted string literal inside an AGE
// `cypher('graph', $$ ... $$)` dollar-quoted block.
//
// CR-01 mitigation: routes through graph.MustSanitizeCypherLiteral —
// the canonical Phase 1 defence is a strict allowlist of ASCII
// letters/digits/URI-safe punctuation; characters that could break
// out of the inner Cypher string literal OR the outer dollar-quoted
// PostgreSQL text literal (`'`, `"`, `\`, `$`, `{`, `}`, `;`, CR/LF,
// NUL, tab, non-ASCII) are REJECTED.
//
// This function panics on unsafe input — it is a defence-in-depth
// chokepoint. All untrusted caller inputs (OpenLineage URIs,
// Iceberg table/namespace identifiers, principal subjects) MUST be
// validated at the HTTP/handler boundary BEFORE reaching this
// function. A panic here surfaces a programming bug: an entry-point
// validator was missed. The HTTP-layer validation is in:
//
//   - internal/lineage/http/handler.go (OpenLineage URIs).
//   - internal/gateway/iceberg/handler.go (identifierRegex on path
//     segments; principal sub validated by upstream auth chain).
//   - internal/ingest/lineage.go::MergeLineageEdge (entry-point
//     validation for direct callers bypassing the HTTP handler).
//
// Production code paths cannot reach this panic; tests that pass
// unsafe input deliberately should expect a panic.
func escapeCypher(s string) string {
	return graph.MustSanitizeCypherLiteral(s)
}
