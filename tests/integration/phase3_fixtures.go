//go:build integration

// Package integration — Phase 3 test fixtures.
//
// Phase3Fixture extends Phase2Fixture (which itself extends Phase1Fixture
// + SaasFixture) with four new engine fixtures Phase 3 needs:
//   - Dremio  (unconditional — always started; live Dremio OSS container)
//   - Glue    (unconditional — LocalStack-backed Glue+S3)
//   - Unity   (conditional — live creds from env; skipped when absent)
//   - Snowflake (conditional — live creds from env; nightly CI only)
//
// Container bootstrap is concurrent — Dremio + Glue start in parallel
// after Phase2Fixture completes so wall-clock addition is
// max(Dremio, Glue) rather than their sum.
//
// Per Phase 0.5 CC3 (no second connection pool): Phase3Fixture REUSES
// the embedded Phase2Fixture's `pool` for the engine-registry seed.
//
// Integration helpers:
//   - IcebergRESTEndpointForEngine(kind) — returns the correct Iceberg
//     REST URL for each engine kind (dremio, snowflake, unity, glue,
//     trino, spark, polaris). Phase 2 analog: Phase2Fixture already
//     handles trino via ProvisionEngineRegistry.
//   - RegisterEngineRegistry(t, tenantID) — seeds public.engines with
//     all available Phase 3 engine rows.

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

// Phase3Fixture is the Phase 3 integration target. It embeds *Phase2Fixture
// (which itself embeds *Phase1Fixture + *SaasFixture) and adds the four
// new engine fixtures Phase 3 requires.
type Phase3Fixture struct {
	*Phase2Fixture

	Dremio    *testfixture.DremioContainer
	Unity     *testfixture.UnityClient  // nil when env credentials absent
	Glue      *testfixture.GlueContainer
	Snowflake *testfixture.SnowflakeClient // nil when env credentials absent
}

// StartPhase3Fixture boots the Phase2Fixture first (sequential — its
// migrations and engine containers must land first), then forks goroutines
// to start Dremio + Glue in parallel. Unity + Snowflake are conditionally
// initialized from env vars (no goroutines — they're instant no-op reads).
//
// On any container start error, all containers started so far are terminated
// and t.Fatal is invoked.
//
// Total wall-clock: ~3-8min cold start (Dremio dominates at ~3min;
// Phase2Fixture's Trino+Spark+Polaris+Nessie+LocalStack are next).
func StartPhase3Fixture(t *testing.T) *Phase3Fixture {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping Phase 3 fixture")
	}

	phase2 := StartPhase2Fixture(t)

	ctx, cancel := context.WithTimeout(phase2.ctx, 10*time.Minute)
	defer cancel()

	var (
		wg       sync.WaitGroup
		dremio   *testfixture.DremioContainer
		glue     *testfixture.GlueContainer
		dremioErr error
		glueErr   error
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		dremio, dremioErr = testfixture.StartDremio(ctx)
	}()
	go func() {
		defer wg.Done()
		glue, glueErr = testfixture.StartGlue(ctx)
	}()
	wg.Wait()

	if dremioErr != nil || glueErr != nil {
		if dremio != nil {
			_ = dremio.Terminate(ctx)
		}
		if glue != nil {
			_ = glue.Terminate(ctx)
		}
		phase2.Terminate()
		t.Fatalf("StartPhase3Fixture: container bootstrap failed: dremio=%v glue=%v",
			dremioErr, glueErr)
	}

	// Unity + Snowflake: instant env-read, no goroutines.
	// These return nil (and call t.Skipf internally) when credentials absent,
	// but t.Skipf panics so we recover it and continue — the nil value means
	// the fixture will skip tests that require these engines.
	// Note: tests that REQUIRE Unity/Snowflake should call StartUnity/StartSnowflake
	// directly from their test function — the Phase3Fixture.Unity/Snowflake
	// fields are informational (non-nil means credentials available).
	var unity *testfixture.UnityClient
	var snowflake *testfixture.SnowflakeClient
	if hasUnityCredentials() {
		unity = startUnityConditional(t)
	}
	if hasSnowflakeCredentials() {
		snowflake = startSnowflakeConditional(t)
	}

	return &Phase3Fixture{
		Phase2Fixture: phase2,
		Dremio:        dremio,
		Unity:         unity,
		Glue:          glue,
		Snowflake:     snowflake,
	}
}

