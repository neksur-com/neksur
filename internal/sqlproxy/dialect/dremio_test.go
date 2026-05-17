// Plan 03-05 Task 1 — DremioInjector unit tests (TDD RED phase).
//
// Nine behaviors covering the live DremioInjector that replaces the
// Phase 2 fail-closed stub. The test shapes mirror the
// splice_test.go contract and the plan's behavior spec.
//
// Store dependency is stubbed via the unexported compiledForTableLoader
// interface — no real database required.
//
// Per Pitfall 11: no query body is asserted in error branches; only
// error sentinel identity is checked.

package dialect_test

import (
	"context"
	"errors"
	"testing"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	"github.com/neksur-com/neksur/internal/sqlproxy/dialect"
	"github.com/neksur-com/neksur/internal/tenant"
)

// fakeCompiledStore is a test-double implementing dialect.CompiledLoader.
// It returns the configured policies or error without touching a database.
type fakeCompiledStore struct {
	policies []store.CompiledPolicy
	err      error
}

func (f *fakeCompiledStore) LoadCompiledForTable(_ context.Context, _ iceberg.TableRef) ([]store.CompiledPolicy, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.policies, nil
}

// newTestDremioInjector constructs a DremioInjector with the given fake store
// and a fresh 128-entry LRU cache.
func newTestDremioInjector(t *testing.T, fcs *fakeCompiledStore) (*dialect.DremioInjector, *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) {
	t.Helper()
	cache, err := lru.New[sqlproxy.CacheKey, sqlproxy.ArtifactEntry](128)
	require.NoError(t, err)
	inj := dialect.NewDremioInjectorWithLoader(fcs, cache)
	return inj, cache
}

// tenantCtx returns a context carrying a parsed tenant UUID.
func tenantCtx(t *testing.T, id string) context.Context {
	t.Helper()
	tid, err := uuid.Parse(id)
	require.NoError(t, err)
	return tenant.WithID(context.Background(), tid)
}

const dremioTestTenant = "d10cd10c-0305-4d11-8a11-111111111111"
const dremioTestTable = "orders"
const dremioTestNS = "ns"

// TestDremioInjector_CacheHit: cache pre-populated → InjectPolicy returns
// spliced query without touching the store.
func TestDremioInjector_CacheHit(t *testing.T) {
	fcs := &fakeCompiledStore{
		// If the cache is hit, LoadCompiledForTable must NOT be called.
		// Returning an error here ensures the test would fail if the injector
		// accidentally calls the store on a cache hit.
		err: errors.New("store must not be called on cache hit"),
	}
	inj, cache := newTestDremioInjector(t, fcs)

	cacheKey := sqlproxy.CacheKey{
		TenantID:  dremioTestTenant,
		Namespace: dremioTestNS,
		Table:     dremioTestTable,
		Engine:    "dremio",
	}
	cache.Add(cacheKey, sqlproxy.ArtifactEntry{
		Body: []byte("deleted = false"),
		Kind: store.KindRowFilter,
	})

	ctx := tenantCtx(t, dremioTestTenant)
	rewritten, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM orders",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.NoError(t, err)
	require.Equal(t, sqlproxy.CacheStatusHit, cacheStatus)
	require.Contains(t, normalize(rewritten), "WHERE (deleted = false)")
}

// TestDremioInjector_CacheMissAGELookup: cache empty → store returns active
// dremio CompiledPolicy → splice applied → cache populated.
func TestDremioInjector_CacheMissAGELookup(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "dremio",
				Status:       store.CompiledPolicyStatusActive,
				ArtifactBody: "deleted = false",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, cache := newTestDremioInjector(t, fcs)

	ctx := tenantCtx(t, dremioTestTenant)
	rewritten, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM orders",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.NoError(t, err)
	require.Equal(t, sqlproxy.CacheStatusMiss, cacheStatus)
	require.Contains(t, normalize(rewritten), "WHERE (deleted = false)")

	// Verify cache was populated.
	key := sqlproxy.CacheKey{
		TenantID:  dremioTestTenant,
		Namespace: dremioTestNS,
		Table:     dremioTestTable,
		Engine:    "dremio",
	}
	_, hit := cache.Get(key)
	require.True(t, hit, "cache must be populated after store miss")
}

