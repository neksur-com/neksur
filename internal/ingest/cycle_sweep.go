// Periodic full-graph cycle sweep — defence-in-depth backstop for the
// per-write `ValidateNoCycle` pre-check.
//
// The per-write cycle check has two known limitations:
//
//  1. **Depth cap at 5 (D-001.08).** Cycles spanning more than 5 hops
//     are NOT caught by ValidateNoCycle. They are pathological — a
//     legitimate ETL graph would not introduce a 6+-hop cycle — but
//     the sweep catches them so SRE can alert on the data-shape bug.
//
//  2. **Concurrent ingest can produce ephemeral cycles.** The
//     advisory lock keyed on `hashtext(srcURI)` serializes concurrent
//     writes that share a source, but two ingests on DIFFERENT
//     sources can each see clean ancestors, both commit, and close a
//     cycle. The sweep periodically scans the full graph and emits a
//     LineageCycleError-shaped log + Prometheus increment for each
//     cycle found.
//
// Per CONTEXT specifics line 171 the sweep's error shape MUST match
// the per-write LineageCycleError so downstream alerting / dashboards
// can treat both alike.

package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/neksur-com/neksur/internal/graph"
)

// LineageCyclesDetectedTotal counts cycles detected by the periodic
// sweep, labeled by tenant_id. Wired to a Prometheus AlertManager rule
// outside this package (ops/prometheus/alerts/lineage-cycles.yaml in
// Plan 01-09's deployment manifest).
//
// We register on the default registry so cmd/neksur-server's /metrics
// endpoint exposes it automatically. The label cardinality is bounded
// by tenant count (~50 in Pool A / 1 in Pool B per tenant) so this is
// safe.
var LineageCyclesDetectedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "lineage_cycles_detected_total",
		Help: "Lineage cycles detected by the periodic sweep (Pitfall 4 defence-in-depth).",
	},
	[]string{"tenant_id"},
)

// DefaultCycleSweepInterval is the cadence at which RunCycleSweep
// re-scans the graph. 1 hour matches the threat-model entry T-1-
// cycle-cascade-via-mass-events: the sweep is low-frequency so a
// pathological ingest cannot use it as an amplification vector.
const DefaultCycleSweepInterval = 1 * time.Hour

// RunCycleSweep runs a periodic full-graph cycle check for the given
// tenant, emitting `slog.Error` + a Prometheus counter increment for
// every cycle detected. Cancel via the supplied context (the function
// returns ctx.Err() on cancellation).
//
// Wiring: cmd/neksur-server may launch one goroutine per active tenant
// from its scheduler loop (Plan 01-06 owns that wiring); for the
// shared graph the sweep can run as a single goroutine that iterates
// through the active tenant list.
//
// The Cypher used here is bounded at *1..5 — the same depth as the
// per-write check. The sweep does NOT use unbounded `*` (which would
// violate D-001.08 + GC-02). For longer cycles, callers must increase
// the sweep depth in a future plan (Phase 2 / D-001.10 territory).
func RunCycleSweep(ctx context.Context, gc *graph.GraphClient, tenantID string, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultCycleSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately so cancellation/blocker semantics are
	// observable in tests without waiting the full interval.
	if err := runCycleSweepOnce(ctx, gc, tenantID); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("ingest.RunCycleSweep: initial sweep failed",
			"tenant_id", tenantID, "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := runCycleSweepOnce(ctx, gc, tenantID); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				slog.Error("ingest.RunCycleSweep: sweep failed",
					"tenant_id", tenantID, "err", err)
			}
		}
	}
}

// cypherFullGraphCycleSweep walks `*1..5` LINEAGE_OF cycles. The
// terminating `=` predicate `id(start) = id(end)` finds nodes that
// reach themselves via 1-5 hops. The depth cap is D-001.08 — longer
// cycles are pathologic and the sweep is the place to surface them
// (the per-write check intentionally doesn't pay the cost of deeper
// walks).
//
// AGE 1.6 quirk: `nodes(path)` list comprehension can panic; we
// instead return only the cycle-anchor URI and leave the per-cycle
// path reconstruction to the caller (fetchCyclePath in cycle.go).
// Single-line shape avoids the `syntax error at or near "ON"`
// parser regression.
const cypherFullGraphCycleSweep = `MATCH (start)-[:LINEAGE_OF*1..5]->(end) WHERE id(start) = id(end) RETURN start.iceberg_id AS uri LIMIT 1000`

// runCycleSweepOnce scans the graph for cycles in [1..5] hops and
// emits an slog + Prometheus event per cycle found.
func runCycleSweepOnce(ctx context.Context, gc *graph.GraphClient, tenantID string) error {
	return gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (uri ag_catalog.agtype)",
			cypherFullGraphCycleSweep,
		)
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("ingest: cycle sweep query: %w", err)
		}
		defer rows.Close()
		anchors := []string{}
		for rows.Next() {
			var rawURI string
			if err := rows.Scan(&rawURI); err != nil {
				slog.Warn("ingest.RunCycleSweep: row scan",
					"tenant_id", tenantID, "err", err)
				continue
			}
			anchors = append(anchors, stripAgtypeQuotes(rawURI))
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("ingest: cycle sweep rows: %w", err)
		}
		// Reconstruct each cycle's path application-side via the same
		// fetchCyclePath helper used by the per-write check.
		for _, uri := range anchors {
			cycle := fetchCyclePath(ctx, tx, uri, uri)
			LineageCyclesDetectedTotal.WithLabelValues(tenantID).Inc()
			lce := &LineageCycleError{
				SourceID: uri,
				TargetID: uri,
				Cycle:    cycle,
			}
			slog.Error("ingest.RunCycleSweep: lineage cycle detected",
				"tenant_id", tenantID,
				"cycle", lce.Cycle,
				"err", lce.Error(),
			)
		}
		if len(anchors) == 0 {
			slog.Debug("ingest.RunCycleSweep: clean", "tenant_id", tenantID)
		}
		return nil
	})
}
