// DremioInjector — sqlproxy.Injector implementation for the Dremio
// dialect. Phase 3 D-3.02 (Plan 03-05) makes it live by cloning the
// TrinoInjector pattern from trino.go (per 03-PATTERNS §12).
//
// Phase 2 history:
//   CR-09: Phase 2 stub returned sqlproxy.ErrPolicyEngineUnavailable
//   (fail-closed, 503 + sql_proxy_inject_failures_total) to avoid the
//   silent 501 / no-alert posture of an ErrEngineNotSupported return.
//   WR-A3: counter family is sql_proxy_inject_failures_total (NOT
//   commit_rejected_total — that is L1-catalog-gateway-only).
//
// Phase 3 status: dremio LIVE (Phase 3 D-3.02 + 03-05-PLAN).
//   The fail-closed stub is replaced with a real splicer that follows
//   the same shape as TrinoInjector (trino.go lines 42-92):
//   tenant.IDFromContext → cache lookup → SpliceArtifact dialect
//   dispatch → cache populate on miss.
//
// Splice.go compatibility:
//   Dremio uses ANSI SQL with double-quote identifier quoting — the
//   same SELECT/WHERE/projection grammar as Trino. SpliceArtifact in
//   splice.go is dialect-agnostic for the single-table SELECT shape;
//   no Dremio-specific arm is needed in splice.go.
//
// divergent_suspended handling:
//   Per D-3.05 and the threat model (T-3-dremio-divergent-bypass),
//   a CompiledPolicy with status=divergent_suspended is treated
//   identically to probe_failed: fail-closed 503 +
//   sql_proxy_inject_failures_total{reason='policy_engine_unavailable'}.
//   Plan 03-11 will add the distinct reason label; for now the same
//   ErrPolicyEngineUnavailable sentinel is returned.
//
// Per Pitfall 11: this file never logs the query body or the artifact
// body — error returns wrap sqlproxy sentinels only.

package dialect

import (
	"context"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	"github.com/neksur-com/neksur/internal/tenant"
)

// CompiledLoader is the narrow interface DremioInjector needs from the
// CompiledStore. *store.CompiledStore satisfies this interface; tests
// inject a fake implementation via NewDremioInjectorWithLoader.
type CompiledLoader interface {
	LoadCompiledForTable(ctx context.Context, ref iceberg.TableRef) ([]store.CompiledPolicy, error)
}

// DremioInjector serves the "dremio" engine kind. Thread-safe: the
// compiledForTableLoader and LRU cache are both safe for concurrent
// use, and InjectPolicy holds no per-request mutable state.
//
// Phase 3 D-3.02: replaces the Phase 2 fail-closed stub with a live
// splicer following the same shape as TrinoInjector (trino.go).
type DremioInjector struct {
	loader CompiledLoader
	cache  *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]
}

// NewDremioInjector constructs a live DremioInjector bound to the given
// CompiledStore + LRU cache (shared across all dialect implementations
// — the CacheKey carries the Engine field so per-dialect entries never
// collide).
func NewDremioInjector(s *store.CompiledStore, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) *DremioInjector {
	return &DremioInjector{loader: s, cache: cache}
}

// NewDremioInjectorWithLoader constructs a DremioInjector with a custom
// CompiledLoader. This constructor exists for unit testing — the
// production wiring layer calls NewDremioInjector with a *store.CompiledStore.
func NewDremioInjectorWithLoader(loader CompiledLoader, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) *DremioInjector {
	return &DremioInjector{loader: loader, cache: cache}
}

// InjectPolicy fetches the active Dremio CompiledPolicy artifact for
// (tenant=ctx, table) and splices it into `query` via
// dialect.SpliceArtifact (Plan 02-12 — replaces the Phase 2 fail-closed
// stub). The artifact kind discriminator drives row-filter
// WHERE-conjunction vs column-mask projection rewriting.
//
// divergent_suspended status is treated as fail-closed (503) per
// D-3.05 / T-3-dremio-divergent-bypass. Plan 03-11 adds the distinct
// reason label; this plan returns ErrPolicyEngineUnavailable.
//
// Per Pitfall 11: this method never logs the query body or the
// artifact body — error returns wrap sqlproxy sentinels only.
func (i *DremioInjector) InjectPolicy(ctx context.Context, query string, table sqlproxy.TableRef, principal sqlproxy.Claims) (string, string, error) {
	tid, ok := tenant.IDFromContext(ctx)
	if !ok {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: tenant missing: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	cacheKey := sqlproxy.CacheKey{
		TenantID:  tid.String(),
		Namespace: table.Namespace,
		Table:     table.Name,
		Engine:    "dremio",
	}

	if entry, hit := i.cache.Get(cacheKey); hit {
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusHit, nil
	}

	compiled, err := i.loader.LoadCompiledForTable(ctx, iceberg.TableRef{
		Namespace: []string{table.Namespace},
		Name:      table.Name,
	})
	if err != nil {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: load: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	for _, cp := range compiled {
		if cp.EngineKind != "dremio" {
			continue
		}
		// divergent_suspended is treated identically to probe_failed:
		// fail-closed 503. Plan 03-11 adds the distinct reason label.
		if cp.Status == store.CompiledPolicyStatusDivergentSuspended {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: policy_engine_divergent: %w", sqlproxy.ErrPolicyEngineUnavailable)
		}
		if cp.Status != store.CompiledPolicyStatusActive {
			continue
		}
		// Predicate-kind artifacts are gateway-handled (Layer 1 commit
		// validation) — they must NOT reach the sqlproxy path. Skip
		// silently so a tenant with mixed kinds gets enforcement on the
		// splice-eligible artifacts and the gateway handles the rest.
		if cp.ArtifactKind == store.KindPredicate {
			continue
		}
		entry := sqlproxy.ArtifactEntry{Body: []byte(cp.ArtifactBody), Kind: cp.ArtifactKind}
		i.cache.Add(cacheKey, entry)
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusMiss, nil
	}

	return "", sqlproxy.CacheStatusMiss, fmt.Errorf("dialect/dremio: no active policy: %w", sqlproxy.ErrPolicyEngineUnavailable)
}
