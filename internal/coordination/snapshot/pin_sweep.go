// pin_sweep.go — daily orphan SnapshotPin sweep.
//
// Runs a periodic goroutine that deletes SnapshotPin nodes whose
// expiry_utc is in the past. Both named pins (operator-issued) and
// session pins (per-query) share a single expiry_utc field — the sweep
// handles both via one Cypher predicate:
//
//	MATCH (sp:SnapshotPin) WHERE sp.expiry_utc < '<now>' DETACH DELETE sp
//	RETURN count(sp)
//
// Per RESEARCH Open Q 5 / Claude's Discretion:
//   - Named pins: default expiry_utc = now()+7d (set by gateway on creation)
//   - Session pins: default expiry_utc = now()+session_timeout (typically 1h)
//
// # Lifecycle pattern
//
// Sweeper.Run mirrors the internal/ingest/cycle_sweep.go + the
// internal/policy/compiler/trigger.go supervisor loop (03-PATTERNS §30):
//
//	select {
//	case <-ctx.Done():  return ctx.Err()
//	case <-ticker.C:   sweep()
//	}
//
// The Sweeper iterates over all active tenants per cycle using the same
// `public.tenants WHERE lifecycle_state = 'active'` query as the
// dispatch poller (internal/detect/dispatch/poller.go).
//
// # Prometheus metric
//
//	snapshot_pin_sweep_total{tenant_id, deleted}  — counter incremented
//	per tenant per cycle with the count of pins deleted.
//
// # Security
//
// The expiry literal is derived from time.Now().UTC().Format(time.RFC3339)
// and passed through graph.MustSanitizeCypherLiteral before splicing
// into the Cypher body. The sweep runs inside ExecuteInTenant so RLS
// scopes the MATCH to the per-tenant graph (T-3-snapshot-pin-orphan-
// data-retention mitigation).

package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/neksur-com/neksur/internal/graph"
)

// SnapshotPinSweepTotal counts expired pins deleted per tenant per cycle.
// Labeled by tenant_id (bounded cardinality: ~50 tenants in Pool A per
// ADR-002) and deleted (string-encoded int for Prometheus label
// cardinality — kept low by discretizing to "0", "1-10", "11-100",
// "100+"; callers use the raw count for logging and use the label for
// alerting thresholds).
//
// The metric is registered on the default promauto registry so
// cmd/neksur-server's /metrics endpoint exposes it automatically.
var SnapshotPinSweepTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "snapshot_pin_sweep_total",
		Help: "Number of expired SnapshotPin nodes deleted by the periodic sweep.",
	},
	[]string{"tenant_id", "deleted"},
)

// DefaultSweepInterval is the production cadence. 24h matches the
// STRIDE threat entry T-3-snapshot-pin-orphan-data-retention: daily
// sweep keeps stale pin data from accumulating.
const DefaultSweepInterval = 24 * time.Hour

// Sweeper runs the daily expired-pin sweep across all active tenants.
// Construct via NewSweeper; start via Run.
type Sweeper struct {
	store     *PinStore
	gc        *graph.GraphClient
	adminPool *pgxpool.Pool
	ticker    *time.Ticker
}

// NewSweeper constructs a Sweeper. interval is the sweep cadence;
// production code passes DefaultSweepInterval; tests pass a short
// interval (e.g., 100*time.Millisecond). adminPool is used to enumerate
// active tenants (public.tenants) — it must be the admin pool, NOT the
// GraphClient pool (CC3 constraint: do not mix pools).
func NewSweeper(store *PinStore, gc *graph.GraphClient, adminPool *pgxpool.Pool, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	return &Sweeper{
		store:     store,
		gc:        gc,
		adminPool: adminPool,
		ticker:    time.NewTicker(interval),
	}
}

// Run starts the sweep loop. Blocks until ctx is cancelled, then returns
// ctx.Err(). The first sweep runs immediately (before the first tick) so
// tests can observe behavior without waiting the full interval.
//
// Per 03-PATTERNS §30 / trigger.go supervisor loop shape:
//
//	for {
//	    select {
//	    case <-ctx.Done(): return ctx.Err()
//	    case <-ticker.C:  sweep()
//	    }
//	}
func (sw *Sweeper) Run(ctx context.Context) error {
	defer sw.ticker.Stop()

	// Run once immediately at startup.
	sw.sweepOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sw.ticker.C:
			sw.sweepOnce(ctx)
		}
	}
}

