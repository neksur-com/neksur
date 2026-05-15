//go:build integration

// Plan 01-07 Task 3 [BLOCKING] — in-process trigger dedup.
//
// Spawns 3 producers (one per source: poller / webhook / s3events) all
// pushing the SAME Hit{MetadataLocation: ...} to the dispatch channel
// within 100ms. Asserts Scanner.Scan called exactly ONCE — proving the
// sync.Map dedup is the load-bearing in-process invariant.

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/detect/dispatch"
)

// recordingTriggerScanner is a Scanner stub for the trigger-dedup
// BLOCKING gate. Counts Scan calls atomically and records the source
// labels (proves which producer "won the race" — for diagnostic logs).
type recordingTriggerScanner struct {
	mu       sync.Mutex
	count    int32
	sources  []string
}

func (r *recordingTriggerScanner) Scan(_ context.Context, h dispatch.Hit) error {
	atomic.AddInt32(&r.count, 1)
	r.mu.Lock()
	r.sources = append(r.sources, h.Source)
	r.mu.Unlock()
	return nil
}

// TestDetectTriggerDedup — three producers push the same Hit; assert
// scanner.Scan called exactly once (in-process sync.Map dedup).
func TestDetectTriggerDedup(t *testing.T) {
	rec := &recordingTriggerScanner{}
	in := make(chan dispatch.Hit, 16)
	t.Setenv("NEKSUR_L3_WORKERS", "4")
	pool := dispatch.NewPool(in, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)

	hit := dispatch.Hit{
		TenantID:         "11111111-1111-4111-8111-111111111111",
		MetadataLocation: "s3://test/snap1/metadata.json",
	}
	var wg sync.WaitGroup
	for _, src := range []string{"poller", "polaris-webhook", "s3-event"} {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			h := hit
			h.Source = src
			in <- h
		}(src)
	}
	wg.Wait()

	// Wait for the workers to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&rec.count) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Allow extra time for any rogue duplicate dispatches.
	time.Sleep(100 * time.Millisecond)

	got := atomic.LoadInt32(&rec.count)
	if got != 1 {
		t.Errorf("scanner.Scan called %d times across 3 producers; want exactly 1 (in-process dedup)",
			got)
		t.Logf("sources received: %+v", rec.sources)
	}
}
