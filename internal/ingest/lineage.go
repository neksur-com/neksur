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

	"github.com/neksur-com/neksur/internal/graph"
)

// cypherMergeLineageEdge is the Phase 1 LINEAGE_OF MERGE template
// adapted for AGE 1.6 (Plan 01-04 deviation #1 — see snapshot.go
// header for the AGE 1.6 ON CREATE SET workaround).
//
// Pattern: tenant_id MUST be in the inline edge property map (V0030
// CHECK constraint LINEAGE_OF_tenant_id_required). created_at +
// run_id are COALESCE'd via WITH+SET so the original-create values
// are preserved on retry (Pitfall 5). last_seen_at is unconditionally
// set to the current ts (heartbeat semantics — same shape as
// committed_at vs last_seen_at on Snapshot).
const cypherMergeLineageEdge = `MATCH (src),(tgt) WHERE src.iceberg_id = '%s' AND tgt.iceberg_id = '%s' MERGE (src)-[r:LINEAGE_OF {tenant_id: '%s'}]->(tgt) WITH r SET r.created_at = COALESCE(r.created_at, '%s'), r.run_id = COALESCE(r.run_id, '%s'), r.last_seen_at = '%s' RETURN id(r)`

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
	// CR-01 entry-point validation: srcURI and tgtURI flow from
	// OpenLineage Dataset.URI() (attacker-controlled per RESEARCH +
	// REVIEW.md CR-01). Reject any Cypher-unsafe character BEFORE
	// the value reaches escapeCypher (which is a defence-in-depth
	// panic guard). The HTTP receiver in internal/lineage/http/handler.go
	// also validates upfront so callers see a clean 400 — this
	// secondary check covers direct callers (tests, future internal
	// drivers).
	for _, field := range []struct{ name, value string }{
		{"src_uri", srcURI},
		{"tgt_uri", tgtURI},
		{"tenant_id", tenantID},
		{"run_id", runID},
	} {
		if _, err := graph.SanitizeCypherLiteral(field.value); err != nil {
			return fmt.Errorf("ingest: merge lineage edge: unsafe %s: %w", field.name, err)
		}
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
		// Args order: src.iceberg_id, tgt.iceberg_id, tenant_id (inline),
		// created_at (COALESCE'd), run_id (COALESCE'd), last_seen_at.
		cypher := fmt.Sprintf(
			cypherMergeLineageEdge,
			escapeCypher(srcURI),
			escapeCypher(tgtURI),
			escapeCypher(tenantID),
			tsStr,
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
