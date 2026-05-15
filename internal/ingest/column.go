// Column + HAS_COLUMN MERGE primitives — D-1.05 per-snapshot schema.
//
// Per RESEARCH §Pattern 3 lines 699-717: Column nodes are keyed by
// (snapshot_loc, name) so schema evolution is a 1-hop query — to find
// "the schema as of snapshot X" you walk one HAS_COLUMN edge from that
// Snapshot. This is the canonical encoding of D-1.05.
//
// The MERGE-on-MATCH-of-Snapshot template means: if the parent
// Snapshot doesn't exist in the graph, the entire MERGE is a no-op
// (returns zero rows). The wrapper turns that into ErrSnapshotNotFound
// so callers ingest the Snapshot first.

package ingest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// pitfall5SemanticTagColumn describes the ON CREATE SET / ON MATCH SET
// semantics this template emulates — same Plan 01-04 deviation #1 as
// snapshot.go (AGE 1.6 rejects the literal `ON CREATE SET` syntax;
// we COALESCE-emulate). Audit-anchor for the MERGE (s)-[r:HAS_COLUMN]->(c)
// edge shape and the ON CREATE SET ordinal preservation property.
const pitfall5SemanticTagColumn = "MERGE (s)-[r:HAS_COLUMN]->(c) — ON CREATE SET ordinal (COALESCE'd)"

var _ = pitfall5SemanticTagColumn

// cypherMergeColumnAndEdge is the Phase 1 Column + HAS_COLUMN MERGE
// template adapted for AGE 1.6 (Plan 01-04 deviation #1 — see
// snapshot.go header for the AGE 1.6 ON CREATE SET workaround). Two
// idempotent MERGEs:
//
//  1. MERGE (c:Column { snapshot_loc, name, tenant_id }) — natural
//     key per D-1.05 + tenant_id inline (CHECK-constraint-safe).
//     COALESCE'd SET on data_type / iceberg_id / required / doc means
//     the original ingest's values are preserved on retry.
//
//  2. MERGE (s)-[r:HAS_COLUMN { tenant_id }]->(c) — tenant_id inline
//     in the edge property map (CHECK-safe). The `ordinal` value
//     uses COALESCE so re-merge does not perturb the original column
//     position.
//
// The MATCH on Snapshot at the top is the guard: if no Snapshot row
// exists for $loc, the MATCH yields zero rows and neither MERGE fires.
// We detect this case by counting rows in the wrapper (returning
// ErrSnapshotNotFound).
//
// AGE 1.6 does NOT support multiple MERGE clauses inside a single
// cypher() call (parser regression). We split into two cypher() calls
// dispatched in sequence inside the same tx — see MergeColumns below.
const cypherMergeColumnNode = `MATCH (s:Snapshot {metadata_location: '%s'}) MERGE (c:Column {snapshot_loc: '%s', name: '%s', tenant_id: '%s'}) WITH c SET c.iceberg_id = COALESCE(c.iceberg_id, %d), c.data_type = COALESCE(c.data_type, '%s'), c.required = COALESCE(c.required, %t), c.doc = COALESCE(c.doc, '%s') RETURN id(c)`

// cypherMergeHasColumnEdge wires the HAS_COLUMN edge from the matched
// Snapshot to the matched Column. tenant_id is inline (CHECK-safe);
// ordinal is COALESCE'd so re-merge preserves the original column
// position.
const cypherMergeHasColumnEdge = `MATCH (s:Snapshot {metadata_location: '%s'}), (c:Column {snapshot_loc: '%s', name: '%s'}) MERGE (s)-[r:HAS_COLUMN {tenant_id: '%s'}]->(c) WITH r SET r.ordinal = COALESCE(r.ordinal, %d) RETURN id(r)`

