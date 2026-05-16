// cache.go — TTL-based LRU cache for STS credentials (Anti-Pattern 3 mitigation).
//
// Anti-Pattern 3 (RESEARCH line 703): caching STS credentials beyond their
// TTL is dangerous. The refresh strategy here is:
//   - Proactive: credentials are considered stale when expiresAt - now < ttl/2
//     (refresh-at-half-TTL ensures the token is always valid when Spark uses it).
//   - Reactive: callers can invalidate a specific entry on receiving a 401 from
//     S3 (the Invalidate method — Phase 2 simplification: the handler calls this
//     on upstream 4xx responses).
//
// Cache key: (TenantID, Namespace, TableName, Region) — includes TenantID to
// enforce cross-tenant isolation by construction (T-2-sts-token-cache-cross-tenant).
// The Region field is included because the same table in different regions gets
// different STS tokens (P4 data residency enforcement).
//
// Default capacity: 4096 entries (matches Phase 1 CEL compile cache and
// Plan 02-05 sqlproxy cache sizing per RESEARCH §Standard Stack line 138).
package credvend

import (
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// defaultCacheSize is the default LRU capacity for the STS credential cache.
// 4096 matches the Phase 1 CEL compile cache default and the Plan 02-05
// sqlproxy LRU size recommendation.
const defaultCacheSize = 4096

// cacheKey is the composite key for the STS credential cache. All four fields
// are comparable (string) so lru.Cache's generic K constraint is satisfied.
//
// TenantID — isolates tenants (cross-tenant reuse impossible by construction).
// Namespace — first namespace segment of the Iceberg table.
// TableName — table name.
// Region    — AWS region (different STS tokens per region for P4 residency).
type cacheKey struct {
	TenantID  string
	Namespace string
	TableName string
	Region    string
}

// cachedCreds wraps STSCredentials with a cache expiration timestamp.
// The cache uses refresh-at-TTL/2 semantics: Get returns nil when
// expiresAt - now < (originalTTL / 2), forcing a refresh before expiry.
type cachedCreds struct {
	creds     *iceberg.STSCredentials
	expiresAt time.Time
	issuedAt  time.Time // for TTL/2 calculation
}

// Cache is a TTL-based LRU cache for STS credentials. Thread-safe via
// the underlying lru.Cache (concurrent-safe per golang-lru/v2 docs).
type Cache struct {
	c *lru.Cache[cacheKey, *cachedCreds]
}

// NewCache constructs a Cache with the given LRU capacity. Use 0 to
// select the default capacity (4096). Returns an error if lru.New fails
// (only possible when size < 0 — golang-lru/v2 invariant).
func NewCache(size int) (*Cache, error) {
	if size <= 0 {
		size = defaultCacheSize
	}
	c, err := lru.New[cacheKey, *cachedCreds](size)
	if err != nil {
		return nil, fmt.Errorf("credvend: new cache: %w", err)
	}
	return &Cache{c: c}, nil
}

// Get returns the cached STSCredentials for (tenantID, namespace, tableName,
// region) if the entry exists AND has not reached its TTL/2 refresh point.
// Returns nil when:
//   - The entry does not exist (cache miss).
//   - The entry has passed TTL/2 (proactive refresh — Anti-Pattern 3 mitigation).
//   - The entry's Expiration time has already passed (stale token).
//
// A nil return means the caller MUST issue a fresh IssueScopedSTSCredentials
// call and then call Add to populate the cache.
func (c *Cache) Get(tenantID, namespace, tableName, region string) *iceberg.STSCredentials {
	key := cacheKey{TenantID: tenantID, Namespace: namespace, TableName: tableName, Region: region}
	entry, ok := c.c.Get(key)
	if !ok {
		return nil
	}
	now := time.Now()
	// Stale check: token is actually expired.
	if now.After(entry.expiresAt) {
		c.c.Remove(key)
		return nil
	}
	// TTL/2 refresh check: if we are past the midpoint of the token lifetime,
	// treat as a cache miss so the caller refreshes before expiry (Anti-Pattern 3).
	half := entry.issuedAt.Add(entry.expiresAt.Sub(entry.issuedAt) / 2)
	if now.After(half) {
		// Don't evict — let the stale entry serve any in-flight concurrent
		// callers while the refresh is pending. The caller will Add the fresh
		// entry which overwrites this one.
		return nil
	}
	return entry.creds
}

// Add inserts or updates the cached STSCredentials for the given key.
// The expiration time is taken from creds.Expiration (set by parseVendedCreds
// from the Polaris response).
func (c *Cache) Add(tenantID, namespace, tableName, region string, creds *iceberg.STSCredentials) {
	if creds == nil {
		return
	}
	key := cacheKey{TenantID: tenantID, Namespace: namespace, TableName: tableName, Region: region}
	c.c.Add(key, &cachedCreds{
		creds:     creds,
		expiresAt: creds.Expiration,
		issuedAt:  time.Now(),
	})
}

// Invalidate removes the cached entry for the given key. Called by the
// handler on 401/403 responses from downstream S3 calls to trigger
// on-demand refresh (reactive path — Anti-Pattern 3).
func (c *Cache) Invalidate(tenantID, namespace, tableName, region string) {
	key := cacheKey{TenantID: tenantID, Namespace: namespace, TableName: tableName, Region: region}
	c.c.Remove(key)
}

// Len returns the number of entries currently in the cache (for metrics).
func (c *Cache) Len() int {
	return c.c.Len()
}
