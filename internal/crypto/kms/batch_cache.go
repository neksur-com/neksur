// batch_cache.go — per-write-batch DEK cache (Pitfall 10 mitigation).
//
// Pitfall 10 (RESEARCH): calling GenerateDataKey once per row is
// catastrophically expensive at write-batch scale — KMS is rated at
// 400 req/s per region per account. A 1M-row batch at 1 call/row =
// 2500 seconds of KMS traffic (orders of magnitude too slow + will
// exceed rate limits). Solution: derive one DEK per (tenant, column,
// batchID) and cache the plaintext for the duration of the batch.
//
// This mirrors the JVM-side ConcurrentHashMap DEK cache in
// neksur-spark-policy's KmsKeyProvider (Plan 02-06) — the Go-side uses
// golang-lru/v2 (already pinned at v2.0.7) to get bounded LRU eviction.
//
// Cache key: (TenantID, ColumnName, BatchID)
//   - TenantID isolates tenants (prevents cross-tenant DEK sharing)
//   - ColumnName differentiates per-column DEKs within a batch
//   - BatchID scopes the cache to a single write batch (DEK is
//     discarded / evicted after the batch completes)
//
// TTL: batch lifetime (default 10 minutes). After TTL expiry the
// cached entry is evicted on next access and a fresh GenerateDataKey
// is issued for the subsequent batch. callers SHOULD use short TTLs
// consistent with their batch processing latency.
//
// Size: default 4096 entries (matches Phase 1 CEL compile cache and
// Plan 02-07 credvend cache sizing; adjustable via NewBatchCache).
package kms

import (
	"fmt"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// batchCacheKey is the composite key for the per-batch DEK cache.
// TenantID prevents cross-tenant DEK sharing; ColumnName differentiates
// per-column DEKs; BatchID scopes to a single write batch.
type batchCacheKey struct {
	TenantID   string
	ColumnName string
	BatchID    string
}

// cachedDEK holds the plaintext DEK and its cache expiration time.
// expiresAt is set to now + TTL on insertion; the cache returns nil
// on a hit if the entry has expired (refresh-on-TTL semantics).
type cachedDEK struct {
	plaintext []byte
	expiresAt time.Time
}

// BatchCache is a bounded LRU cache for plaintext data-encryption keys
// keyed on (TenantID, ColumnName, BatchID). It wraps golang-lru/v2 and
// adds TTL-based expiry.
//
// Thread-safety: the underlying lru.Cache is safe for concurrent use;
// the mu mutex serialises the two-phase read (Get) + write (Add) on
// cache miss to prevent redundant GenerateDataKey calls under contention.
type BatchCache struct {
	cache *lru.Cache[batchCacheKey, *cachedDEK]
	ttl   time.Duration
	mu    sync.Mutex
}

// NewBatchCache constructs a BatchCache with the given LRU size and TTL.
// The default sizing recommendation is 4096 entries and 10 minutes TTL.
// Returns an error only if size <= 0 (golang-lru/v2 invariant).
func NewBatchCache(size int, ttl time.Duration) (*BatchCache, error) {
	c, err := lru.New[batchCacheKey, *cachedDEK](size)
	if err != nil {
		return nil, fmt.Errorf("kms: batch cache: %w", err)
	}
	return &BatchCache{cache: c, ttl: ttl}, nil
}

// Get returns the cached plaintext DEK for the given key, or nil if the
// key is absent or the cached entry has expired. Callers SHOULD check
// for nil and call Set after generating a fresh DEK.
func (b *BatchCache) Get(tenantID, columnName, batchID string) []byte {
	key := batchCacheKey{TenantID: tenantID, ColumnName: columnName, BatchID: batchID}
	entry, ok := b.cache.Get(key)
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		// TTL expired — evict proactively so the next caller triggers
		// a fresh GenerateDataKey rather than spinning on expired entries.
		b.cache.Remove(key)
		return nil
	}
	// Return a copy to prevent the caller from mutating the cached slice.
	out := make([]byte, len(entry.plaintext))
	copy(out, entry.plaintext)
	return out
}

// Set stores the plaintext DEK in the cache for the given key.
// The entry expires after the configured TTL. Set is a no-op if
// plaintext is empty (defensive).
func (b *BatchCache) Set(tenantID, columnName, batchID string, plaintext []byte) {
	if len(plaintext) == 0 {
		return
	}
	key := batchCacheKey{TenantID: tenantID, ColumnName: columnName, BatchID: batchID}
	// Store a copy to prevent the caller from mutating the cached data.
	stored := make([]byte, len(plaintext))
	copy(stored, plaintext)
	b.cache.Add(key, &cachedDEK{
		plaintext: stored,
		expiresAt: time.Now().Add(b.ttl),
	})
}

// GetOrGenerate is a convenience helper that combines Get + a
// user-supplied generate function + Set. It is the canonical call site
// for Pitfall 10 mitigation: the caller passes the GenerateDataKey
// function; GetOrGenerate guarantees exactly one call per cache miss.
//
// The mu lock around the full read-generate-write sequence prevents
// concurrent goroutines in the same batch from issuing redundant
// GenerateDataKey calls for the same (tenant, column, batchID) triple.
//
// generateFn receives the tenantID, columnName, and batchID and returns
// (plaintext, error). On error, nothing is cached and the error is
// returned to the caller.
func (b *BatchCache) GetOrGenerate(tenantID, columnName, batchID string, generateFn func(tenantID, columnName string) ([]byte, error)) ([]byte, error) {
	// Fast path — check without holding the lock.
	if dek := b.Get(tenantID, columnName, batchID); dek != nil {
		return dek, nil
	}

	// Slow path — acquire lock, re-check, generate if still absent.
	b.mu.Lock()
	defer b.mu.Unlock()

	// Re-check under the lock (double-checked locking).
	if dek := b.Get(tenantID, columnName, batchID); dek != nil {
		return dek, nil
	}

	plaintext, err := generateFn(tenantID, columnName)
	if err != nil {
		return nil, fmt.Errorf("kms: batch cache: generate: %w", err)
	}

	b.Set(tenantID, columnName, batchID, plaintext)

	// Return a copy from Get to ensure consistent copy semantics.
	return b.Get(tenantID, columnName, batchID), nil
}

// Len returns the current number of entries in the cache (for metrics /
// debugging).
func (b *BatchCache) Len() int {
	return b.cache.Len()
}
