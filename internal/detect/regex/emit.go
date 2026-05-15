// ADR-007 graph emission for L3 detection findings (Plan 01-07).
//
// Per CONTEXT line 85 + ADR-007 §6.5, an L3 detection scan emits:
//
//   1. DetectionRun vlabel  — the per-scan audit node, keyed on a
//      synthetic run_id (UUIDv4). Holds scan_strategy, sample_size,
//      timestamps.
//   2. (Snapshot)-[:VIOLATION_DETECTED_BY]->(DetectionRun) — links
//      the offending Snapshot back to the DetectionRun (D-003.06
//      preexisting edge, V0010).
//   3. For each finding:
//      a. Tag vlabel (Phase 1 ones from Phase1Bank: pii-ssn-us,
//         pii-email, pii-credit-card, pii-phone, pii-iban). Tag is
//         a long-lived dictionary node — we MERGE not CREATE.
//      b. Column vlabel (per-snapshot key: snapshot_loc + name per
//         D-1.05). Created by ingest's MergeColumns; we MERGE here as
//         defence-in-depth so the edge MATCH doesn't fail when
//         detection runs before ingest's column emission.
//      c. (Column)-[:CLASSIFIED_AS]->(Tag) — the canonical Phase 1
//         tag-application edge (V0010 preexisting).
//      d. Classification vlabel — the per-finding instance (NEW V0030
//         vlabel per ADR-007 §6.5). Records confidence + source +
//         detection_run_id so historical classifications (across
//         multiple scans of the same column) are independent nodes.
//      e. (Classification)-[:DETECTED_BY]->(DetectionRun) — links the
//         per-finding instance back to its scan (NEW V0030 edge per
//         ADR-007).
//
// Cross-replica dedup (Pitfall 10): BEFORE any graph emission we
// INSERT into the relational `detection_runs` table with `ON CONFLICT
// (snapshot_metadata_location) DO NOTHING RETURNING id`. The V0062
// UNIQUE constraint catches the race; if RETURNING yields no rows,
// another replica already won the scan and we return early without
// emitting anything (the winning replica's emission is the
// authoritative one).
//
// AGE 1.6 quirks applied (Plan 01-04 + 01-05 + 01-06 SUMMARY lessons):
//
//   - No `ON CREATE SET / ON MATCH SET` — emulate via COALESCE-on-WITH-SET.
//   - tenant_id MUST be in the inline MERGE property map (V0030 CHECK).
//   - One MERGE per cypher() call (multi-MERGE-per-call rejected).
//   - Single-line Cypher per call.
//   - Schema-qualify the relational `detection_runs` to
//     `tenant_<uuid>.detection_runs` because the GraphClient pool's
//     BeforeAcquire sets `search_path = ag_catalog, "$user", public`
//     (Plan 01-06 audit-schema-qualify lesson).

package regex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
)

// adr007EmissionAuditAnchor preserves the canonical ADR-007 emission
// shape this package implements. AGE 1.6's quirks force COALESCE-on-WITH-SET
// and per-cypher-call MERGE dispatch in the live Cypher; this constant
// captures the canonical openCypher shape for grep + code-review
// visibility (mirrors the d003_06_audit_anchor pattern in
// internal/gateway/iceberg/audit.go).
const adr007EmissionAuditAnchor = "MERGE (dr:DetectionRun) + (cl:Classification)-[:DETECTED_BY]->(dr) + (s:Snapshot)-[:VIOLATION_DETECTED_BY]->(dr) per ADR-007 §6.5 + CONTEXT line 85"

var _ = adr007EmissionAuditAnchor // referenced by audit tooling.

// Cypher templates per the audit anchors above. AGE 1.6 emulation
// patterns: COALESCE-on-WITH-SET for ON CREATE SET; single-line shape;
// inline tenant_id in the MERGE property map.

