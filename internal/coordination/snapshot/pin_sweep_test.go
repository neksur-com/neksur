// Tests for Sweeper — expired SnapshotPin sweep.
//
// Uses the in-memory fake store (NewTestPinStore) so tests run without
// a live Postgres+AGE instance.

package snapshot_test

import (
	"context"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/coordination/snapshot"
	"github.com/neksur-com/neksur/internal/iceberg"
)

// buildTestSweeper creates a Sweeper wired to the given PinStore using
// a short sweep interval. adminPool is nil in test mode (Sweeper uses
// in-memory path when store.mem != nil).
func buildTestSweeper(t *testing.T, store *snapshot.PinStore) *snapshot.Sweeper {
	t.Helper()
	// In test mode, adminPool=nil is safe — Sweeper detects in-memory
	// mode via store.mem != nil and skips the tenant-enumeration query.
	return snapshot.NewSweeper(store, nil, nil, 50*time.Millisecond)
}

// TestSweep_DeletesExpired: insert two pins (one expired, one active);
// run sweep; verify the expired pin is removed and the active one remains.
func TestSweep_DeletesExpired(t *testing.T) {
	t.Parallel()
	store, _ := buildTestStore(t)
	sw := buildTestSweeper(t, store)

	ctx := ctxWithTenant(t, "tenant-sweep")
	ref := iceberg.TableRef{Name: "sweeptbl", Namespace: []string{"ns"}}

	expired := snapshot.SnapshotPin{
		Name:              "pin-exp",
		PinnedByPrincipal: "user:grace@example.com",
		AtSnapshotID:      "snap-old",
		PinnedAt:          time.Now().UTC().Add(-2 * time.Hour),
		ExpiryUTC:         time.Now().UTC().Add(-1 * time.Minute), // expired
		TableName:         "sweeptbl",
		TableNamespace:    "ns",
	}
	active := snapshot.SnapshotPin{
		Name:              "pin-act",
		PinnedByPrincipal: "user:grace@example.com",
		AtSnapshotID:      "snap-new",
		PinnedAt:          time.Now().UTC(),
		ExpiryUTC:         time.Now().UTC().Add(7 * 24 * time.Hour), // active
		TableName:         "sweeptbl",
		TableNamespace:    "ns",
	}
	if err := store.UpsertSnapshotPin(ctx, expired); err != nil {
		t.Fatalf("upsert expired: %v", err)
	}
	if err := store.UpsertSnapshotPin(ctx, active); err != nil {
		t.Fatalf("upsert active: %v", err)
	}

	// Run one sweep cycle.
	sw.SweepOnceForTest(ctx)

	// After sweep, only the active pin should remain.
	pins, err := store.ActivePinsForTable(ctx, ref)
	if err != nil {
		t.Fatalf("ActivePinsForTable post-sweep: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected 1 active pin post-sweep, got %d", len(pins))
	}
	if pins[0].Name != "pin-act" {
		t.Errorf("expected pin-act to survive, got %s", pins[0].Name)
	}
}

// TestSweep_TenantScoped: sweep for tenant A does not delete pins for
// tenant B.
func TestSweep_TenantScoped(t *testing.T) {
	t.Parallel()
	store, _ := buildTestStore(t)
	sw := buildTestSweeper(t, store)

	ctxA := ctxWithTenant(t, "tenant-sweep-a")
	ctxB := ctxWithTenant(t, "tenant-sweep-b")
	ref := iceberg.TableRef{Name: "cross", Namespace: []string{"ns"}}

	pinA := snapshot.SnapshotPin{
		Name:              "pin-a-exp",
		PinnedByPrincipal: "user:henry@example.com",
		AtSnapshotID:      "snap-a",
		PinnedAt:          time.Now().UTC().Add(-2 * time.Hour),
		ExpiryUTC:         time.Now().UTC().Add(-1 * time.Minute), // expired
		TableName:         "cross",
		TableNamespace:    "ns",
	}
	pinB := snapshot.SnapshotPin{
		Name:              "pin-b-active",
		PinnedByPrincipal: "user:iris@example.com",
		AtSnapshotID:      "snap-b",
		PinnedAt:          time.Now().UTC(),
		ExpiryUTC:         time.Now().UTC().Add(7 * 24 * time.Hour), // active
		TableName:         "cross",
		TableNamespace:    "ns",
	}
	if err := store.UpsertSnapshotPin(ctxA, pinA); err != nil {
		t.Fatalf("upsert tenant A: %v", err)
	}
	if err := store.UpsertSnapshotPin(ctxB, pinB); err != nil {
		t.Fatalf("upsert tenant B: %v", err)
	}

	// Sweep sweeps ALL tenants in mem mode; verify the in-memory
	// scope correctly separates tenants.
	sw.SweepOnceForTest(context.Background())

	// Tenant A's expired pin should be gone.
	pinsA, err := store.ActivePinsForTable(ctxA, ref)
	if err != nil {
		t.Fatalf("ActivePinsForTable tenant A: %v", err)
	}
	if len(pinsA) != 0 {
		t.Errorf("tenant A: expected 0 pins after sweep, got %d", len(pinsA))
	}

	// Tenant B's active pin should survive.
	pinsB, err := store.ActivePinsForTable(ctxB, ref)
	if err != nil {
		t.Fatalf("ActivePinsForTable tenant B: %v", err)
	}
	if len(pinsB) != 1 {
		t.Errorf("tenant B: expected 1 active pin, got %d", len(pinsB))
	}
}

// TestSweep_GracefulShutdown: cancelling the context causes Run to
// return ctx.Err().
func TestSweep_GracefulShutdown(t *testing.T) {
	t.Parallel()
	store, _ := buildTestStore(t)
	sw := buildTestSweeper(t, store)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- sw.Run(ctx)
	}()

	// Cancel after a short delay.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after context cancellation")
	}
}
