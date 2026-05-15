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

// cypherMergeColumnAndEdge is the canonical Column + HAS_COLUMN MERGE
// template (RESEARCH §Pattern 3 lines 699-717 verbatim). Two
// idempotent MERGEs:
//
//  1. MERGE (c:Column { snapshot_loc, name }) — per-snapshot Column key.
//  2. MERGE (s)-[r:HAS_COLUMN]->(c) — one edge per snapshot+column.
//
// The MATCH on Snapshot at the top is the guard: if no Snapshot row
// exists for $loc, the MATCH yields zero rows and neither MERGE fires.
// We detect this case by counting rows in the wrapper (returning
// ErrSnapshotNotFound).
const cypherMergeColumnAndEdge = `
MATCH (s:Snapshot { metadata_location: '%s' })
MERGE (c:Column { snapshot_loc: '%s', name: '%s' })
ON CREATE SET
  c.iceberg_id  = %d,
  c.data_type   = '%s',
  c.required    = %t,
  c.doc         = '%s',
  c.tenant_id   = '%s'
MERGE (s)-[r:HAS_COLUMN]->(c)
ON CREATE SET
  r.ordinal = %d,
  r.tenant_id = '%s'
RETURN id(c)
`

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
			cypher := fmt.Sprintf(
				cypherMergeColumnAndEdge,
				escapeCypher(snapMetaLoc),
				escapeCypher(snapMetaLoc),
				escapeCypher(col.Name),
				col.ID,
				escapeCypher(col.Type),
				col.Required,
				escapeCypher(col.Doc),
				escapeCypher(tenantID),
				idx,
				escapeCypher(tenantID),
			)
			q := fmt.Sprintf(
				"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
				cypher,
			)
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return fmt.Errorf("ingest: merge column %q (ordinal %d): %w", col.Name, idx, err)
			}
			matched := 0
			for rows.Next() {
				matched++
			}
			rerr := rows.Err()
			rows.Close()
			if rerr != nil {
				return fmt.Errorf("ingest: merge column %q rows: %w", col.Name, rerr)
			}
			if matched == 0 {
				return fmt.Errorf("ingest: merge column %q: %w (snapshot=%s)",
					col.Name, ErrSnapshotNotFound, snapMetaLoc)
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
