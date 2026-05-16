//go:build integration

// Package integration — Phase 2 test fixtures.
//
// Phase2Fixture extends Phase1Fixture (which itself extends SaasFixture)
// with two new containers Phase 2 needs:
//   - Trino  (read-path reference engine, Plans 02-04, 02-05, 02-15)
//   - Spark  (write-path L2 Extension, Plans 02-08..02-10, 02-14)
//
// And teaches a small `ProvisionEngineRegistry` helper that seeds
// public.engines (V0070) with trino + spark + dremio rows for a tenant
// so the cross-engine compiler tests (Plan 02-04) have a known engine set.
//
// Container bootstrap is concurrent — once Phase1Fixture is up, the two
// new containers boot in parallel via a sync.WaitGroup so wall-clock
// addition over Phase1Fixture is max(Trino, Spark) rather than their
// sum.
//
// Per Phase 0.5 CC3 (no second connection pool): Phase2Fixture REUSES
// the embedded Phase1Fixture's `pool` for the engine-registry seed —
// no new pool constructor call inside this file.

package integration

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/tests/testfixture"
)

// Phase2Fixture is the Phase 2 integration target. It embeds
// *Phase1Fixture (which itself embeds *SaasFixture) and adds the two
// new containers Phase 2 needs.
type Phase2Fixture struct {
	*Phase1Fixture

	Trino *testfixture.TrinoContainer
	Spark *testfixture.SparkContainer
}

// StartPhase2Fixture boots the Phase1Fixture first (sequential — its
// migrations and Polaris/Nessie/LocalStack containers have to land
// before anything else), then forks two goroutines to start Trino and
// Spark in parallel. On any container start error, every container
// started so far is terminated and t.Fatal is invoked.
//
// Total wall-clock: ~60-120s warm, up to ~3-5min cold (Spark + Trino
// image pulls dominate; Phase1Fixture's Polaris is the next-heaviest).
func StartPhase2Fixture(t *testing.T) *Phase2Fixture {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping Phase 2 fixture")
	}

	phase1 := StartPhase1Fixture(t)

	// Allocate the parallel-start budget against the phase1 ctx so the
	// overall fixture deadline (10min in Phase1Fixture) applies across
	// the whole boot sequence.
	ctx, cancel := context.WithTimeout(phase1.ctx, 8*time.Minute)
	defer cancel()

	var (
		wg       sync.WaitGroup
		trino    *testfixture.TrinoContainer
		spark    *testfixture.SparkContainer
		trinoErr error
		sparkErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		trino, trinoErr = testfixture.StartTrino(ctx)
	}()
	go func() {
		defer wg.Done()
		spark, sparkErr = testfixture.StartSpark(ctx)
	}()
	wg.Wait()

	if trinoErr != nil || sparkErr != nil {
		if trino != nil {
			_ = trino.Terminate(ctx)
		}
		if spark != nil {
			_ = spark.Terminate(ctx)
		}
		phase1.Terminate()
		t.Fatalf("StartPhase2Fixture: container bootstrap failed: trino=%v spark=%v",
			trinoErr, sparkErr)
	}

	return &Phase2Fixture{
		Phase1Fixture: phase1,
		Trino:         trino,
		Spark:         spark,
	}
}

