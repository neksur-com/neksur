//go:build integration

// Plan 01-07 Task 3 [BLOCKING] — cross-replica dedup via V0062
// detection_runs.snapshot_metadata_location UNIQUE constraint
// (Pitfall 10).
//
// Spawns TWO concurrent regex.EmitDetectionResults goroutines for the
// SAME snap_loc (simulating two replicas behind the same ALB receiving
// the same SNS event). Asserts:
//
//   - BOTH calls return without panic.
//   - The detection_runs table holds EXACTLY 1 row (UNIQUE constraint
//     enforces the dedup).
//   - At most ONE of the two calls returns a non-empty runID (the
//     winner); the other returns "" (cross-replica skip).

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/detect/regex"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/tenant"
)

const detectCrossReplicaTenant = "55555555-5555-4555-8555-555555555555"

// TestDetectCrossReplicaDedup — Pitfall 10: two replicas race for the
// same snapshot; UNIQUE constraint must enforce single-row.
func TestDetectCrossReplicaDedup(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()
	_ = fx.ProvisionTenant(t, detectCrossReplicaTenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const snapLoc = "s3://cross-replica-test/snap-uuid/metadata.json"

	// Spawn 2 concurrent EmitDetectionResults calls.
	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]string, 2)
	errors := make([]error, 2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			runID, err := regex.EmitDetectionResults(
				context.Background(), gc, detectCrossReplicaTenant,
				snapLoc, "regex", 0, nil)
			results[idx] = runID
			errors[idx] = err
		}(i)
	}
	wg.Wait()

	// Both must succeed (no panic, no error).
	for i, err := range errors {
		if err != nil {
			t.Errorf("EmitDetectionResults[%d] error: %v", i, err)
		}
	}

	// At most one returns a non-empty runID (the winner). The other
	// returns "" indicating Pitfall 10 cross-replica skip.
	winners := 0
	for _, r := range results {
		if r != "" {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners=%d; want exactly 1 (Pitfall 10 dedup)", winners)
	}

	// Now query detection_runs — MUST have exactly 1 row.
	count := countDetectionRunsForLoc(t, fx.ctx, gc, detectCrossReplicaTenant, snapLoc)
	if count != 1 {
		t.Errorf("detection_runs count for snap_loc=%s = %d; want exactly 1 (V0062 UNIQUE constraint)",
			snapLoc, count)
	}
}

// countDetectionRunsForLoc SELECTs count(*) FROM tenant_<uuid>.detection_runs
// WHERE snapshot_metadata_location = $1. Schema-qualified because the
// GraphClient pool's search_path doesn't include the tenant schema.
func countDetectionRunsForLoc(t *testing.T, ctx context.Context, gc *graph.GraphClient, tenantID, snapLoc string) int64 {
	t.Helper()
	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}
	schema := tenant.SchemaName(tenantUUID)
	qSchema := pgx.Identifier{schema, "detection_runs"}.Sanitize()
	query := fmt.Sprintf("SELECT count(*) FROM %s WHERE snapshot_metadata_location = $1", qSchema)

	var n int64 = -1
	err = gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, query, snapLoc)
		return row.Scan(&n)
	})
	if err != nil {
		t.Logf("countDetectionRunsForLoc error: %v", err)
		return -1
	}
	return n
}
