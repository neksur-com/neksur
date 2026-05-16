//go:build integration

// Plan 02-04 Task BLOCKING — TestCompiledPolicyMERGE.
//
// 5 concurrent goroutines call UpsertCompiledPolicy with the same
// (policy_id, engine_kind, engine_version, table) tuple. Assert
// exactly 1 CompiledPolicy node + 3 edges per (policy_id, engine_kind)
// via Cypher count.
//
// Why this matters: AGE 1.6 has a known quirk where MERGE inside a
// single cypher() call can spuriously create duplicate nodes under
// race when properties collide. The store layer's
// UpsertCompiledPolicy decomposes into one MERGE per cypher() call
// (compiled.go:138 — "each MERGE is issued in its own cypher() call");
// this test is the regression guard for that decomposition.
//
// The 3 edges: COMPILED_FROM (→ Policy), APPLIES_TO (→ Table),
// GOVERNED_BY (→ Engine). Counts via Cypher MATCH count(*).

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const mergeIdempotencyTenant = "1de111fe-0204-4555-8a55-555555555555"

// TestCompiledPolicyMERGE — see file header.
func TestCompiledPolicyMERGE(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, mergeIdempotencyTenant)
	_ = fx.ProvisionEngineRegistry(t, mergeIdempotencyTenant, []string{"trino"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Seed the Policy + Table prerequisites — UpsertCompiledPolicy's
	// COMPILED_FROM / APPLIES_TO MATCHes target these nodes.
	const policyID = "merge-idem-policy"
	const tableName = "merge_orders"
	const ns = "test"
	seedPolicyOfKind(t, gc, mergeIdempotencyTenant, policyID,
		`true`, tableName, ns, "Policy", "schema", "SCHEMA_GOVERNS")

	tenantUUID := uuid.MustParse(mergeIdempotencyTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	cstore := store.NewCompiledStore(gc)
	cp := store.CompiledPolicy{
		PolicyID:       policyID,
		EngineKind:     "trino",
		EngineVersion:  "467",
		TableName:      tableName,
		TableNamespace: ns,
		Status:         store.CompiledPolicyStatusActive,
		SourceChecksum: "deadbeef",
		ArtifactBody:   `SELECT 1`,
	}

	const N = 5
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if err := cstore.UpsertCompiledPolicy(ctx, cp); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent UpsertCompiledPolicy: %v", e)
	}

	// Cypher counts via the shared countMatching helper
	// (ingest_merge_idempotent_test.go).
	nodeCount := countMatching(t, ctx, gc, mergeIdempotencyTenant, fmt.Sprintf(
		`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: 'trino', engine_version: '467'}) RETURN count(cp)`,
		mergeIdempotencyTenant, policyID,
	))
	if nodeCount != 1 {
		t.Errorf("CompiledPolicy node count = %d; want 1 (concurrent MERGE created duplicates)", nodeCount)
	}

	edgeCases := []struct {
		name  string
		query string
	}{
		{
			name: "COMPILED_FROM",
			query: fmt.Sprintf(
				`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: 'trino'})-[r:COMPILED_FROM]->(:Policy {tenant_id: '%s', id: '%s'}) RETURN count(r)`,
				mergeIdempotencyTenant, policyID, mergeIdempotencyTenant, policyID,
			),
		},
		{
			name: "APPLIES_TO",
			query: fmt.Sprintf(
				`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: 'trino'})-[r:APPLIES_TO]->(:Table {tenant_id: '%s', name: '%s', namespace: '%s'}) RETURN count(r)`,
				mergeIdempotencyTenant, policyID, mergeIdempotencyTenant, tableName, ns,
			),
		},
		{
			name: "GOVERNED_BY",
			query: fmt.Sprintf(
				`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: 'trino'})-[r:GOVERNED_BY]->(:Engine {tenant_id: '%s', kind: 'trino', version: '467'}) RETURN count(r)`,
				mergeIdempotencyTenant, policyID, mergeIdempotencyTenant,
			),
		},
	}
	for _, c := range edgeCases {
		got := countMatching(t, ctx, gc, mergeIdempotencyTenant, c.query)
		if got != 1 {
			t.Errorf("edge %s count = %d; want 1 (race produced duplicates)", c.name, got)
		}
	}
}