// MergeColumns upserts Column vertices + HAS_COLUMN edges for the given
// snapshot. Each (snapshot_loc, name) pair is a distinct Column node
// per D-1.05; re-MERGE with the same key is idempotent. The HAS_COLUMN
// edge carries `ordinal` for stable column-order recovery.
//
// All columns are merged inside ONE transaction (RESEARCH line 730 —
// "MUST run inside SAME transaction" for consistency); a partial failure
// rolls back the entire batch so the graph never observes a half-merged
// schema.
//
// Errors:
//   - ErrSnapshotNotFound: the Snapshot row with metadata_location =
//     snapMetaLoc does not exist. Call MergeSnapshot first.
//   - Wrapped pgx errors propagate from the Postgres layer.
func (s *Service) MergeColumns(ctx context.Context, tenantID, snapMetaLoc string, columns []iceberg.SchemaField) error {
	if snapMetaLoc == "" {
		return fmt.Errorf("ingest: merge columns: empty snapshot metadata_location")
	}
	if len(columns) == 0 {
		// Empty batch is allowed (empty-schema Snapshot edge case);
		// we still verify the Snapshot exists so callers don't silently
		// no-op against a missing parent.
		return s.assertSnapshotExists(ctx, tenantID, snapMetaLoc)
	}
	return s.gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for idx, col := range columns {
			// Step 1 — MERGE the Column node. AGE 1.6 ON-CREATE-SET
			// emulation via COALESCE (snapshot.go header).
			cypherNode := fmt.Sprintf(
				cypherMergeColumnNode,
				escapeCypher(snapMetaLoc),
				escapeCypher(snapMetaLoc),
				escapeCypher(col.Name),
				escapeCypher(tenantID),
				col.ID,
				escapeCypher(col.Type),
				col.Required,
				escapeCypher(col.Doc),
			)
			qNode := fmt.Sprintf(
				"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
				cypherNode,
			)
			rows, err := tx.Query(ctx, qNode)
			if err != nil {
				return fmt.Errorf("ingest: merge column node %q (ordinal %d): %w", col.Name, idx, err)
			}
			matched := 0
			for rows.Next() {
				matched++
			}
			rerr := rows.Err()
			rows.Close()
			if rerr != nil {
				return fmt.Errorf("ingest: merge column node %q rows: %w", col.Name, rerr)
			}
			if matched == 0 {
				// MATCH on Snapshot returned nothing → the parent
				// Snapshot doesn't exist. Caller must MergeSnapshot
				// first (D-1.05 invariant).
				return fmt.Errorf("ingest: merge column %q: %w (snapshot=%s)",
					col.Name, ErrSnapshotNotFound, snapMetaLoc)
			}

			// Step 2 — MERGE the HAS_COLUMN edge. AGE 1.6 doesn't
			// allow multiple MERGE clauses in one cypher() call; we
			// dispatch the edge MERGE separately in the same tx so
			// the rollback semantics match a single transactional MERGE.
			cypherEdge := fmt.Sprintf(
				cypherMergeHasColumnEdge,
				escapeCypher(snapMetaLoc),
				escapeCypher(snapMetaLoc),
				escapeCypher(col.Name),
				escapeCypher(tenantID),
				idx,
			)
			qEdge := fmt.Sprintf(
				"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
				cypherEdge,
			)
			if _, err := tx.Exec(ctx, qEdge); err != nil {
				return fmt.Errorf("ingest: merge has_column edge %q (ordinal %d): %w", col.Name, idx, err)
			}
		}
		return nil
	})
}

// assertSnapshotExists is the empty-columns guard: returns
// ErrSnapshotNotFound if no Snapshot row with the given
// metadata_location exists in the current tenant scope.
func (s *Service) assertSnapshotExists(ctx context.Context, tenantID, snapMetaLoc string) error {
	return s.gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cypher := fmt.Sprintf(
			"MATCH (s:Snapshot { metadata_location: '%s' }) RETURN id(s) LIMIT 1",
			escapeCypher(snapMetaLoc),
		)
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
			cypher,
		)
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("ingest: assert snapshot exists: %w", err)
		}
		defer rows.Close()
		if !rows.Next() {
			return fmt.Errorf("ingest: assert snapshot exists: %w (snapshot=%s)",
				ErrSnapshotNotFound, snapMetaLoc)
		}
		return nil
	})
}
