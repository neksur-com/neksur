// SparkInjector — sqlproxy.Injector implementation for the Spark
// dialect. Wave 2 Plan 02-05 dispatch B. Mirrors TrinoInjector in
// structure; differs only in the engine label used for the CacheKey
// and the CompiledPolicy filter.

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

// SparkInjector serves the "spark" engine kind. See TrinoInjector for
// the per-field rationale — the two structs share an identical shape.
type SparkInjector struct {
	store *store.CompiledStore
	cache *lru.Cache[sqlproxy.CacheKey, []byte]
}

// NewSparkInjector constructs a SparkInjector bound to the given
// CompiledStore + shared LRU cache.
func NewSparkInjector(s *store.CompiledStore, cache *lru.Cache[sqlproxy.CacheKey, []byte]) *SparkInjector {
	return &SparkInjector{store: s, cache: cache}
}

// InjectPolicy fetches the active Spark CompiledPolicy artifact for
// (tenant=ctx, table) and structurally rewrites `query` — see
// dialect.go package doc for the Phase 2 rewrite-shape contract.
//
// Per Pitfall 11: no query body or artifact body is ever logged.
func (i *SparkInjector) InjectPolicy(ctx context.Context, query string, table sqlproxy.TableRef, principal sqlproxy.Claims) (string, string, error) {
	tid, ok := tenant.IDFromContext(ctx)
	if !ok {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: tenant missing: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	cacheKey := sqlproxy.CacheKey{
		TenantID:  tid.String(),
		Namespace: table.Namespace,
		Table:     table.Name,
		Engine:    "spark",
	}

	if body, hit := i.cache.Get(cacheKey); hit {
		rewritten, rerr := rewriteWithBody(query, body, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusHit, nil
	}

	compiled, err := i.store.LoadCompiledForTable(ctx, iceberg.TableRef{
		Namespace: []string{table.Namespace},
		Name:      table.Name,
	})
	if err != nil {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: load: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	for _, cp := range compiled {
		if cp.EngineKind == "spark" && cp.Status == store.CompiledPolicyStatusActive {
			body := []byte(cp.ArtifactBody)
			i.cache.Add(cacheKey, body)
			rewritten, rerr := rewriteWithBody(query, body, principal)
			if rerr != nil {
				return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: %w", rerr)
			}
			return rewritten, sqlproxy.CacheStatusMiss, nil
		}
	}

	return "", sqlproxy.CacheStatusMiss, fmt.Errorf("dialect/spark: no active policy: %w", sqlproxy.ErrPolicyEngineUnavailable)
}
