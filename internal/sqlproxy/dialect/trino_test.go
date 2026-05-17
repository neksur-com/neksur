// Plan 03-16 Task 1 — TrinoInjector unit tests (TDD RED→GREEN).
//
// Covers the fail-closed contract for CompiledPolicyStatusDivergentSuspended
// (CR-02 gap closure) plus baseline cache-hit and no-policy behaviors.
//
// Store dependency is stubbed via the fakeCompiledStore + CompiledLoader
// interface defined in dremio_test.go (same package dialect_test) — no
// real database required.
//
// Per Pitfall 11: no query body is asserted in error branches; only
// error sentinel identity is checked.

package dialect_test

import (
	"context"
	"errors"
	"testing"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/stretchr/testify/require"

	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	"github.com/neksur-com/neksur/internal/sqlproxy/dialect"
)

const trinoTestTenant = "a10ca10c-0316-4a11-8a11-111111111111"
const trinoTestTable = "events"
const trinoTestNS = "trino_ns"

// newTestTrinoInjector constructs a TrinoInjector with the given fake store
// and a fresh 128-entry LRU cache.
func newTestTrinoInjector(t *testing.T, fcs *fakeCompiledStore) (*dialect.TrinoInjector, *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) {
	t.Helper()
	cache, err := lru.New[sqlproxy.CacheKey, sqlproxy.ArtifactEntry](128)
	require.NoError(t, err)
	inj := dialect.NewTrinoInjectorWithLoader(fcs, cache)
	return inj, cache
}

// TestTrinoInjector_DivergentSuspended: CompiledPolicy with
// status=divergent_suspended for the "trino" engine kind → fail-closed
// ErrPolicyEngineUnavailable with "policy_engine_divergent" in the error message.
func TestTrinoInjector_DivergentSuspended(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "trino",
				Status:       store.CompiledPolicyStatusDivergentSuspended,
				ArtifactBody: "deleted = false",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, _ := newTestTrinoInjector(t, fcs)
	ctx := tenantCtx(t, trinoTestTenant)

	_, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM events",
		sqlproxy.TableRef{Namespace: trinoTestNS, Name: trinoTestTable},
		sqlproxy.Claims{})
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
	require.Equal(t, sqlproxy.CacheStatusError, cacheStatus)
	require.Contains(t, err.Error(), "policy_engine_divergent")
}

// TestTrinoInjector_DivergentSuspended_PrecedenceOverActive: when both an
// Active row and a DivergentSuspended row exist for "trino" on the same table,
// InjectPolicy must fail-closed regardless of slice order. This locks the
// "stale-but-still-Active policy cannot shadow a divergence suspension" contract.
func TestTrinoInjector_DivergentSuspended_PrecedenceOverActive(t *testing.T) {
	tests := []struct {
		name     string
		policies []store.CompiledPolicy
	}{
		{
			name: "divergent_suspended first",
			policies: []store.CompiledPolicy{
				{
					EngineKind:   "trino",
					Status:       store.CompiledPolicyStatusDivergentSuspended,
					ArtifactBody: "deleted = false",
					ArtifactKind: store.KindRowFilter,
				},
				{
					EngineKind:   "trino",
					Status:       store.CompiledPolicyStatusActive,
					ArtifactBody: "tenant_id = 1",
					ArtifactKind: store.KindRowFilter,
				},
			},
		},
		{
			name: "active first",
			policies: []store.CompiledPolicy{
				{
					EngineKind:   "trino",
					Status:       store.CompiledPolicyStatusActive,
					ArtifactBody: "tenant_id = 1",
					ArtifactKind: store.KindRowFilter,
				},
				{
					EngineKind:   "trino",
					Status:       store.CompiledPolicyStatusDivergentSuspended,
					ArtifactBody: "deleted = false",
					ArtifactKind: store.KindRowFilter,
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fcs := &fakeCompiledStore{policies: tc.policies}
			inj, _ := newTestTrinoInjector(t, fcs)
			ctx := tenantCtx(t, trinoTestTenant)

			_, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM events",
				sqlproxy.TableRef{Namespace: trinoTestNS, Name: trinoTestTable},
				sqlproxy.Claims{})
			require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
			require.Equal(t, sqlproxy.CacheStatusError, cacheStatus)
			require.Contains(t, err.Error(), "policy_engine_divergent")
		})
	}
}