// cypherMergeDetectionRun creates the per-scan DetectionRun audit node.
//
// Audit-anchor (canonical openCypher):
//
//	MERGE (dr:DetectionRun { run_id: $run_id })
//	ON CREATE SET dr.started_at = $started_at,
//	              dr.scan_strategy = $strategy,
//	              dr.sample_size = $sample,
//	              dr.tenant_id = $tenant
const cypherMergeDetectionRun = `MERGE (dr:DetectionRun {run_id: '%s', tenant_id: '%s'}) WITH dr SET dr.started_at = COALESCE(dr.started_at, '%s'), dr.scan_strategy = COALESCE(dr.scan_strategy, '%s'), dr.sample_size = COALESCE(dr.sample_size, %d) RETURN id(dr)`

// cypherMergeSnapshotForDetect ensures the Snapshot vlabel exists so
// the VIOLATION_DETECTED_BY MATCH below resolves. The actual snapshot
// content (committed_at etc.) is owned by internal/ingest's
// MergeSnapshot; we MERGE the bare key for defence-in-depth.
const cypherMergeSnapshotForDetect = `MERGE (s:Snapshot {metadata_location: '%s', tenant_id: '%s'}) RETURN id(s)`

// cypherMergeViolationEdge — Snapshot → DetectionRun audit edge per
// V0010 preexisting elabel (ACTUAL_WRITE / VIOLATION_DETECTED_BY).
//
// Audit-anchor (canonical openCypher):
//
//	MATCH (s:Snapshot {metadata_location: $snap_loc}), (dr:DetectionRun {run_id: $run_id})
//	MERGE (s)-[r:VIOLATION_DETECTED_BY {tenant_id: $tenant}]->(dr)
//	ON CREATE SET r.detected_at = $ts
const cypherMergeViolationEdge = `MATCH (s:Snapshot {metadata_location: '%s'}), (dr:DetectionRun {run_id: '%s'}) MERGE (s)-[r:VIOLATION_DETECTED_BY {tenant_id: '%s'}]->(dr) WITH r SET r.detected_at = COALESCE(r.detected_at, '%s') RETURN id(r)`

// cypherMergeColumn ensures the per-snapshot Column vlabel exists. Same
// natural key as ingest's MergeColumns (snapshot_loc + name per D-1.05).
const cypherMergeColumn = `MERGE (c:Column {snapshot_loc: '%s', name: '%s', tenant_id: '%s'}) RETURN id(c)`

// cypherMergeTag is the Tag dictionary node MERGE. Tags are long-lived
// (one per category like pii-email); MERGE is idempotent.
const cypherMergeTag = `MERGE (tag:Tag {id: '%s', tenant_id: '%s'}) WITH tag SET tag.name = COALESCE(tag.name, '%s'), tag.category = COALESCE(tag.category, '%s') RETURN id(tag)`

// cypherMergeClassifiedAsEdge — Column → Tag edge per V0010 preexisting
// CLASSIFIED_AS elabel.
//
// Audit-anchor (canonical openCypher):
//
//	MATCH (c:Column {snapshot_loc: $snap_loc, name: $col_name}), (tag:Tag {id: $tag_id})
//	MERGE (c)-[r:CLASSIFIED_AS {tenant_id: $tenant}]->(tag)
//	ON CREATE SET r.confidence = $confidence, r.source = 'regex'
//	ON MATCH SET  r.last_seen_at = $ts, r.confidence = $confidence
const cypherMergeClassifiedAsEdge = `MATCH (c:Column {snapshot_loc: '%s', name: '%s'}), (tag:Tag {id: '%s'}) MERGE (c)-[r:CLASSIFIED_AS {tenant_id: '%s'}]->(tag) WITH r SET r.confidence = %f, r.source = 'regex', r.last_seen_at = '%s' RETURN id(r)`

// cypherMergeClassification — the NEW Classification vlabel per ADR-007 §6.5.
// One Classification per (column, tag, detection_run) tuple.
const cypherMergeClassification = `MERGE (cl:Classification {column_snap_loc: '%s', column_name: '%s', tag_id: '%s', detection_run: '%s', tenant_id: '%s'}) WITH cl SET cl.confidence = COALESCE(cl.confidence, %f), cl.source = COALESCE(cl.source, 'regex'), cl.match_type = COALESCE(cl.match_type, '%s') RETURN id(cl)`

