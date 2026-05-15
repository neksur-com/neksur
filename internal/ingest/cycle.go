// Lineage cycle pre-check — D-1.06 + D-001.08 + Pitfall 4.
//
// ValidateNoCycle is called before every LINEAGE_OF MERGE. It performs:
//
//  1. Postgres advisory lock keyed on `hashtext($srcURI)` (Pitfall 4):
//     serializes concurrent cycle-introducing writes on the same source
//     URI. Without this, two goroutines could simultaneously check
//     "would this introduce a cycle?" against the pre-write state, both
//     see clean ancestors, and both write — closing the cycle anyway.
//     The advisory lock is per-transaction (`pg_advisory_xact_lock`) so
//     it auto-releases at COMMIT/ROLLBACK; no leak risk.
//
//  2. Bounded `[:LINEAGE_OF*1..5]` traversal (D-001.08): walks up to 5
//     hops of ancestors from the target. If the source appears in the
//     ancestor set, the proposed edge would close a cycle of length ≤ 5.
//     5 is the maximum supported depth per ADR-001 D-001.08; cycle
//     chains longer than 5 are NOT detected here (and are caught by
//     the periodic RunCycleSweep — see cycle_sweep.go).
//
// The Cypher RETURNs the ancestor's iceberg_id plus `nodes(path)` (the
// cycle path as a list of iceberg_id strings) so LineageCycleError can
// surface the exact cycle for operator debugging (CONTEXT specifics
// line 171).

package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
)

// cypherCycleCheck walks ancestors of $target_uri via bounded
// `[:LINEAGE_OF*1..5]` (D-001.08 — the ONLY permitted bounded form for
// this query). If the source URI appears in the ancestor set, the path
// list is returned so we can build a precise LineageCycleError.
//
// The MATCH binds `path` so `nodes(path)` yields the ordered vertex
// list closing the cycle (source → ... → target → ... → source).
const cypherCycleCheck = `
MATCH path = (target { iceberg_id: '%s' })-[:LINEAGE_OF*1..5]->(ancestor { iceberg_id: '%s' })
RETURN [n IN nodes(path) | n.iceberg_id] AS cycle_path
LIMIT 1
`

// ValidateNoCycle returns a *LineageCycleError if MERGE'ing a
// LINEAGE_OF edge from srcURI → tgtURI would close a cycle. Must be
// called BEFORE the LINEAGE_OF MERGE, inside the SAME transaction (so
// the check sees the same snapshot of the graph the MERGE will write
// against; RESEARCH lines 730-733).
//
// The advisory lock keyed on srcURI's hashtext serializes concurrent
// cycle-introducing writes on the same source (Pitfall 4 mitigation).
// Both writers see the SAME pre-write ancestor set, so at most one
// succeeds.
//
// nil is returned when no cycle would be introduced (the common case).
// A non-LineageCycleError is returned for transport/auth failures.
func ValidateNoCycle(ctx context.Context, gc *graph.GraphClient, tenantID, srcURI, tgtURI string) error {
	return gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return validateNoCycleTx(ctx, tx, srcURI, tgtURI)
	})
}

// validateNoCycleTx is the in-transaction variant — called from
// MergeLineageEdge after that function has already established its own
// ExecuteInTenant transaction AND acquired the Pitfall 4 advisory lock
// keyed on srcURI's hashtext. Separating cycle check + MERGE allows
// them to share one tx (RESEARCH line 730). Standalone callers that
// bypass MergeLineageEdge MUST use ValidateNoCycle above — it acquires
// the lock for them.
func validateNoCycleTx(ctx context.Context, tx pgx.Tx, srcURI, tgtURI string) error {
	// Pitfall 4 — advisory lock keyed on srcURI so concurrent writes on
	// the same source serialize. `pg_advisory_xact_lock` auto-releases
	// at COMMIT/ROLLBACK; no manual unlock needed. (Acquired here AS
	// WELL AS in lineage.go::MergeLineageEdge — Postgres advisory locks
	// are idempotent for the same (txn, key) pair so a second acquisition
	// in the same transaction is a no-op.)
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))", srcURI,
	); err != nil {
		return fmt.Errorf("ingest: cycle pre-check advisory lock: %w", err)
	}

	// Cycle pre-check via bounded *1..5 VLP (D-001.08).
	// The bounded form `*1..5` is REQUIRED — unbounded `*` or `*1..`
	// would be rejected by graph.ValidateTraversalDepth at the gateway
	// layer, but we never even reach that gate because we use tx.Exec
	// directly. The hardcoded `*1..5` in the template is the
	// belt-and-suspenders enforcement.
	cypher := fmt.Sprintf(
		cypherCycleCheck,
		escapeCypher(tgtURI),
		escapeCypher(srcURI),
	)
	q := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
		cypher,
	)
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("ingest: cycle pre-check: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		// No ancestor row → no cycle → all good.
		return nil
	}

	// A row came back — we have a cycle. Parse the cycle_path so the
	// returned LineageCycleError surfaces the exact chain.
	var rawPath []byte
	if err := rows.Scan(&rawPath); err != nil {
		// We KNOW there's a cycle; scan failure means we cannot
		// extract the path. Surface a degraded LineageCycleError
		// rather than swallowing the cycle into a generic error.
		return &LineageCycleError{
			SourceID: srcURI,
			TargetID: tgtURI,
			Cycle:    []string{srcURI, tgtURI, srcURI},
		}
	}
	cycle := parseCyclePath(rawPath, srcURI, tgtURI)
	return &LineageCycleError{
		SourceID: srcURI,
		TargetID: tgtURI,
		Cycle:    cycle,
	}
}

// parseCyclePath unwraps AGE's agtype list-of-strings shape into a Go
// []string. The agtype on-wire form is typically a JSON string ending
// in `::list` — e.g., `["a","b","c","a"]::list`. We strip the type
// annotation suffix and json.Unmarshal the array.
//
// On any parse failure we fall back to a degraded 3-element path
// (src → tgt → src) so the LineageCycleError still gives the operator
// enough context to start debugging.
func parseCyclePath(raw []byte, srcURI, tgtURI string) []string {
	s := string(raw)
	// Strip ::list / ::path / ::array suffix.
	for _, suffix := range []string{"::list", "::path", "::array"} {
		if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
			s = s[:len(s)-len(suffix)]
			break
		}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{srcURI, tgtURI, srcURI}
	}
	// Append the closing src so the path reads as A → B → C → A
	// (the cycle path query returns A → B → C; we append A).
	if len(out) > 0 && out[len(out)-1] != srcURI {
		out = append(out, srcURI)
	}
	return out
}