// TestTrinoInjector_DivergentSuspended_OtherEngineIgnored: a CompiledPolicy
// for EngineKind="spark" with Status=DivergentSuspended must NOT trigger the
// Trino fail-closed branch. The engine-kind filter runs first; the divergent
// check only fires after the row has been accepted as a "trino" row.
func TestTrinoInjector_DivergentSuspended_OtherEngineIgnored(t *testing.T) {
	fcs := &fakeCompiledStore{
		policies: []store.CompiledPolicy{
			{
				EngineKind:   "spark",
				Status:       store.CompiledPolicyStatusDivergentSuspended,
				ArtifactBody: "deleted = false",
				ArtifactKind: store.KindRowFilter,
			},
		},
	}
	inj, _ := newTestTrinoInjector(t, fcs)
	ctx := tenantCtx(t, trinoTestTenant)

	_, _, err := inj.InjectPolicy(ctx, "SELECT id FROM events",
		sqlproxy.TableRef{Namespace: trinoTestNS, Name: trinoTestTable},
		sqlproxy.Claims{})
	// Should fall through to "no active policy" — not the divergent branch.
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
	require.NotContains(t, err.Error(), "policy_engine_divergent")
}

// TestTrinoInjector_CacheHit: cache pre-populated → InjectPolicy returns
// spliced query without touching the store.
func TestTrinoInjector_CacheHit(t *testing.T) {
	fcs := &fakeCompiledStore{
		// If the cache is hit, LoadCompiledForTable must NOT be called.
		// Returning an error here ensures the test fails if the injector
		// accidentally calls the store on a cache hit.
		err: errors.New("store must not be called on cache hit"),
	}
	inj, cache := newTestTrinoInjector(t, fcs)

	cacheKey := sqlproxy.CacheKey{
		TenantID:  trinoTestTenant,
		Namespace: trinoTestNS,
		Table:     trinoTestTable,
		Engine:    "trino",
	}
	cache.Add(cacheKey, sqlproxy.ArtifactEntry{
		Body: []byte("deleted = false"),
		Kind: store.KindRowFilter,
	})

	ctx := tenantCtx(t, trinoTestTenant)
	rewritten, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM events",
		sqlproxy.TableRef{Namespace: trinoTestNS, Name: trinoTestTable},
		sqlproxy.Claims{})
	require.NoError(t, err)
	require.Equal(t, sqlproxy.CacheStatusHit, cacheStatus)
	require.Contains(t, normalize(rewritten), "WHERE (deleted = false)")
}

// TestTrinoInjector_NoPolicyForTable: store returns empty list →
// ErrPolicyEngineUnavailable with CacheStatusMiss.
func TestTrinoInjector_NoPolicyForTable(t *testing.T) {
	fcs := &fakeCompiledStore{policies: nil}
	inj, _ := newTestTrinoInjector(t, fcs)

	ctx := tenantCtx(t, trinoTestTenant)
	_, cacheStatus, err := inj.InjectPolicy(ctx, "SELECT id FROM events",
		sqlproxy.TableRef{Namespace: trinoTestNS, Name: trinoTestTable},
		sqlproxy.Claims{})
	require.Error(t, err)
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
	require.Equal(t, sqlproxy.CacheStatusMiss, cacheStatus)
}

// TestTrinoInjector_TenantMissing: ctx without tenant → ErrPolicyEngineUnavailable.
func TestTrinoInjector_TenantMissing(t *testing.T) {
	fcs := &fakeCompiledStore{}
	inj, _ := newTestTrinoInjector(t, fcs)

	_, _, err := inj.InjectPolicy(context.Background(), "SELECT id FROM events",
		sqlproxy.TableRef{Namespace: trinoTestNS, Name: trinoTestTable},
		sqlproxy.Claims{})
	require.ErrorIs(t, err, sqlproxy.ErrPolicyEngineUnavailable)
}
