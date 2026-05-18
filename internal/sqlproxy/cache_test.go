// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// Tests for Cache.Invalidate — the cross-engine LRU invalidation method
// added in Plan 03-07 for the schema-cache broadcaster.

package sqlproxy_test

import (
	"testing"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// TestCacheInvalidate_TenantScopedEviction verifies that Invalidate(tenantID, tableID)
// removes only entries matching BOTH tenantID and tableID, leaving all other entries
// (different table, different tenant) intact.
func TestCacheInvalidate_TenantScopedEviction(t *testing.T) {
	raw, err := lru.New[sqlproxy.CacheKey, sqlproxy.ArtifactEntry](128)
	if err != nil {
		t.Fatalf("lru.New: %v", err)
	}
	cache := sqlproxy.NewCache(raw)

	// Populate:
	//   Tenant A / table1 / trino  → should be evicted by Invalidate(A, table1)
	//   Tenant A / table1 / spark  → should be evicted by Invalidate(A, table1)
	//   Tenant A / table2 / trino  → should survive
	//   Tenant B / table1 / trino  → should survive
	add := func(tenant, ns, table, engine string) {
		cache.Raw().Add(sqlproxy.CacheKey{
			TenantID:  tenant,
			Namespace: ns,
			Table:     table,
			Engine:    engine,
		}, sqlproxy.ArtifactEntry{Body: []byte("artifact"), Kind: "row_filter"})
	}
	add("tenant-A", "public", "table1", "trino")
	add("tenant-A", "public", "table1", "spark")
	add("tenant-A", "public", "table2", "trino")
	add("tenant-B", "public", "table1", "trino")

	if cache.Raw().Len() != 4 {
		t.Fatalf("pre-invalidate cache size = %d; want 4", cache.Raw().Len())
	}

	// Invalidate tenant-A / table1 (regardless of namespace or engine).
	cache.Invalidate("tenant-A", "table1")

	if cache.Raw().Len() != 2 {
		t.Errorf("post-invalidate cache size = %d; want 2", cache.Raw().Len())
	}

	// Verify surviving entries.
	checkPresent := func(tenant, ns, table, engine string) {
		t.Helper()
		k := sqlproxy.CacheKey{TenantID: tenant, Namespace: ns, Table: table, Engine: engine}
		if _, ok := cache.Raw().Get(k); !ok {
			t.Errorf("expected entry %+v to survive Invalidate", k)
		}
	}
	checkAbsent := func(tenant, ns, table, engine string) {
		t.Helper()
		k := sqlproxy.CacheKey{TenantID: tenant, Namespace: ns, Table: table, Engine: engine}
		if _, ok := cache.Raw().Get(k); ok {
			t.Errorf("expected entry %+v to be evicted by Invalidate", k)
		}
	}

	checkAbsent("tenant-A", "public", "table1", "trino")
	checkAbsent("tenant-A", "public", "table1", "spark")
	checkPresent("tenant-A", "public", "table2", "trino")
	checkPresent("tenant-B", "public", "table1", "trino")
}

// TestCacheInvalidate_NoMatchingEntry verifies Invalidate is a no-op when no
// entry matches the (tenantID, tableID) pair — it must not panic.
func TestCacheInvalidate_NoMatchingEntry(t *testing.T) {
	raw, err := lru.New[sqlproxy.CacheKey, sqlproxy.ArtifactEntry](128)
	if err != nil {
		t.Fatalf("lru.New: %v", err)
	}
	cache := sqlproxy.NewCache(raw)
	raw.Add(sqlproxy.CacheKey{TenantID: "tenant-X", Table: "other"}, sqlproxy.ArtifactEntry{})

	// Must not panic or remove the non-matching entry.
	cache.Invalidate("tenant-A", "table1")

	if raw.Len() != 1 {
		t.Errorf("cache size after no-match Invalidate = %d; want 1", raw.Len())
	}
}
