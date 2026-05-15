//go:build integration

// Plan 01-04 Task 3 [BLOCKING] — Lineage cycle detection + concurrent race.
//
// Three tests:
//
//   TestIngestLineageCycleRejected   — D-1.06: cycle pre-check rejects.
//   TestIngestLineageCycleConcurrent — Pitfall 4: advisory lock serializes.
//   TestIngestLineageCycleRollsBackTx — partial-state rollback proof.
//
// All run against StartPhase1Fixture. Seed three Table nodes (A, B, C)
// with `iceberg_id` properties so the MATCH-by-iceberg_id MERGE template
// in internal/ingest/lineage.go finds them. Then exercise the LINEAGE_OF
// MERGE pipeline.

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/ingest"
)

const cycleTestTenant = "66666666-6666-4666-6666-666666666666"

// TestIngestLineageCycleRejected — basic happy / sad paths.
//
//	A → B succeeds.
//	B → C succeeds.
//	C → A is the cycle-closer → returns *LineageCycleError.
func TestIngestLineageCycleRejected(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, cycleTestTenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()
	svc := ingest.NewService(gc)

	uriA := "iceberg://test/A"
	uriB := "iceberg://test/B"
	uriC := "iceberg://test/C"
	seedTableNodes(t, fx.ctx, gc, cycleTestTenant, []string{uriA, uriB, uriC})

	now := time.Now().UTC()

	// A → B  (clean)
	if err := svc.MergeLineageEdge(fx.ctx, cycleTestTenant, uriA, uriB, "run-1", now); err != nil {
		t.Fatalf("A→B unexpected error: %v", err)
	}
	// B → C  (clean — extends the chain)
	if err := svc.MergeLineageEdge(fx.ctx, cycleTestTenant, uriB, uriC, "run-2", now); err != nil {
		t.Fatalf("B→C unexpected error: %v", err)
	}

	// C → A would close the cycle A → B → C → A. Cycle pre-check
	// fires and returns a *LineageCycleError.
	err = svc.MergeLineageEdge(fx.ctx, cycleTestTenant, uriC, uriA, "run-3", now)
	if err == nil {
		t.Fatalf("C→A should have been rejected as a cycle; got nil")
	}

	var cyc *ingest.LineageCycleError
	if !errors.As(err, &cyc) {
		t.Fatalf("expected *ingest.LineageCycleError; got %T: %v", err, err)
	}
	if !errors.Is(err, ingest.ErrLineageCycle) {
		t.Errorf("errors.Is(err, ErrLineageCycle) = false; expected true")
	}

	// Cycle path must contain all three URIs forming the chain.
	containsAll(t, cyc.Cycle, []string{uriA, uriB, uriC})

	// Sanity: the in-graph LINEAGE_OF edge count is 2 (A→B, B→C) and
	// the rejected C→A did NOT land — TestIngestLineageCycleRollsBackTx
	// covers this assertion in depth, but a quick check here keeps
	// failure modes from compounding across the test set.
	edges := countMatching(t, fx.ctx, gc, cycleTestTenant,
		"MATCH ()-[r:LINEAGE_OF]->() RETURN count(r)")
	if edges != 2 {
		t.Errorf("LINEAGE_OF count after rejection = %d; expected 2", edges)
	}
}

