// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// Cache wraps *lru.Cache[CacheKey, ArtifactEntry] with a tenant-scoped
// Invalidate method for the schema-cache broadcaster (Plan 03-07).
//
// The underlying LRU is the existing per-process ArtifactEntry cache shared
// across all dialect Injector implementations. Wrapping it here (rather than
// modifying InjectorDeps.Cache in-place) keeps the Cache type testable without
// an Injector construction dependency.
//
// Thread-safety: lru.Cache[K, V] is internally thread-safe (mutex-protected
// hot path). Cache adds no additional locking because Invalidate calls only
// Keys() + Remove() — both are already mutex-guarded in the underlying LRU.

package sqlproxy

import (
	lru "github.com/hashicorp/golang-lru/v2"
)

// Cache is a thin wrapper around the shared LRU providing:
//   - Raw()        — access to the underlying *lru.Cache (for dialect code that
//     constructs Injectors via InjectorDeps.Cache)
//   - Invalidate() — tenant-scoped cache eviction for the schema-cache broadcaster
//
// Construct once at neksur-server startup via NewCache, then pass Cache.Raw()
// to InjectorDeps.Cache (which existing dialect code expects as *lru.Cache[...]).
// Inject *Cache into the schemacache.Invalidator for cross-engine flushes.
type Cache struct {
	inner *lru.Cache[CacheKey, ArtifactEntry]
}

// NewCache wraps an existing lru.Cache in a Cache. The raw argument must not
// be nil; passing nil causes a panic at first method call.
func NewCache(raw *lru.Cache[CacheKey, ArtifactEntry]) *Cache {
	return &Cache{inner: raw}
}

// Raw returns the underlying *lru.Cache so that dialect Injectors can be
// wired via InjectorDeps.Cache without changing their constructor signatures.
func (c *Cache) Raw() *lru.Cache[CacheKey, ArtifactEntry] {
	return c.inner
}

// Invalidate removes ALL cache entries whose CacheKey matches BOTH tenantID
// and tableID, regardless of Namespace or Engine. This is the correct eviction
// scope for a schema_changed event: a schema change on (tenant, table) invalidates
// all per-engine compiled artifacts for that table across every namespace and
// every engine kind.
//
// Implementation note: Keys() iterates the full LRU (O(N)), which is acceptable
// because:
//   - The LRU is bounded to a small capacity (typically 512–2048 entries).
//   - Schema_changed events are low-frequency (operator-paced DDL, not every write).
//   - Remove() is O(log N) inside the LRU.
//
// If the LRU ever grows to tens of thousands of entries, a secondary index
// (map[tenantID+tableID][]CacheKey) can be added alongside the LRU —
// tracked as a deferred optimization.
func (c *Cache) Invalidate(tenantID, tableID string) {
	// Snapshot keys first to avoid modifying the collection during iteration.
	keys := c.inner.Keys()
	for _, k := range keys {
		if k.TenantID == tenantID && k.Table == tableID {
			c.inner.Remove(k)
		}
	}
}
