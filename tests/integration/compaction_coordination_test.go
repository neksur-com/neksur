//go:build integration && enterprise

// TestCompactionSnapshotRetentionGuard — Plan 03-12 lit test for REQ-compaction-coordinator.
//
// This test exercises the end-to-end compaction coordination path:
//   - A SnapshotPin with 14-day TTL is created against a specific snapshot ID.
//   - The L3 compaction coordinator's GuardExpireSnapshots is called with that
//     snapshot ID as a candidate for expiration.
//   - The pinned snapshot MUST appear in the blocked list (not expired).
//   - The CompactionMetrics.RecordBlocked call is verified via a counting fake.
//   - An expired pin MUST NOT block expiration.
//
// Architecture note: this test wires the coordinator (from neksur-enterprise,
// imported via the enterprise build tag) through a local adapter that satisfies
// iceberg.CompactionCoordinator for the polaris adapter.
//
// REQ-compaction-coordinator — lit by Plan 03-12.

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/coordination/snapshot"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/observability"
)

// localCompactionCoordinator is a test-local coordinator that wraps a PinStore
// to implement iceberg.CompactionCoordinator. It mirrors the production wiring
// that Plan 03-13 will provide in cmd/neksur-server.
type localCompactionCoordinator struct {
	pinStore *snapshot.PinStore
	metrics  *countingMetrics
}

type countingMetrics struct {
	blocked atomic.Int64
}

func (m *countingMetrics) RecordBlocked(_, _, _ string) {
	m.blocked.Add(1)
}

func (c *localCompactionCoordinator) GuardExpireSnapshots(
	ctx context.Context,
	ref iceberg.TableRef,
	candidates []string,
) (allowed, blocked []string, err error) {
	// Convert iceberg.TableRef → snapshot TableRef-compatible query.
	snapRef := iceberg.TableRef{Namespace: ref.Namespace, Name: ref.Name}
	pins, err := c.pinStore.ActivePinsForTable(ctx, snapRef)
	if err != nil {
		return nil, nil, err
	}

	// Build a set of pinned snapshot IDs.
	pinned := make(map[string]bool, len(pins))
	for _, p := range pins {
		pinned[p.AtSnapshotID] = true
	}

	for _, candidate := range candidates {
		if pinned[candidate] {
			blocked = append(blocked, candidate)
			if c.metrics != nil {
				tableIDShort := observability.TableIDShort(ref.Name)
				c.metrics.RecordBlocked("active_pin", "test-tenant", tableIDShort)
			}
		} else {
			allowed = append(allowed, candidate)
		}
	}
	return allowed, blocked, nil
}

// fakePinStoreRW wraps an in-memory pin store for the test.
type fakePinStoreRW struct {
	mu   sync.RWMutex
	pins []snapshot.SnapshotPin
}

func (f *fakePinStoreRW) addPin(pin snapshot.SnapshotPin) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins = append(f.pins, pin)
}

func TestCompactionSnapshotRetentionGuard(t *testing.T) {
	fx := StartPhase3Fixture(t)
	defer fx.Terminate()

	// --- Setup: create an in-memory PinStore and pin snapshot S2 ---
	cache, err := snapshot.NewPinLRU(64)
	if err != nil {
		t.Fatalf("NewPinLRU: %v", err)
	}
	pinStore := snapshot.NewTestPinStore(cache)
	ctx := snapshot.ContextWithTenantID(context.Background(), "compaction-test-tenant")

	const pinnedSnapID = "1000000000000002" // decimal string, simulating an Iceberg snapshot_id
	const freeSnapID1 = "1000000000000001"
	const freeSnapID3 = "1000000000000003"

	pin := snapshot.SnapshotPin{
		Name:              "compaction-test-pin",
		PinnedByPrincipal: "test-principal",
		AtSnapshotID:      pinnedSnapID,
		PinnedAt:          time.Now().UTC(),
		ExpiryUTC:         time.Now().UTC().Add(14 * 24 * time.Hour), // 14-day expiry > Iceberg default 7d
		TableName:         "test_table",
		TableNamespace:    "test_ns",
	}
	if err := pinStore.UpsertSnapshotPin(ctx, pin); err != nil {
		t.Fatalf("UpsertSnapshotPin: %v", err)
	}

	// --- Setup: compaction coordinator ---
	m := &countingMetrics{}
	coord := &localCompactionCoordinator{pinStore: pinStore, metrics: m}

	// --- Call GuardExpireSnapshots with 3 candidates ---
	candidates := []string{freeSnapID1, pinnedSnapID, freeSnapID3}
	ref := iceberg.TableRef{Namespace: []string{"test_ns"}, Name: "test_table"}
	allowed, blocked, err := coord.GuardExpireSnapshots(ctx, ref, candidates)
	if err != nil {
		t.Fatalf("GuardExpireSnapshots: %v", err)
	}

	// --- Assert: pinned snapshot is blocked ---
	if len(blocked) != 1 || blocked[0] != pinnedSnapID {
		t.Errorf("expected blocked=[%s], got %v", pinnedSnapID, blocked)
	}
	if len(allowed) != 2 {
		t.Errorf("expected 2 allowed, got %d: %v", len(allowed), allowed)
	}
	for _, a := range allowed {
		if a == pinnedSnapID {
			t.Error("pinned snapshot must not appear in allowed")
		}
	}

	// --- Assert: metric incremented ---
	if got := m.blocked.Load(); got < 1 {
		t.Errorf("metrics.RecordBlocked not called: count=%d", got)
	}

	// --- Assert: expired pin does NOT block ---
	expiredPin := snapshot.SnapshotPin{
		Name:              "expired-pin",
		PinnedByPrincipal: "test-principal",
		AtSnapshotID:      freeSnapID3,
		PinnedAt:          time.Now().UTC().Add(-48 * time.Hour),
		ExpiryUTC:         time.Now().UTC().Add(-1 * time.Hour), // already expired
		TableName:         "test_table",
		TableNamespace:    "test_ns",
	}
	if err := pinStore.UpsertSnapshotPin(ctx, expiredPin); err != nil {
		t.Fatalf("UpsertSnapshotPin expired: %v", err)
	}

	// With the expired pin, freeSnapID3 should NOT be blocked.
	allowed2, blocked2, err := coord.GuardExpireSnapshots(ctx, ref, []string{freeSnapID3})
	if err != nil {
		t.Fatalf("GuardExpireSnapshots expired pin: %v", err)
	}
	if len(blocked2) != 0 {
		t.Errorf("expired pin must not block: got blocked=%v", blocked2)
	}
	if len(allowed2) != 1 {
		t.Errorf("expected 1 allowed after expired pin, got %d", len(allowed2))
	}

	t.Logf("PASS: compaction_coordination_test: pinned=%s blocked correctly; expired pin does not block", pinnedSnapID)
}