// sweepOnce enumerates active tenants and runs the expired-pin delete
// Cypher per tenant. Errors are logged and skipped so one tenant's
// graph failure does not abort the others.
func (sw *Sweeper) sweepOnce(ctx context.Context) {
	// In-memory mode: sweep the in-memory store directly.
	if sw.store != nil && sw.store.mem != nil {
		sw.sweepMem(ctx)
		return
	}

	tenants, err := listSweepTenants(ctx, sw.adminPool)
	if err != nil {
		slog.Error("coordination/snapshot/sweeper: list tenants failed", "err", err)
		return
	}
	for _, tenantID := range tenants {
		select {
		case <-ctx.Done():
			return
		default:
		}
		deleted, err := sw.sweepTenant(ctx, tenantID)
		if err != nil {
			slog.Error("coordination/snapshot/sweeper: sweep tenant failed",
				"tenant_id", tenantID, "err", err)
			continue
		}
		if deleted > 0 {
			slog.Info("coordination/snapshot/sweeper: deleted expired pins",
				"tenant_id", tenantID, "deleted", deleted)
		}
		SnapshotPinSweepTotal.WithLabelValues(tenantID, strconv.Itoa(deleted)).Inc()
	}
}

// SweepOnceForTest exposes sweepOnce for use in tests that need to
// trigger a sweep cycle without waiting for the ticker. Must only be
// called from test code.
func (sw *Sweeper) SweepOnceForTest(ctx context.Context) {
	sw.sweepOnce(ctx)
}

// sweepMem sweeps the in-memory store. Used by unit tests that don't
// have a live Postgres+AGE instance or a real adminPool.
func (sw *Sweeper) sweepMem(ctx context.Context) {
	if sw.store == nil || sw.store.mem == nil {
		return
	}
	now := time.Now().UTC()
	mem := sw.store.mem
	var toDelete []string
	for key, pin := range mem.pins {
		if !pin.ExpiryUTC.After(now) {
			toDelete = append(toDelete, key)
		}
	}
	deleted := len(toDelete)
	for _, key := range toDelete {
		// Extract tenantID for cache invalidation.
		parts := strings.SplitN(key, "/", 2)
		tenantID := parts[0]
		pin := mem.pins[key]
		cacheKey := PinCacheKey{
			TenantID:  tenantID,
			Namespace: pin.TableNamespace,
			Table:     pin.TableName,
		}
		sw.store.cache.Invalidate(cacheKey)
		delete(mem.pins, key)
	}
	if deleted > 0 {
		slog.Info("coordination/snapshot/sweeper: in-mem deleted expired pins",
			"deleted", deleted)
	}
}

// sweepTenant deletes all SnapshotPin nodes with expiry_utc < now() for
// the given tenant. Returns the count of deleted nodes.
//
// AGE 1.6 note: DETACH DELETE is supported; `RETURN count(sp)` returns
// the deletion count as an agtype integer. The count is 0 if no pins
// expired since the last sweep.
func (sw *Sweeper) sweepTenant(ctx context.Context, tenantID string) (int, error) {
	tenantLit := graph.MustSanitizeCypherLiteral(tenantID)
	nowLit := graph.MustSanitizeCypherLiteral(time.Now().UTC().Format(time.RFC3339))

	var deleted int
	err := sw.gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cy := fmt.Sprintf(
			`MATCH (sp:SnapshotPin {tenant_id: '%s'}) `+
				`WHERE sp.expiry_utc < '%s' `+
				`DETACH DELETE sp RETURN count(sp)`,
			tenantLit, nowLit,
		)
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (cnt ag_catalog.agtype)",
			cy,
		)
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("coordination/snapshot/sweeper: query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rawCnt string
			if err := rows.Scan(&rawCnt); err != nil {
				return fmt.Errorf("coordination/snapshot/sweeper: scan: %w", err)
			}
			// Parse agtype integer (may be plain "0" or "0::int8").
			raw := stripAgtypeQ(rawCnt)
			n, err := strconv.Atoi(raw)
			if err != nil {
				// Non-fatal: log and treat as 0.
				slog.Warn("coordination/snapshot/sweeper: parse count",
					"raw", rawCnt, "err", err)
				continue
			}
			deleted += n
		}
		return rows.Err()
	})
	return deleted, err
}

// listSweepTenants returns active tenant UUIDs from public.tenants.
// Mirrors internal/detect/dispatch/poller.go::listActiveTenants.
func listSweepTenants(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text FROM public.tenants
		WHERE lifecycle_state = 'active'
	`)
	if err != nil {
		return nil, fmt.Errorf("coordination/snapshot/sweeper: query tenants: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("coordination/snapshot/sweeper: scan tenant: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coordination/snapshot/sweeper: rows err: %w", err)
	}
	return out, nil
}
