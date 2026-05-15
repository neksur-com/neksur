//go:build integration

// Package integration — Phase 1 test fixtures.
//
// Phase1Fixture extends SaasFixture (Phase 0.5) with the three new
// containers Phase 1 needs:
//   - Polaris    (Iceberg REST catalog reference, Plans 01-02, 01-06)
//   - Nessie     (branching catalog, Plan 01-03 adapter)
//   - LocalStack (S3 + SNS + SQS for Plan 01-07 detection events)
//
// And teaches the per-tenant provisioning path how to apply Plan 01-01's
// graph migrations (V0030–V0032) via internal/migrate.ApplyTenantGraph,
// plus seed a `catalog_credentials` row pointing at the Polaris fixture
// so downstream plan tests can immediately look up "the catalog for this
// tenant".
//
// Container bootstrap is concurrent — once SaasFixture is up, the three
// new containers boot in parallel via a sync.WaitGroup so the wall-clock
// addition over SaasFixture is the slowest of the three (~30s typical)
// rather than their sum (~90s serial).

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/migrate"
	"github.com/neksur-com/neksur/tests/testfixture"
)

// Phase1Fixture is the Phase 1 integration target. It composes
// SaasFixture (Phase 0.5) with the three new containers and a
// long-lived admin pool used by the per-tenant provisioning path.
type Phase1Fixture struct {
	*SaasFixture

	Polaris    *testfixture.PolarisContainer
	Nessie     *testfixture.NessieContainer
	LocalStack *testfixture.LocalStackContainer

	pool *pgxpool.Pool

	ctx    context.Context
	cancel context.CancelFunc
}

// StartPhase1Fixture boots the SaasFixture first (sequential — its
// migrations have to land before anything else), then forks three
// goroutines to start Polaris, Nessie, and LocalStack in parallel.
// On any container start error, every container started so far is
// terminated and t.Fatal is invoked.
//
// Total wall-clock: ~30-60s warm, up to ~3-5min on a cold image pull
// (Polaris is the dominant cost — JVM bootstrap plus ~600MB image).
func StartPhase1Fixture(t *testing.T) *Phase1Fixture {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping Phase 1 fixture")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	saas := StartSaasFixture(t)

	// Reuse the SaasFixture's superuser DSN to build a long-lived admin
	// pool. ApplyTenantGraph + catalog_credentials INSERTs run against
	// this pool inside ProvisionTenant.
	pool, err := pgxpool.New(ctx, saas.Container.SuperuserDSN)
	if err != nil {
		saas.Terminate()
		cancel()
		t.Fatalf("StartPhase1Fixture: pgxpool.New: %v", err)
	}

	var (
		wg          sync.WaitGroup
		polaris     *testfixture.PolarisContainer
		nessie      *testfixture.NessieContainer
		localStack  *testfixture.LocalStackContainer
		polarisErr  error
		nessieErr   error
		localStkErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		polaris, polarisErr = testfixture.StartPolaris(ctx)
	}()
	go func() {
		defer wg.Done()
		nessie, nessieErr = testfixture.StartNessie(ctx)
	}()
	go func() {
		defer wg.Done()
		localStack, localStkErr = testfixture.StartLocalStack(ctx)
	}()
	wg.Wait()

	// On any failure, tear down whatever started and bail.
	if polarisErr != nil || nessieErr != nil || localStkErr != nil {
		if polaris != nil {
			_ = polaris.Terminate(ctx)
		}
		if nessie != nil {
			_ = nessie.Terminate(ctx)
		}
		if localStack != nil {
			_ = localStack.Terminate(ctx)
		}
		pool.Close()
		saas.Terminate()
		cancel()
		t.Fatalf("StartPhase1Fixture: container bootstrap failed: polaris=%v nessie=%v localstack=%v",
			polarisErr, nessieErr, localStkErr)
	}

	return &Phase1Fixture{
		SaasFixture: saas,
		Polaris:     polaris,
		Nessie:      nessie,
		LocalStack:  localStack,
		pool:        pool,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Terminate shuts down every container in reverse-start order and
// closes the admin pool. Safe to call multiple times.
func (f *Phase1Fixture) Terminate() {
	if f == nil {
		return
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer tcancel()
	if f.LocalStack != nil {
		_ = f.LocalStack.Terminate(tctx)
	}
	if f.Nessie != nil {
		_ = f.Nessie.Terminate(tctx)
	}
	if f.Polaris != nil {
		_ = f.Polaris.Terminate(tctx)
	}
	if f.pool != nil {
		f.pool.Close()
	}
	if f.SaasFixture != nil {
		f.SaasFixture.Terminate()
	}
	if f.cancel != nil {
		f.cancel()
	}
}

// ProvisionTenant runs the SaasFixture's per-tenant Atlas pass
// (V0050–V0066), then applies the Phase 1 graph migrations
// (V0030–V0032) via internal/migrate.ApplyTenantGraph, then seeds a
// `catalog_credentials` row in the new tenant schema pointing at the
// Polaris fixture so downstream tests can immediately look up the
// catalog endpoint.
//
// Returns the resulting Postgres schema name (`tenant_<uuid_with_underscores>`).
//
// Idempotency: the underlying SaasFixture.ProvisionTenant guards
// schema + role create with IF NOT EXISTS; ApplyTenantGraph skips
// already-applied versions via the per-tenant graph_schema_revisions
// table; the catalog_credentials INSERT uses ON CONFLICT DO NOTHING
// on `nickname` so a second call is a no-op.
func (f *Phase1Fixture) ProvisionTenant(t *testing.T, tenantUUID string) string {
	t.Helper()
	schema := f.SaasFixture.ProvisionTenant(t, tenantUUID)

	if err := migrate.ApplyTenantGraph(f.ctx, f.pool, schema); err != nil {
		t.Fatalf("Phase1Fixture.ProvisionTenant: ApplyTenantGraph(%s): %v", schema, err)
	}

	// Seed a polaris row so plan tests can SELECT * FROM catalog_credentials
	// and find a working endpoint with no additional setup.
	configJSON, _ := json.Marshal(map[string]any{
		"endpoint":       f.Polaris.Endpoint,
		"warehouse":      "test",
		"clientId":       f.Polaris.ClientID,
		"clientSecret":   f.Polaris.ClientSecret,
		"scope":          "PRINCIPAL_ROLE:ALL",
		"credentialMode": "passthrough",
	})

	// Use the admin pool so the INSERT bypasses Layer 2 GRANT scoping
	// (V0066 attached FORCE RLS to catalog_credentials with a "GUC must
	// be set" predicate; we set app.current_tenant explicitly so the
	// predicate passes for this admin transaction). Schema-qualified
	// identifier prevents reliance on search_path.
	qSchema := pgx.Identifier{schema}.Sanitize()
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("Phase1Fixture.ProvisionTenant: pool acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(f.ctx,
		"SELECT set_config('app.current_tenant', $1, true)", tenantUUID); err != nil {
		t.Fatalf("Phase1Fixture.ProvisionTenant: set_config: %v", err)
	}
	insertSQL := fmt.Sprintf(`
        INSERT INTO %s.catalog_credentials (catalog_kind, nickname, endpoint, config_json)
        VALUES ('polaris', 'prod-polaris', $1, $2::jsonb)
        ON CONFLICT (nickname) DO NOTHING
    `, qSchema)
	if _, err := conn.Exec(f.ctx, insertSQL, f.Polaris.Endpoint, string(configJSON)); err != nil {
		t.Fatalf("Phase1Fixture.ProvisionTenant: insert catalog_credentials: %v", err)
	}

	return schema
}