// Terminate shuts down Phase 2's new containers, then delegates to
// Phase1Fixture.Terminate. Safe to call multiple times.
func (f *Phase2Fixture) Terminate() {
	if f == nil {
		return
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer tcancel()
	if f.Spark != nil {
		_ = f.Spark.Terminate(tctx)
	}
	if f.Trino != nil {
		_ = f.Trino.Terminate(tctx)
	}
	if f.Phase1Fixture != nil {
		f.Phase1Fixture.Terminate()
	}
}

// ProvisionEngineRegistry seeds public.engines (V0070) with rows for
// the tenant under test. The default seed includes trino, spark, and
// dremio at canonical versions matching Phase 2 fixtures — snowflake is
// deliberately omitted (D-2.08: snowflake is plumbing-only this phase,
// no live dialect yet).
//
// The engineKinds parameter lets per-test seeds vary (e.g., a test
// that only exercises Trino can pass []string{"trino"}). When nil, the
// default trino+spark+dremio set lands.
//
// Returns the engine IDs in stable order matching the engineKinds order
// (or the default order if engineKinds was nil). Callers can map these
// to CompiledPolicy.GOVERNED_BY edge targets.
//
// Idempotent: the underlying UNIQUE (tenant_id, kind, version) constraint
// + ON CONFLICT DO NOTHING means a second call is a no-op that returns
// the existing IDs. Uses the Phase1Fixture admin pool (CC3 — no second
// pgxpool).
func (f *Phase2Fixture) ProvisionEngineRegistry(t *testing.T, tenantUUID string, engineKinds []string) []uuid.UUID {
	t.Helper()

	if engineKinds == nil {
		engineKinds = []string{"trino", "spark", "dremio"}
	}

	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("Phase2Fixture.ProvisionEngineRegistry: pool acquire: %v", err)
	}
	defer conn.Release()

	versions := map[string]string{
		"trino":     "467",
		"spark":     "3.5.4",
		"dremio":    "25.0",
		"snowflake": "horizon-r2",
	}
	endpoints := map[string]string{
		"trino":     f.Trino.Endpoint,
		"spark":     "spark://localhost:7077", // placeholder; Plan 02-09 wires real master URL
		"dremio":    "http://localhost:9047",  // placeholder; Phase 3 brings live Dremio
		"snowflake": "https://example.snowflakecomputing.com",
	}

	ids := make([]uuid.UUID, 0, len(engineKinds))
	for _, kind := range engineKinds {
		ver, ok := versions[kind]
		if !ok {
			t.Fatalf("Phase2Fixture.ProvisionEngineRegistry: unknown engine kind %q", kind)
		}
		endpoint, ok := endpoints[kind]
		if !ok {
			t.Fatalf("Phase2Fixture.ProvisionEngineRegistry: no endpoint for kind %q", kind)
		}
		id := uuid.New()
		// ON CONFLICT DO NOTHING + RETURNING handles the idempotent
		// re-call shape: if the (tenant, kind, version) row exists we
		// don't get a RETURNING, so the SELECT below picks up the
		// existing id.
		insertSQL := `
            INSERT INTO public.engines (id, tenant_id, kind, version, endpoint_url)
            VALUES ($1, $2::uuid, $3, $4, $5)
            ON CONFLICT (tenant_id, kind, version) DO NOTHING
            RETURNING id
        `
		var insertedID uuid.UUID
		err := conn.QueryRow(f.ctx, insertSQL, id, tenantUUID, kind, ver, endpoint).Scan(&insertedID)
		if err != nil {
			// pgx.ErrNoRows on conflict — fall through to SELECT lookup.
			lookupSQL := `SELECT id FROM public.engines WHERE tenant_id = $1::uuid AND kind = $2 AND version = $3`
			if lerr := conn.QueryRow(f.ctx, lookupSQL, tenantUUID, kind, ver).Scan(&insertedID); lerr != nil {
				t.Fatalf("Phase2Fixture.ProvisionEngineRegistry: insert %s: %v; lookup: %v", kind, err, lerr)
			}
		}
		ids = append(ids, insertedID)
	}

	return ids
}

// canaryEngineRowProbe is a tiny helper for the Wave-0
// phase2_migrations_applied_per_tenant_test — it verifies that the V0070
// public.engines table exists by INSERTing + immediately DELETing a
// no-op probe row. The test uses the helper as a fail-fast schema
// existence check; full ProvisionEngineRegistry coverage lives in the
// downstream plan tests.
func canaryEngineRowProbe(ctx context.Context, fx *Phase2Fixture, tenantUUID string) error {
	conn, err := fx.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("canaryEngineRowProbe: acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx,
		`INSERT INTO public.engines (tenant_id, kind, version, endpoint_url)
         VALUES ($1::uuid, 'trino', '0-canary', 'http://noop')
         ON CONFLICT (tenant_id, kind, version) DO NOTHING`,
		tenantUUID); err != nil {
		return fmt.Errorf("canaryEngineRowProbe: insert: %w", err)
	}
	if _, err := conn.Exec(ctx,
		`DELETE FROM public.engines WHERE tenant_id = $1::uuid AND version = '0-canary'`,
		tenantUUID); err != nil {
		return fmt.Errorf("canaryEngineRowProbe: delete: %w", err)
	}
	return nil
}
