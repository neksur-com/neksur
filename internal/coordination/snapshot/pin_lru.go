// PinLRU — LRU cache for ActivePinsForTable hot reads.
//
// Wraps github.com/hashicorp/golang-lru/v2 (already in go.mod) with
// a three-field composite key:
//
//	{TenantID, Namespace, Table} → []SnapshotPin
//
// Size: 4096 entries — mirrors the Phase 1 CEL cache sizing
// (see internal/policy/cel/ LRU constructor).
//
// # TTL semantics
//
// There is intentionally no TTL-on-expiry in the cache. Each SnapshotPin
// carries its own expiry_utc field which ActivePinsForTable checks at
// consultation time. The cache is invalidated on UpsertSnapshotPin
// (which writes a new pin or updates an existing one).
//
// # Sanitization notice (upstream responsibility)
//
// PinCacheKey fields are derived from caller-supplied values that have
// already passed through graph.MustSanitizeCypherLiteral before reaching
// the cache. The cache does NOT perform additional sanitization — it
// relies on the PinStore to have already validated the inputs. The cache
// key is never written to the AGE graph directly.
//
// # Upstream UUID-validation requirement
//
// The pin_name field of SnapshotPin (surfaced in PinCacheKey.Table
// indirectly) MUST be UUID-validated by the gateway endpoint that calls
// UpsertSnapshotPin. This package does not enforce UUID shape — the
// MustSanitizeCypherLiteral call in UpsertSnapshotPin is the
// defence-in-depth second check, not the primary validator.

package snapshot

import (
	lru "github.com/hashicorp/golang-lru/v2"
)

// PinCacheKey is the composite cache key for the pin LRU.
// Exported so unit tests can construct keys without going through
// PinStore (and to avoid reflection-based struct comparison issues
// with internal LRU key equality).
type PinCacheKey struct {
	TenantID  string
	Namespace string
	Table     string
}

// PinLRU is a thread-safe LRU cache mapping (TenantID, Namespace, Table)
// to the slice of active SnapshotPins for that table. Thread-safe.
//
// Construct via NewPinLRU; share a single instance across all PinStore
// callers. The default production size is 4096.
type PinLRU struct {
	c *lru.Cache[PinCacheKey, []SnapshotPin]
}

// NewPinLRU constructs a PinLRU with the given maximum entry count.
// Returns an error if size <= 0 (lru.New constraint).
func NewPinLRU(size int) (*PinLRU, error) {
	c, err := lru.New[PinCacheKey, []SnapshotPin](size)
	if err != nil {
		return nil, err
	}
	return &PinLRU{c: c}, nil
}

// Get returns the cached slice for the given key, or (nil, false) on a
// miss. The returned slice MUST NOT be mutated by the caller.
func (p *PinLRU) Get(key PinCacheKey) ([]SnapshotPin, bool) {
	return p.c.Get(key)
}

// Add adds or replaces the entry for the given key. Returns true if the
// Add caused an eviction (cache was at capacity).
func (p *PinLRU) Add(key PinCacheKey, pins []SnapshotPin) bool {
	return p.c.Add(key, pins)
}

// Invalidate removes the entry for the given key from the cache.
// No-op if the key is not present.
func (p *PinLRU) Invalidate(key PinCacheKey) {
	p.c.Remove(key)
}