// Terminate shuts down Phase 3's new containers, then delegates to
// Phase2Fixture.Terminate. Safe to call multiple times.
func (f *Phase3Fixture) Terminate() {
	if f == nil {
		return
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer tcancel()
	if f.Dremio != nil {
		_ = f.Dremio.Terminate(tctx)
	}
	if f.Glue != nil {
		_ = f.Glue.Terminate(tctx)
	}
	// Unity + Snowflake are live-account clients — no containers to terminate.
	if f.Phase2Fixture != nil {
		f.Phase2Fixture.Terminate()
	}
}

// IcebergRESTEndpointForEngine returns the correct Iceberg REST URL for
// the given engine kind within the Phase 3 test environment. This is the
// canonical way for Phase 3 integration tests to discover the endpoint
// they should use when registering engine catalog configurations.
//
// Supported engine kinds: "dremio", "snowflake", "unity", "glue",
// "trino", "spark", "polaris".
func (f *Phase3Fixture) IcebergRESTEndpointForEngine(kind string) string {
	if f == nil {
		return ""
	}
	switch kind {
	case "dremio":
		if f.Dremio != nil {
			// Dremio consumes an Iceberg REST endpoint (Polaris), not hosts one.
			// Return the Phase1Fixture Polaris endpoint that Dremio should be
			// configured to use. Phase 3 adapter tests configure Dremio via API.
			return f.Polaris.Endpoint
		}
		return ""
	case "glue":
		if f.Glue != nil {
			return f.Glue.IcebergRESTEndpoint()
		}
		return ""
	case "unity":
		if f.Unity != nil {
			return f.Unity.IcebergRESTEndpoint()
		}
		return ""
	case "snowflake":
		// Snowflake-as-Iceberg-REST-client points at Polaris per D-3.01.
		return f.Polaris.Endpoint
	case "trino", "spark", "polaris":
		return f.Polaris.Endpoint
	default:
		return ""
	}
}

// RegisterEngineRegistry seeds public.engines (V0070) with rows for all
// available Phase 3 engines for the given tenant. Extends Phase2Fixture's
// ProvisionEngineRegistry with dremio, glue, unity, and snowflake.
//
// The method is idempotent (ON CONFLICT DO NOTHING pattern from Phase2Fixture).
// It skips engines whose fixtures are nil (credentials absent / container
// not started).
//
// Returns the engine UUIDs in insertion order matching engineKinds. Callers
// map these to CompiledPolicy.GOVERNED_BY edge targets.
func (f *Phase3Fixture) RegisterEngineRegistry(t *testing.T, tenantID string) []uuid.UUID {
	t.Helper()
	if f == nil {
		return nil
	}

	// Always-on Phase 3 engines (containers running).
	engineKinds := []string{"trino", "spark"}
	if f.Dremio != nil {
		engineKinds = append(engineKinds, "dremio")
	}
	if f.Glue != nil {
		engineKinds = append(engineKinds, "glue")
	}
	// Conditional Phase 3 engines (live creds only).
	if f.Unity != nil {
		engineKinds = append(engineKinds, "unity")
	}
	if f.Snowflake != nil {
		engineKinds = append(engineKinds, "snowflake")
	}

	// Extend Phase2Fixture's ProvisionEngineRegistry with Phase 3 engine
	// endpoint mappings. We call the parent helper with the extended kind list.
	// Phase2Fixture.ProvisionEngineRegistry already handles trino + spark;
	// the new kinds need their endpoint URLs injected via the Phase3Fixture.
	return f.provisionPhase3Engines(t, tenantID, engineKinds)
}

// provisionPhase3Engines is the Phase 3 extension of
// Phase2Fixture.ProvisionEngineRegistry that adds endpoint URLs for the
// Phase 3 engine kinds.
func (f *Phase3Fixture) provisionPhase3Engines(t *testing.T, tenantUUID string, engineKinds []string) []uuid.UUID {
	t.Helper()

	endpointFor := func(kind string) string {
		switch kind {
		case "trino":
			if f.Trino != nil {
				return f.Trino.Endpoint
			}
		case "spark":
			return "spark://localhost:7077" // placeholder; Plan 03-09 wires real endpoint
		case "dremio":
			if f.Dremio != nil {
				return f.Dremio.APIEndpoint
			}
		case "glue":
			if f.Glue != nil {
				return f.Glue.IcebergRESTEndpoint()
			}
		case "unity":
			if f.Unity != nil {
				return f.Unity.IcebergRESTEndpoint()
			}
		case "snowflake":
			if f.Snowflake != nil {
				return f.Snowflake.AccountURL()
			}
		}
		return fmt.Sprintf("http://noop-%s:0", kind) // default placeholder
	}

	versionFor := func(kind string) string {
		switch kind {
		case "trino":
			return "467"
		case "spark":
			return "3.5.4"
		case "dremio":
			return "25.0"
		case "glue":
			return "glue-3.0"
		case "unity":
			return "unity-2026"
		case "snowflake":
			return "horizon-r2"
		}
		return "unknown"
	}

	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("Phase3Fixture.provisionPhase3Engines: pool acquire: %v", err)
	}
	defer conn.Release()

	ids := make([]uuid.UUID, 0, len(engineKinds))
	for _, kind := range engineKinds {
		id := uuid.New()
		insertSQL := `
			INSERT INTO public.engines (id, tenant_id, kind, version, endpoint_url)
			VALUES ($1, $2::uuid, $3, $4, $5)
			ON CONFLICT (tenant_id, kind, version) DO NOTHING
			RETURNING id
		`
		var insertedID uuid.UUID
		err := conn.QueryRow(f.ctx, insertSQL, id, tenantUUID, kind, versionFor(kind), endpointFor(kind)).Scan(&insertedID)
		if err != nil {
			// pgx.ErrNoRows on conflict — fall through to SELECT lookup.
			lookupSQL := `SELECT id FROM public.engines WHERE tenant_id = $1::uuid AND kind = $2 AND version = $3`
			if lerr := conn.QueryRow(f.ctx, lookupSQL, tenantUUID, kind, versionFor(kind)).Scan(&insertedID); lerr != nil {
				t.Fatalf("Phase3Fixture.provisionPhase3Engines: insert %s: %v; lookup: %v", kind, err, lerr)
			}
		}
		ids = append(ids, insertedID)
	}
	return ids
}

