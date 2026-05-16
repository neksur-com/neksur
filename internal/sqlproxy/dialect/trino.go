// TrinoInjector — sqlproxy.Injector implementation for the Trino
// dialect. Wave 2 Plan 02-05 dispatch B + Plan 02-12 CR-A3 splicer.

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

// TrinoInjector serves the "trino" engine kind. Thread-safe: the
// CompiledStore and LRU cache are both safe for concurrent use, and
// InjectPolicy holds no per-request mutable state.
type TrinoInjector struct {
	store *store.CompiledStore
	cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]
}

// NewTrinoInjector constructs a TrinoInjector bound to the given
// CompiledStore + LRU cache (shared across all dialect implementations
// — the CacheKey carries the Engine field so per-dialect entries never
// collide).
func NewTrinoInjector(s *store.CompiledStore, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) *TrinoInjector {
	return &TrinoInjector{store: s, cache: cache}
}

// InjectPolicy fetches the active Trino CompiledPolicy artifact for
// (tenant=ctx, table) and splices it into `query` via
// dialect.SpliceArtifact (Plan 02-12 — replaces the iter-1 no-op
// comment-appending rewrite). The artifact kind discriminator drives
// row-filter WHERE-conjunction vs column-mask projection rewriting.
//
// Per Pitfall 11: this method never logs the query body or the
// artifact body — error returns wrap sqlproxy sentinels only.
func (i *TrinoInjector) InjectPolicy(ctx context.Context, query string, table sqlproxy.TableRef, principal sqlproxy.Claims) (string, string, error) {
	tid, ok := tenant.IDFromContext(ctx)
	if !ok {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: tenant missing: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	cacheKey := sqlproxy.CacheKey{
		TenantID:  tid.String(),
		Namespace: table.Namespace,
		Table:     table.Name,
		Engine:    "trino",
	}

	if entry, hit := i.cache.Get(cacheKey); hit {
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusHit, nil
	}

	compiled, err := i.store.LoadCompiledForTable(ctx, iceberg.TableRef{
		Namespace: []string{table.Namespace},
		Name:      table.Name,
	})
	if err != nil {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: load: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	for _, cp := range compiled {
		if cp.EngineKind != "trino" || cp.Status != store.CompiledPolicyStatusActive {
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
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusMiss, nil
	}

	return "", sqlproxy.CacheStatusMiss, fmt.Errorf("dialect/trino: no active policy: %w", sqlproxy.ErrPolicyEngineUnavailable)
}
