//go:build integration

// Plan 01-04 Task 3 [BLOCKING] — MERGE idempotency end-to-end.
//
// Two tests prove the ON CREATE / ON MATCH split (Pitfall 5):
//
//   TestIngestMERGEIdempotent       — Snapshot.committed_at preserved on retry.
//   TestIngestHasColumnPerSnapshot  — D-1.05 per-snapshot Column + HAS_COLUMN.
//
// Both run against StartPhase1Fixture (Postgres+AGE+Polaris+Nessie+LocalStack)
// with a fresh tenant per test. Tests acquire a pgxpool wired with
// graph.WithBeforeAcquireDiscardAll + the AGE prelude AfterConnect so
// ingest.NewService(graph.NewGraphClient(...)) operates on the real
// AGE graph and the V0030/V0032 RLS policies fire.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/ingest"
)

const ingestMergeTenant = "44444444-4444-4444-4444-444444444444"

// TestIngestMERGEIdempotent — call MergeSnapshot twice with the same
// metadata_location; the graph holds exactly one Snapshot row and the
// original `committed_at` is NOT clobbered by the second MERGE (the
// ON CREATE / ON MATCH split mitigation — Pitfall 5).
func TestIngestMERGEIdempotent(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, ingestMergeTenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()
	svc := ingest.NewService(gc)

	const metaLoc = "s3://test-bucket/orders/metadata/00001-uuid.metadata.json"
	snap := iceberg.Snapshot{
		SnapshotID:       1001,
		ParentSnapshotID: 0,
		TimestampMs:      time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC).UnixMilli(),
		Operation:        "append",
		MetadataLocation: metaLoc,
	}

	// First MERGE — CREATE branch.
	if err := svc.MergeSnapshot(fx.ctx, ingestMergeTenant, snap); err != nil {
		t.Fatalf("first MergeSnapshot: %v", err)
	}

	// Capture the committed_at after the first MERGE so we can assert
	// it survives the second MERGE.
	committedAt1 := readSnapshotProperty(t, fx.ctx, gc, ingestMergeTenant, metaLoc, "committed_at")

	// Second MERGE with a DIFFERENT TimestampMs — if ON CREATE / ON
	// MATCH split is wrong, committed_at would be overwritten. The
	// snap value carries a new TimestampMs so the assertion is
	// load-bearing.
	snap2 := snap
	snap2.TimestampMs = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	if err := svc.MergeSnapshot(fx.ctx, ingestMergeTenant, snap2); err != nil {
		t.Fatalf("second MergeSnapshot: %v", err)
	}

	committedAt2 := readSnapshotProperty(t, fx.ctx, gc, ingestMergeTenant, metaLoc, "committed_at")
	if committedAt1 != committedAt2 {
		t.Errorf("committed_at clobbered on second MERGE: was %q now %q (Pitfall 5 ON CREATE/ON MATCH split broken)",
			committedAt1, committedAt2)
	}

	// Snapshot count = 1 (idempotency).
	count := countMatching(t, fx.ctx, gc, ingestMergeTenant,
		"MATCH (s:Snapshot) WHERE s.metadata_location = '"+metaLoc+"' RETURN count(s)")
	if count != 1 {
		t.Errorf("Snapshot count after 2 MERGEs = %d; expected 1", count)
	}

	// last_seen_at SHOULD be updated by the ON MATCH branch — sanity
	// check that the second MERGE landed in the MATCH path.
	lastSeen := readSnapshotProperty(t, fx.ctx, gc, ingestMergeTenant, metaLoc, "last_seen_at")
	if lastSeen == "" {
		t.Errorf("last_seen_at not set after second MERGE — ON MATCH branch did not fire")
	}
}