// TestDremioInjector_NoPolicyForTable: store returns empty list → original
// query returned unchanged with CacheStatusMiss.
func TestDremioInjector_NoPolicyForTable(t *testing.T) {
	fcs := &fakeCompiledStore{policies: nil}
	inj, _ := newTestDremioInjector(t, fcs)

	ctx := tenantCtx(t, dremioTestTenant)
	_, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM orders",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	// No active policy → fail-closed sentinel.
	require.Error(t, err)
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
	require.Equal(t, sqlproxy.CacheStatusMiss, cacheStatus)
}

// TestDremioInjector_RowFilterSimple: "deleted = false" + "SELECT id FROM t"
// → "SELECT id FROM t WHERE (deleted = false)".
func TestDremioInjector_RowFilterSimple(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "dremio",
				Status:       store.CompiledPolicyStatusActive,
				ArtifactBody: "deleted = false",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, _ := newTestDremioInjector(t, fcs)
	ctx := tenantCtx(t, dremioTestTenant)

	rewritten, _, err := inj.InjectPolicy(ctx, "SELECT id FROM t",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.NoError(t, err)
	require.Equal(t, "SELECT id FROM t WHERE (deleted = false)", normalize(rewritten))
}

// TestDremioInjector_RowFilterWithWhere: existing WHERE is AND-conjoined.
func TestDremioInjector_RowFilterWithWhere(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "dremio",
				Status:       store.CompiledPolicyStatusActive,
				ArtifactBody: "tenant_id = current_tenant_id()",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, _ := newTestDremioInjector(t, fcs)
	ctx := tenantCtx(t, dremioTestTenant)

	rewritten, _, err := inj.InjectPolicy(ctx, "SELECT id FROM t WHERE id > 10",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.NoError(t, err)
	require.Equal(t,
		"SELECT id FROM t WHERE (id > 10) AND (tenant_id = current_tenant_id())",
		normalize(rewritten))
}

// TestDremioInjector_ColumnMaskProjection: column-mask artifact rewrites
// projection list.
func TestDremioInjector_ColumnMaskProjection(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "dremio",
				Status:       store.CompiledPolicyStatusActive,
				ArtifactBody: "ssn AS '***'",
				ArtifactKind: store.KindColumnMask,
			},
		},
	}
	inj, _ := newTestDremioInjector(t, fcs)
	ctx := tenantCtx(t, dremioTestTenant)

	rewritten, _, err := inj.InjectPolicy(ctx, "SELECT id, ssn, email FROM customers",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.NoError(t, err)
	require.Equal(t,
		"SELECT id, '***' AS ssn, email FROM customers",
		normalize(rewritten))
}

// TestDremioInjector_UnsupportedJOIN: JOIN query returns ErrUnsupportedQueryShape.
func TestDremioInjector_UnsupportedJOIN(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "dremio",
				Status:       store.CompiledPolicyStatusActive,
				ArtifactBody: "deleted = false",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, _ := newTestDremioInjector(t, fcs)
	ctx := tenantCtx(t, dremioTestTenant)

	_, _, err := inj.InjectPolicy(ctx, "SELECT * FROM a JOIN b ON a.id = b.id",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.ErrorIs(t, err, sqlproxy.ErrUnsupportedQueryShape)
}

// TestDremioInjector_TenantMissing: ctx without tenant → ErrPolicyEngineUnavailable.
func TestDremioInjector_TenantMissing(t *testing.T) {
	fcs := &fakeCompiledStore{}
	inj, _ := newTestDremioInjector(t, fcs)

	// Bare context.Background() carries no tenant.
	_, _, err := inj.InjectPolicy(context.Background(), "SELECT id FROM orders",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
}

// TestDremioInjector_DivergentSuspended: CompiledPolicy with
// status=divergent_suspended → fail-closed ErrPolicyEngineUnavailable.
func TestDremioInjector_DivergentSuspended(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "dremio",
				Status:       store.CompiledPolicyStatusDivergentSuspended,
				ArtifactBody: "deleted = false",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, _ := newTestDremioInjector(t, fcs)
	ctx := tenantCtx(t, dremioTestTenant)

	_, _, err := inj.InjectPolicy(ctx, "SELECT id FROM orders",
		sqlproxy.TableRef{Namespace: dremioTestNS, Name: dremioTestTable},
		sqlproxy.Claims{})
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
}
