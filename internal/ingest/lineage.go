// LINEAGE_OF MERGE primitive — D-1.06.
//
// Per RESEARCH §Pattern 3 lines 741-752 the LINEAGE_OF MERGE template
// keys edges on (src, tgt) via the MATCH-by-iceberg_id shape, MERGEs
// the edge, and splits ON CREATE / ON MATCH so re-application of the
// same edge preserves the original `created_at` timestamp.
//
// The cycle pre-check (cycle.go::validateNoCycleTx) runs FIRST inside
// the same transaction as the MERGE — RESEARCH line 730 — so the check
// sees the same pre-write graph state the MERGE will modify, and a
// concurrent writer cannot sneak a cycle past us (Pitfall 4
// serialization via advisory lock).

package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// cypherMergeLineageEdge is the canonical LINEAGE_OF MERGE template
// (RESEARCH §Pattern 3 lines 741-752 verbatim). MATCH-by-iceberg_id
// pairs the source and target nodes (which must already exist in the
// graph — typically as Table or Snapshot vertices); MERGE creates the
// edge if missing, updates `last_seen_at` on the matched-existing path.
const cypherMergeLineageEdge = `
MATCH (src),(tgt)
WHERE src.iceberg_id = '%s' AND tgt.iceberg_id = '%s'
MERGE (src)-[r:LINEAGE_OF]->(tgt)
ON CREATE SET
  r.created_at = '%s',
  r.tenant_id  = '%s',
  r.run_id     = '%s'
ON MATCH SET
  r.last_seen_at = '%s'
RETURN id(r)
`

// MergeLineageEdge upserts a LINEAGE_OF edge from src → tgt. Performs
// the bounded `*1..5` cycle pre-check + the advisory lock first, then
// the MERGE — all inside ONE transaction (D-1.06 + Pitfall 4).
//
// Returns:
//   - nil on success (whether the edge was CREATED or MATCHED).
//   - *LineageCycleError when the MERGE would close a cycle ≤ 5 hops.
//     The error carries the offending cycle path for operator debugging.
//   - Wrapped pgx errors for transport/auth failures.
//
// runID is the OpenLineage RunID that produced the edge (Phase 1
// stores it on the edge so operators can trace "which Spark run
// established this lineage" later). Empty string is allowed (the
// gateway / scheduler may MERGE edges without an OpenLineage source).
func (s *Service) MergeLineageEdge(ctx context.Context, tenantID, srcURI, tgtURI, runID string, ts time.Time) error {
	if srcURI == "" || tgtURI == "" {
		return fmt.Errorf("ingest: merge lineage edge: %w", ErrLineageBatchEmpty)
	}
	if srcURI == tgtURI {
		// Self-edges are trivially cycles (length 1). Reject before
		// the cycle pre-check so we don't perturb the graph state.
		return &LineageCycleError{
			SourceID: srcURI,
			TargetID: tgtURI,
			Cycle:    []string{srcURI, tgtURI},
		}
	}
	tsStr := ts.UTC().Format(time.RFC3339Nano)
	return s.gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Step 0 (Pitfall 4): advisory lock keyed on srcURI's hashtext.
		// `pg_advisory_xact_lock` serializes concurrent cycle-introducing
		// writes on the same source — both writers see the same pre-write
		// ancestor set during the next step, and at most one succeeds in
		// closing a cycle. Auto-releases at COMMIT/ROLLBACK. (Re-acquired
		// inside validateNoCycleTx for the standalone ValidateNoCycle path;
		// idempotent for the same (txn, key) pair.)
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext($1))", srcURI,
		); err != nil {
			return fmt.Errorf("ingest: merge lineage edge advisory lock: %w", err)
		}

		// Step 1 (D-1.06): bounded `*1..5` cycle pre-check inside this same
		// tx so the check sees the same pre-write graph state the MERGE
		// will modify. validateNoCycleTx returns a *LineageCycleError on
		// detection; we propagate it untouched so callers can errors.As /
		// errors.Is it.
		if err := validateNoCycleTx(ctx, tx, srcURI, tgtURI); err != nil {
			return err
		}

		// Step 2: the MERGE itself. Wrapped errors carry the operation
		// name for triage; the typed LineageCycleError from step 1
		// would already have early-returned above.
		cypher := fmt.Sprintf(
			cypherMergeLineageEdge,
			escapeCypher(srcURI),
			escapeCypher(tgtURI),
			tsStr,
			escapeCypher(tenantID),
			escapeCypher(runID),
			tsStr,
		)
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
			cypher,
		)
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("ingest: merge lineage edge (%s -> %s): %w", srcURI, tgtURI, err)
		}
		return nil
	})
}
