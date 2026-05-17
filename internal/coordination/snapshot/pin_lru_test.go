// Tests for PinLRU — LRU cache wrapping golang-lru/v2.
//
// TDD RED: written before the implementation; intended to fail at
// compilation until pin_lru.go is written.

package snapshot_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/coordination/snapshot"
)

// TestPinLRU_EvictionAtMax: inserting more entries than the cache
// capacity evicts the least-recently-used entry.
func TestPinLRU_EvictionAtMax(t *testing.T) {
	t.Parallel()
	// Use a small size (3) to test eviction without inserting 4097 entries.
	cache, err := snapshot.NewPinLRU(3)
	if err != nil {
		t.Fatalf("NewPinLRU: %v", err)
	}

	pin := []snapshot.SnapshotPin{{
		Name:         "pin-evict",
		AtSnapshotID: "snap-x",
		ExpiryUTC:    time.Now().UTC().Add(1 * time.Hour),
	}}

	// Fill cache to capacity: tbl-0 is LRU after adding tbl-1, tbl-2.
	for i := 0; i < 3; i++ {
		key := snapshot.PinCacheKey{TenantID: "t1", Namespace: "ns", Table: fmt.Sprintf("tbl-%d", i)}
		cache.Add(key, pin)
	}

	// Add one more — evicts the LRU entry (tbl-0, the first inserted).
	// We do NOT call Get(tbl-0) here to avoid promoting it to MRU.
	key3 := snapshot.PinCacheKey{TenantID: "t1", Namespace: "ns", Table: "tbl-3"}
	evicted := cache.Add(key3, pin) // returns true when an entry was evicted

	// Verify that the cache reported an eviction.
	if !evicted {
		t.Error("expected Add to report an eviction when cache is full")
	}

	// tbl-3 (just added) must be present.
	if _, ok := cache.Get(key3); !ok {
		t.Error("expected tbl-3 to be present after add")
	}

	// tbl-2 and tbl-1 (recently accessed via Add ordering) should still
	// be present; exactly ONE of the three original entries was evicted.
	key0 := snapshot.PinCacheKey{TenantID: "t1", Namespace: "ns", Table: "tbl-0"}
	key1 := snapshot.PinCacheKey{TenantID: "t1", Namespace: "ns", Table: "tbl-1"}
	key2 := snapshot.PinCacheKey{TenantID: "t1", Namespace: "ns", Table: "tbl-2"}
	_, ok0 := cache.Get(key0)
	_, ok1 := cache.Get(key1)
	_, ok2 := cache.Get(key2)

	// Exactly one of the original three should be missing (evicted).
	present := 0
	for _, ok := range []bool{ok0, ok1, ok2} {
		if ok {
			present++
		}
	}
	if present != 2 {
		t.Errorf("expected exactly 2 of 3 original entries to remain after eviction; got %d", present)
	}

	// tbl-0 is the LRU entry (inserted first, never accessed after) —
	// it should be the one evicted.
	if ok0 {
		t.Error("expected tbl-0 (LRU) to be evicted, but it is still present")
	}
}

// TestPinLRU_TenantKeyIsolation: same table name in different tenants
// produces different LRU entries.
func TestPinLRU_TenantKeyIsolation(t *testing.T) {
	t.Parallel()
	cache, err := snapshot.NewPinLRU(64)
	if err != nil {
		t.Fatalf("NewPinLRU: %v", err)
	}

	pinA := []snapshot.SnapshotPin{{
		Name:         "pin-a",
		AtSnapshotID: "snap-a",
		ExpiryUTC:    time.Now().UTC().Add(1 * time.Hour),
	}}
	pinB := []snapshot.SnapshotPin{{
		Name:         "pin-b",
		AtSnapshotID: "snap-b",
		ExpiryUTC:    time.Now().UTC().Add(1 * time.Hour),
	}}

	keyA := snapshot.PinCacheKey{TenantID: "tenant-a", Namespace: "ns", Table: "shared-table"}
	keyB := snapshot.PinCacheKey{TenantID: "tenant-b", Namespace: "ns", Table: "shared-table"}

	cache.Add(keyA, pinA)
	cache.Add(keyB, pinB)

	gotA, okA := cache.Get(keyA)
	gotB, okB := cache.Get(keyB)

	if !okA || !okB {
		t.Fatalf("expected both keys to be present; okA=%v okB=%v", okA, okB)
	}
	if gotA[0].Name == gotB[0].Name {
		t.Errorf("tenant isolation broken: both keys returned same pin %q", gotA[0].Name)
	}
	if gotA[0].Name != "pin-a" {
		t.Errorf("tenant A: want pin-a, got %s", gotA[0].Name)
	}
	if gotB[0].Name != "pin-b" {
		t.Errorf("tenant B: want pin-b, got %s", gotB[0].Name)
	}
}

// TestPinLRU_InvalidateRemovesEntry: Invalidate(key) removes the entry
// so the next Get returns false.
func TestPinLRU_InvalidateRemovesEntry(t *testing.T) {
	t.Parallel()
	cache, err := snapshot.NewPinLRU(64)
	if err != nil {
		t.Fatalf("NewPinLRU: %v", err)
	}

	key := snapshot.PinCacheKey{TenantID: "t", Namespace: "ns", Table: "tbl"}
	pin := []snapshot.SnapshotPin{{Name: "pin-inv", ExpiryUTC: time.Now().UTC().Add(time.Hour)}}
	cache.Add(key, pin)

	if _, ok := cache.Get(key); !ok {
		t.Fatal("expected entry before Invalidate")
	}
	cache.Invalidate(key)
	if _, ok := cache.Get(key); ok {
		t.Error("expected entry to be removed after Invalidate")
	}
}