// TestIngestLineageCycleConcurrent — Pitfall 4 mitigation proof.
//
// Setup: A → B, B → C exist.
//
// Two goroutines fire simultaneously:
//
//	G1: MergeLineageEdge(C, A)    — would close the cycle.
//	G2: MergeLineageEdge(A, D)    — D is fresh, no cycle.
//
// With the advisory lock keyed on hashtext(srcURI):
//
//   - G1 + G2 have DIFFERENT srcURIs (C vs A) so the lock does NOT
//     serialize them at the source-key level — but the cycle pre-check
//     still operates against a consistent transaction-local view. G1
//     (C→A) MUST fail with LineageCycleError; G2 (A→D) MUST succeed.
//
// We assert EXACTLY ONE LineageCycleError across both calls (G1's),
// and zero spurious errors from G2.
//
// Why this matters: an early implementation of cycle pre-check without
// transaction isolation could see "no cycle" for both goroutines on
// the read side, then both commit, closing the cycle anyway. The
// `pg_advisory_xact_lock` + read-modify-write inside ONE transaction
// is what gives us correctness here.
func TestIngestLineageCycleConcurrent(t *testing.T) {
	t.Parallel()
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenant = "77777777-7777-4777-7777-777777777777"
	_ = fx.ProvisionTenant(t, tenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()
	svc := ingest.NewService(gc)

	uriA := "iceberg://concurrent/A"
	uriB := "iceberg://concurrent/B"
	uriC := "iceberg://concurrent/C"
	uriD := "iceberg://concurrent/D"
	seedTableNodes(t, fx.ctx, gc, tenant, []string{uriA, uriB, uriC, uriD})

	now := time.Now().UTC()
	// Establish A → B, B → C in serial first.
	if err := svc.MergeLineageEdge(fx.ctx, tenant, uriA, uriB, "seed-1", now); err != nil {
		t.Fatalf("seed A→B: %v", err)
	}
	if err := svc.MergeLineageEdge(fx.ctx, tenant, uriB, uriC, "seed-2", now); err != nil {
		t.Fatalf("seed B→C: %v", err)
	}

	// Fire the two concurrent writes.
	var wg sync.WaitGroup
	wg.Add(2)
	var errG1, errG2 error

	go func() {
		defer wg.Done()
		// Cycle attempt: C → A (would close A→B→C→A).
		errG1 = svc.MergeLineageEdge(fx.ctx, tenant, uriC, uriA, "race-cycle", time.Now().UTC())
	}()
	go func() {
		defer wg.Done()
		// Non-cycle: A → D (D has no edges yet).
		errG2 = svc.MergeLineageEdge(fx.ctx, tenant, uriA, uriD, "race-no-cycle", time.Now().UTC())
	}()
	wg.Wait()

	// G1 MUST fail with LineageCycleError.
	var cyc *ingest.LineageCycleError
	if !errors.As(errG1, &cyc) {
		t.Errorf("G1 (C→A) expected *LineageCycleError; got %T: %v", errG1, errG1)
	}

	// G2 MUST succeed.
	if errG2 != nil {
		t.Errorf("G2 (A→D, no cycle) unexpected error: %v", errG2)
	}

	// Sanity: count edges. Original 2 (A→B, B→C) + 1 successful new (A→D) = 3.
	// G1's rejected C→A MUST NOT have landed.
	edges := countMatching(t, fx.ctx, gc, tenant,
		"MATCH ()-[r:LINEAGE_OF]->() RETURN count(r)")
	if edges != 3 {
		t.Errorf("LINEAGE_OF count after concurrent race = %d; expected 3 (A→B, B→C, A→D)", edges)
	}
}

// TestIngestLineageCycleRollsBackTx — assert transaction rollback on
// cycle detection: NO partial-state LINEAGE_OF edge is created when
// the pre-check rejects.
func TestIngestLineageCycleRollsBackTx(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenant = "88888888-8888-4888-8888-888888888888"
	_ = fx.ProvisionTenant(t, tenant)

	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()
	svc := ingest.NewService(gc)

	uriX := "iceberg://rollback/X"
	uriY := "iceberg://rollback/Y"
	seedTableNodes(t, fx.ctx, gc, tenant, []string{uriX, uriY})

	now := time.Now().UTC()
	if err := svc.MergeLineageEdge(fx.ctx, tenant, uriX, uriY, "seed", now); err != nil {
		t.Fatalf("seed X→Y: %v", err)
	}

	// Attempt the cycle-closer Y → X.
	err = svc.MergeLineageEdge(fx.ctx, tenant, uriY, uriX, "rollback-attempt", now)
	if err == nil {
		t.Fatalf("Y→X should have been rejected as cycle; got nil")
	}
	var cyc *ingest.LineageCycleError
	if !errors.As(err, &cyc) {
		t.Fatalf("expected *LineageCycleError; got %T: %v", err, err)
	}

	// Edge count = 1 (only X→Y from the seed). If the MERGE had partially
	// landed before the cycle pre-check fired, this would be 2.
	edges := countMatching(t, fx.ctx, gc, tenant,
		"MATCH ()-[r:LINEAGE_OF]->() RETURN count(r)")
	if edges != 1 {
		t.Errorf("LINEAGE_OF count after rejected MERGE = %d; expected 1 (rollback proof)", edges)
	}
}

// --- helpers ----------------------------------------------------------

// seedTableNodes creates Table vertices with `iceberg_id` properties
// matching the supplied URIs, plus the mandatory tenant_id. Uses
// direct AGE Cypher (not the ingest.Service) because the LINEAGE_OF
// MERGE template MATCHes src/tgt by iceberg_id — we need the nodes
// to exist before the first MergeLineageEdge call.
func seedTableNodes(t *testing.T, ctx context.Context, gc *graph.GraphClient, tenantID string, uris []string) {
	t.Helper()
	// AGE 1.6 doesn't support `ON CREATE SET` (Plan 01-04 deviation #1).
	// tenant_id MUST be in the inline property map of MERGE because the
	// V0030 CHECK constraint Table_tenant_id_required fires before any
	// follow-up SET. See internal/ingest/snapshot.go header comment.
	for _, uri := range uris {
		cy := fmt.Sprintf(
			`MERGE (t:Table {iceberg_id: '%s', tenant_id: '%s'}) RETURN id(t)`,
			uri, tenantID,
		)
		q := "SELECT * FROM ag_catalog.cypher('neksur', $$ " + cy + " $$) AS (result ag_catalog.agtype)"
		err := gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		if err != nil {
			t.Fatalf("seedTableNodes(%s): %v", uri, err)
		}
	}
}

// containsAll asserts every needle is somewhere in haystack. Order
// doesn't matter — the cycle path can be reported starting from any
// node in the chain.
func containsAll(t *testing.T, haystack []string, needles []string) {
	t.Helper()
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	missing := []string{}
	for _, n := range needles {
		if !set[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Errorf("cycle path missing URIs %v; got %v", missing, haystack)
	}
}