// TestIngestHasColumnPerSnapshot — D-1.05: HAS_COLUMN edge is
// per-snapshot, keyed by (snapshot_loc, name). Merging the same
// (snapshot, columns) twice yields exactly N columns + N edges.
func TestIngestHasColumnPerSnapshot(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenant = "55555555-5555-4555-5555-555555555555"
	_ = fx.ProvisionTenant(t, tenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()
	svc := ingest.NewService(gc)

	const metaLoc = "s3://test-bucket/orders/metadata/00002-uuid.metadata.json"
	snap := iceberg.Snapshot{
		SnapshotID:       2001,
		TimestampMs:      time.Now().UnixMilli(),
		Operation:        "append",
		MetadataLocation: metaLoc,
	}
	if err := svc.MergeSnapshot(fx.ctx, tenant, snap); err != nil {
		t.Fatalf("MergeSnapshot: %v", err)
	}

	cols := []iceberg.SchemaField{
		{ID: 1, Name: "order_id", Type: "long", Required: true},
		{ID: 2, Name: "customer_email", Type: "string", Required: false, Doc: "PII"},
		{ID: 3, Name: "order_total", Type: "decimal(10,2)", Required: true},
	}
	if err := svc.MergeColumns(fx.ctx, tenant, metaLoc, cols); err != nil {
		t.Fatalf("first MergeColumns: %v", err)
	}

	// First-call counts.
	colCount := countMatching(t, fx.ctx, gc, tenant,
		"MATCH (s:Snapshot {metadata_location: '"+metaLoc+"'})-[:HAS_COLUMN]->(c:Column) RETURN count(c)")
	if colCount != 3 {
		t.Errorf("HAS_COLUMN count after first MergeColumns = %d; expected 3", colCount)
	}

	// Second MergeColumns with the SAME columns — both sides of the MERGE
	// land on MATCH; counts must not double.
	if err := svc.MergeColumns(fx.ctx, tenant, metaLoc, cols); err != nil {
		t.Fatalf("second MergeColumns: %v", err)
	}
	colCount2 := countMatching(t, fx.ctx, gc, tenant,
		"MATCH (s:Snapshot {metadata_location: '"+metaLoc+"'})-[:HAS_COLUMN]->(c:Column) RETURN count(c)")
	if colCount2 != 3 {
		t.Errorf("HAS_COLUMN count after second (idempotent) MergeColumns = %d; expected 3", colCount2)
	}

	// ErrSnapshotNotFound on unknown snapshot location.
	err = svc.MergeColumns(fx.ctx, tenant, "s3://test-bucket/no-such-snap.metadata.json", cols)
	if err == nil {
		t.Errorf("MergeColumns against missing Snapshot should error; got nil")
	}
}

// --- helpers ----------------------------------------------------------

// readSnapshotProperty fetches a single property value from the
// Snapshot vertex identified by metaLoc. Returns the agtype text
// (string-quoted on the wire) with the surrounding quotes stripped.
func readSnapshotProperty(t *testing.T, ctx context.Context, gc *graph.GraphClient, tenantID, metaLoc, prop string) string {
	t.Helper()
	cy := "MATCH (s:Snapshot {metadata_location: '" + metaLoc + "'}) RETURN s." + prop
	q := "SELECT * FROM ag_catalog.cypher('neksur', $$ " + cy + " $$) AS (result ag_catalog.agtype)"
	var got string
	err := gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, q)
		return row.Scan(&got)
	})
	if err != nil {
		// pgx.ErrNoRows is acceptable — caller decides whether empty is OK.
		return ""
	}
	// agtype string form: `"value"::text` or `"value"`. Strip ::* suffix
	// then strip the surrounding quotes.
	got = stripAgtypeStringSuffix(got)
	return strings.Trim(got, `"`)
}

// stripAgtypeStringSuffix removes the `::text` / `::numeric` etc. type
// annotation that AGE appends to scalar results.
func stripAgtypeStringSuffix(s string) string {
	for _, suffix := range []string{"::text", "::numeric", "::int", "::list", "::path"} {
		if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
			return s[:len(s)-len(suffix)]
		}
	}
	return s
}

// countMatching runs a count Cypher and returns the numeric result.
// On parse failure returns -1 so a failed test surfaces the issue
// instead of silently asserting 0.
func countMatching(t *testing.T, ctx context.Context, gc *graph.GraphClient, tenantID, cypher string) int64 {
	t.Helper()
	q := "SELECT * FROM ag_catalog.cypher('neksur', $$ " + cypher + " $$) AS (result ag_catalog.agtype)"
	var got string
	err := gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, q)
		return row.Scan(&got)
	})
	if err != nil {
		t.Logf("countMatching scan error (cypher=%s): %v", cypher, err)
		return -1
	}
	// agtype integer result is the raw decimal string (no suffix).
	// Parse to int64; on parse failure return -1.
	n, parseErr := parseAgtypeInt(got)
	if parseErr != nil {
		t.Logf("countMatching parse error (got=%q): %v", got, parseErr)
		return -1
	}
	return n
}

// parseAgtypeInt converts an AGE-returned scalar agtype to int64.
// AGE returns counts as bare decimals (e.g., "3" or "0"); strip any
// trailing ::* type annotation.
func parseAgtypeInt(s string) (int64, error) {
	s = stripAgtypeStringSuffix(s)
	var out int64
	_, err := scanInt64(s, &out)
	return out, err
}

// scanInt64 is a tiny strconv wrapper so the helper file stays
// dependency-light at the top of the cluster of integration tests.
func scanInt64(s string, out *int64) (int, error) {
	// Use strconv.ParseInt for the simple case; the integration test
	// only ever sees count() results which are bare decimals.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, &numericParseError{s: s}
		}
		*out = (*out)*10 + int64(c-'0')
	}
	if len(s) == 0 {
		return 0, &numericParseError{s: s}
	}
	return len(s), nil
}

type numericParseError struct{ s string }

func (e *numericParseError) Error() string { return "parse int: " + e.s }