// hasUnityCredentials returns true if all required Unity env vars are set.
// Does NOT validate values — just checks presence.
func hasUnityCredentials() bool {
	return os.Getenv("NEKSUR_UNITY_WORKSPACE_HOST") != "" &&
		os.Getenv("NEKSUR_UNITY_OAUTH_CLIENT_ID") != "" &&
		os.Getenv("NEKSUR_UNITY_OAUTH_CLIENT_SECRET") != "" &&
		os.Getenv("NEKSUR_UNITY_CATALOG_NAME") != ""
}

// hasSnowflakeCredentials returns true if all required Snowflake env vars are set.
// Does NOT validate values — just checks presence.
func hasSnowflakeCredentials() bool {
	return os.Getenv("NEKSUR_SNOWFLAKE_ACCOUNT") != "" &&
		os.Getenv("NEKSUR_SNOWFLAKE_USER") != "" &&
		os.Getenv("NEKSUR_SNOWFLAKE_PASSWORD") != "" &&
		os.Getenv("NEKSUR_SNOWFLAKE_WAREHOUSE") != ""
}

// startUnityConditional starts Unity without causing t.Skipf to abort
// the Phase3Fixture startup. Returns nil if credentials absent.
// The real t.Skipf behavior is deferred to tests that call StartUnity directly.
func startUnityConditional(t *testing.T) (result *testfixture.UnityClient) {
	t.Helper()
	// We can't call testfixture.StartUnity here without risking a skip
	// that aborts the fixture startup. Instead build the client directly.
	host := os.Getenv("NEKSUR_UNITY_WORKSPACE_HOST")
	catalogName := os.Getenv("NEKSUR_UNITY_CATALOG_NAME")
	if host == "" || catalogName == "" {
		return nil
	}
	// If we reach here, hasUnityCredentials() returned true, so all vars are set.
	// Construct via StartUnity on a dummy test helper that won't abort our caller.
	// Use a fresh sub-helper.
	var u *testfixture.UnityClient
	subtesting := &testing.T{}
	func() {
		defer func() { recover() }() // catch t.Skipf's panic gracefully
		u = testfixture.StartUnity(subtesting)
	}()
	return u
}

// startSnowflakeConditional starts Snowflake without causing t.Skipf to abort
// the Phase3Fixture startup. Returns nil if credentials absent.
func startSnowflakeConditional(t *testing.T) (result *testfixture.SnowflakeClient) {
	t.Helper()
	var s *testfixture.SnowflakeClient
	subtesting := &testing.T{}
	func() {
		defer func() { recover() }() // catch t.Skipf's panic gracefully
		s = testfixture.StartSnowflake(subtesting)
	}()
	return s
}
