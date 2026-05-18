// SparkInjector — sqlproxy.Injector implementation for the Spark
// dialect. Wave 2 Plan 02-05 dispatch B + Plan 02-12 CR-A3 splicer.
// Mirrors TrinoInjector in structure; differs only in the engine
// label used for the CacheKey and the CompiledPolicy filter. The
// splice logic in splice.go is dialect-agnostic for SELECT shape
// and dialect-specific only in the function-name vocabulary the
// per-dialect compiler emits inside artifact bodies (already
// handled by the Phase 2 internal/policy/compiler/dialect/
// subdirectory).
//
// Plan 03-16: explicit fail-closed branch for CompiledPolicyStatusDivergentSuspended
// (mirrors dremio.go:170-172 + trino.go post-Plan-03-16; closes 03-VERIFICATION CR-02).

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
	loader CompiledLoader
	cache  *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]
}

// NewSparkInjector constructs a SparkInjector bound to the given
// CompiledStore + shared LRU cache.
func NewSparkInjector(s *store.CompiledStore, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) *SparkInjector {
	return &SparkInjector{loader: s, cache: cache}
}

// NewSparkInjectorWithLoader constructs a SparkInjector with a custom
// CompiledLoader. This constructor exists for unit testing — the
// production wiring layer calls NewSparkInjector with a *store.CompiledStore.
func NewSparkInjectorWithLoader(loader CompiledLoader, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) *SparkInjector {
	return &SparkInjector{loader: loader, cache: cache}
}

// InjectPolicy fetches the active Spark CompiledPolicy artifact for
// (tenant=ctx, table) and splices it into `query` via
// dialect.SpliceArtifact (Plan 02-12 — replaces the iter-1 no-op
// comment-appending rewrite).
//
// Per Pitfall 11: no query body or artifact body is ever logged.
// divergent_suspended status is treated as fail-closed (503) per D-3.05;
// Plan 03-11 owns the reason='policy_engine_divergent' metric label.
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

	if entry, hit := i.cache.Get(cacheKey); hit {
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusHit, nil
	}

	compiled, err := i.loader.LoadCompiledForTable(ctx, iceberg.TableRef{
		Namespace: []string{table.Namespace},
		Name:      table.Name,
	})
	if err != nil {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: load: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	// Pre-pass: check for divergent_suspended rows before processing any
	// active rows. A divergent_suspended row for "spark" wins over any
	// active row regardless of slice order (T-3-16-stale-active-shadow).
	// divergent_suspended is fail-closed (D-3.05 — Plan 03-11 verifier
	// auto-suspend takes effect here). Plan 03-11 maps
	// ErrPolicyEngineUnavailable to the
	// sql_proxy_inject_failures_total{reason='policy_engine_divergent'} label.
	for _, cp := range compiled {
		if cp.EngineKind != "spark" {
			continue
		}
		if cp.Status == store.CompiledPolicyStatusDivergentSuspended {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: policy_engine_divergent: %w", sqlproxy.ErrPolicyEngineUnavailable)
		}
	}
	for _, cp := range compiled {
		if cp.EngineKind != "spark" {
			continue
		}
		if cp.Status != store.CompiledPolicyStatusActive {
			continue
		}
		if cp.ArtifactKind == store.KindPredicate {
			continue
		}
		entry := sqlproxy.ArtifactEntry{Body: []byte(cp.ArtifactBody), Kind: cp.ArtifactKind}
		i.cache.Add(cacheKey, entry)
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/spark: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusMiss, nil
	}

	return "", sqlproxy.CacheStatusMiss, fmt.Errorf("dialect/spark: no active policy: %w", sqlproxy.ErrPolicyEngineUnavailable)
}