// cypherMergeDetectedByEdge — Classification → DetectionRun edge per
// V0030 NEW elabel.
//
// Audit-anchor (canonical openCypher):
//
//	MATCH (cl:Classification { column_snap_loc, column_name, tag_id, detection_run }), (dr:DetectionRun { run_id })
//	MERGE (cl)-[r:DETECTED_BY {tenant_id: $tenant}]->(dr)
const cypherMergeDetectedByEdge = `MATCH (cl:Classification {column_snap_loc: '%s', column_name: '%s', tag_id: '%s', detection_run: '%s'}), (dr:DetectionRun {run_id: '%s'}) MERGE (cl)-[r:DETECTED_BY {tenant_id: '%s'}]->(dr) RETURN id(r)`

// EmitDetectionResults is the canonical Phase 1 L3 detection emission
// per ADR-007 + CONTEXT line 85.
//
// Returns:
//   - (runID, nil) on successful emission.
//   - ("", nil)    when another replica already started a scan for the
//                  same snap_loc (Pitfall 10 cross-replica dedup);
//                  caller logs + continues.
//   - (runID, wrapped pgx error) on graph or relational failure.
//
// Lifecycle:
//   1. Generate runID (UUIDv4).
//   2. Open ExecuteInTenant tx.
//   3. INSERT into `tenant_<uuid>.detection_runs` (snapshot_metadata_location UNIQUE)
//      with ON CONFLICT DO NOTHING RETURNING id. If RETURNING yields
//      zero rows → cross-replica dup → COMMIT empty tx + return ("", nil).
//   4. MERGE DetectionRun + Snapshot + VIOLATION_DETECTED_BY edge.
//   5. For each finding: MERGE Column + Tag + CLASSIFIED_AS + Classification +
//      DETECTED_BY.
//   6. UPDATE detection_runs SET finished_at = now(), findings_count.
//   7. COMMIT tx.
func EmitDetectionResults(
	ctx context.Context,
	gc *graph.GraphClient,
	tenantID string,
	snapMetaLoc string,
	strategy string,
	sampleSize int,
	findings []ColumnFinding,
) (runID string, retErr error) {
	if snapMetaLoc == "" {
		return "", fmt.Errorf("detect/regex: emit results: empty snap_loc")
	}
	if strategy == "" {
		strategy = "regex"
	}

	runID = uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Resolve schema name once — the relational detection_runs table
	// lives in tenant_<uuid>.detection_runs (V0062). The GraphClient
	// pool's search_path doesn't include the tenant schema; we
	// schema-qualify via pgx.Identifier.Sanitize() (Plan 01-06 audit
	// schema-qualify lesson).
	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		return "", fmt.Errorf("detect/regex: emit results: parse tenant id: %w", err)
	}
	schema := tenant.SchemaName(tenantUUID)
	detectionRunsTable := pgx.Identifier{schema, "detection_runs"}.Sanitize()

	skipped := false
	emitErr := gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Step 1 — Pitfall 10 cross-replica dedup INSERT.
		// The V0062 UNIQUE constraint on snapshot_metadata_location
		// ensures one replica wins; the loser's RETURNING yields zero
		// rows.
		insertSQL := fmt.Sprintf(`
			INSERT INTO %s (run_id, snapshot_metadata_location, started_at, scan_strategy, sample_size)
			VALUES ($1, $2, now(), $3, $4)
			ON CONFLICT (snapshot_metadata_location) DO NOTHING
			RETURNING id
		`, detectionRunsTable)
		var id int64
		row := tx.QueryRow(ctx, insertSQL, runID, snapMetaLoc, strategy, sampleSize)
		if scanErr := row.Scan(&id); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// Pitfall 10 — another replica already inserted; skip
				// emission entirely (winning replica owns the audit
				// trail).
				skipped = true
				return nil
			}
			return fmt.Errorf("detect/regex: insert detection_run: %w", scanErr)
		}

		// Step 2 — DetectionRun MERGE + Snapshot MERGE + edge.
		drCypher := fmt.Sprintf(
			cypherMergeDetectionRun,
			escapeCypher(runID), escapeCypher(tenantID),
			now, escapeCypher(strategy), sampleSize,
		)
		if err := execAGE(ctx, tx, drCypher, "merge detection run"); err != nil {
			return err
		}

		snapCypher := fmt.Sprintf(
			cypherMergeSnapshotForDetect,
			escapeCypher(snapMetaLoc), escapeCypher(tenantID),
		)
		if err := execAGE(ctx, tx, snapCypher, "merge snapshot for detect"); err != nil {
			return err
		}

		violationCypher := fmt.Sprintf(
			cypherMergeViolationEdge,
			escapeCypher(snapMetaLoc), escapeCypher(runID),
			escapeCypher(tenantID), now,
		)
		if err := execAGE(ctx, tx, violationCypher, "merge violation edge"); err != nil {
			return err
		}

		// Step 3 — per-finding emission.
		for _, f := range findings {
			colCypher := fmt.Sprintf(
				cypherMergeColumn,
				escapeCypher(snapMetaLoc), escapeCypher(f.ColumnName),
				escapeCypher(tenantID),
			)
			if err := execAGE(ctx, tx, colCypher, "merge column"); err != nil {
				return err
			}

			tagCypher := fmt.Sprintf(
				cypherMergeTag,
				escapeCypher(f.TagID), escapeCypher(tenantID),
				escapeCypher(f.TagName), escapeCypher(f.TagCategory),
			)
			if err := execAGE(ctx, tx, tagCypher, "merge tag"); err != nil {
				return err
			}

			classifiedAsCypher := fmt.Sprintf(
				cypherMergeClassifiedAsEdge,
				escapeCypher(snapMetaLoc), escapeCypher(f.ColumnName),
				escapeCypher(f.TagID), escapeCypher(tenantID),
				f.Confidence, now,
			)
			if err := execAGE(ctx, tx, classifiedAsCypher, "merge classified-as edge"); err != nil {
				return err
			}

			classificationCypher := fmt.Sprintf(
				cypherMergeClassification,
				escapeCypher(snapMetaLoc), escapeCypher(f.ColumnName),
				escapeCypher(f.TagID), escapeCypher(runID),
				escapeCypher(tenantID),
				f.Confidence, escapeCypher(f.MatchType),
			)
			if err := execAGE(ctx, tx, classificationCypher, "merge classification"); err != nil {
				return err
			}

			detectedByCypher := fmt.Sprintf(
				cypherMergeDetectedByEdge,
				escapeCypher(snapMetaLoc), escapeCypher(f.ColumnName),
				escapeCypher(f.TagID), escapeCypher(runID),
				escapeCypher(runID), escapeCypher(tenantID),
			)
			if err := execAGE(ctx, tx, detectedByCypher, "merge detected-by edge"); err != nil {
				return err
			}
		}

		// Step 4 — UPDATE detection_runs.finished_at + findings_count.
		updateSQL := fmt.Sprintf(`
			UPDATE %s
			SET finished_at = now(), findings_count = $1
			WHERE id = $2
		`, detectionRunsTable)
		if _, err := tx.Exec(ctx, updateSQL, len(findings), id); err != nil {
			return fmt.Errorf("detect/regex: update detection_run: %w", err)
		}
		return nil
	})
	if emitErr != nil {
		return runID, fmt.Errorf("detect/regex: emit results: %w", emitErr)
	}
	if skipped {
		return "", nil
	}
	return runID, nil
}

// execAGE runs one Cypher statement inside the given tx, wrapping
// errors with the operation name. Mirrors internal/gateway/iceberg/audit.go's
// execAGE — duplicated here to avoid a cross-package dependency.
func execAGE(ctx context.Context, tx pgx.Tx, cypher, op string) error {
	q := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
		cypher,
	)
	if _, err := tx.Exec(ctx, q); err != nil {
		return fmt.Errorf("detect/regex: %s: %w", op, err)
	}
	return nil
}

// escapeCypher single-quote-escapes a string literal for safe inlining
// into a Cypher MERGE/MATCH body. Mirrors internal/ingest's escapeCypher
// + internal/gateway/iceberg's escapeCypher exactly — duplicated here
// to avoid a cross-package dependency.
func escapeCypher(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.ReplaceAll(s, "'", "\\'")
}
