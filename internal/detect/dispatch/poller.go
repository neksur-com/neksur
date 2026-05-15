// 30-second baseline poller — D-1.11 always-on safety net.
//
// The poller is the third trigger source (alongside Polaris webhook +
// S3 ObjectCreated SNS+SQS). When webhooks misfire (Polaris node
// outage, SNS region failure), the poller catches the missed snapshots
// within 30 seconds. ADR-003 §5.4 explicitly calls out the
// belt-and-suspenders trigger model — multiple sources feeding ONE
// dedup-protected pool is more resilient than any single source.
//
// Per-tenant iteration:
//   1. Enumerate active tenants from public.tenants WHERE lifecycle_state = 'active'.
//   2. For each tenant, query the graph via gc.ExecuteInTenant for
//      Snapshot vlabels with committed_at within the last 1 minute
//      (overlap-tolerant; the in-process dedup catches duplicates).
//   3. Push Hit{Source: "poller"} to the channel.
//
// Performance: the poller is INTENTIONALLY low-cost — a per-tenant
// MATCH that returns recent snapshots. With ~100 tenants per replica
// the overhead is sub-second per poll cycle. Phase 6 may switch to a
// Postgres LISTEN/NOTIFY-driven poller if tenant counts grow into the
// thousands.

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
)

// DefaultPollerInterval is the 30s baseline poll cadence per D-1.11.
const DefaultPollerInterval = 30 * time.Second

// pollerLookbackWindow — query for snapshots committed in the last
// 60 seconds (2x the poll interval) to be overlap-tolerant. The
// in-process dedup catches the duplicates so the overlap is cheap.
const pollerLookbackWindow = 60 * time.Second

// pollerMaxPerTenant — cap on snapshots pulled per tenant per cycle.
// Bounded to keep one tenant's burst from starving others.
const pollerMaxPerTenant = 100

// RunPoller starts the 30-second baseline poller per tenant. Blocks
// until ctx.Done.
//
// Parameters:
//   - ctx       — cancellation context. Cancellation drains the
//                 in-flight tenant pass and exits cleanly.
//   - adminPool — admin pgxpool for SELECT public.tenants. Distinct
//                 from the GraphClient pool because public.tenants is
//                 NOT in any tenant schema.
//   - gc        — graph client (per-tenant Snapshot enumeration).
//   - in        — the dispatch channel — push Hit{Source: "poller"}.
//   - interval  — poll cadence (typically DefaultPollerInterval); tests
//                 may pass a shorter value.
//
// On any error during a tenant pass the poller logs via slog.Error and
// continues with the next tenant (a single tenant's bad state must
// not block the others).
func RunPoller(
	ctx context.Context,
	adminPool *pgxpool.Pool,
	gc *graph.GraphClient,
	in chan<- Hit,
	interval time.Duration,
) {
	if interval <= 0 {
		interval = DefaultPollerInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run an initial pass immediately at startup so the first poll
	// doesn't wait `interval` (operators expect detection to start as
	// soon as the server is up).
	pollOnce(ctx, adminPool, gc, in)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce(ctx, adminPool, gc, in)
		}
	}
}

// pollOnce executes one cycle: enumerate active tenants, query each
// for fresh snapshots, push to channel. Errors logged + skipped per
// tenant.
func pollOnce(
	ctx context.Context,
	adminPool *pgxpool.Pool,
	gc *graph.GraphClient,
	in chan<- Hit,
) {
	tenants, err := listActiveTenants(ctx, adminPool)
	if err != nil {
		slog.Error("dispatch/poller: list active tenants failed", "err", err)
		return
	}

	since := time.Now().UTC().Add(-pollerLookbackWindow).Format(time.RFC3339Nano)
	for _, tenantID := range tenants {
		select {
		case <-ctx.Done():
			return
		default:
		}
		hits, err := freshSnapshots(ctx, gc, tenantID, since)
		if err != nil {
			slog.Error("dispatch/poller: fresh snapshots failed",
				"tenant", tenantID, "err", err)
			continue
		}
		for _, h := range hits {
			select {
			case <-ctx.Done():
				return
			case in <- h:
			}
		}
	}
}

// listActiveTenants returns tenant UUIDs whose lifecycle_state is
// 'active'. Suspended / wind-down tenants don't get scanned (their
// commit paths return 503 anyway per D-0.5.20).
func listActiveTenants(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text FROM public.tenants
		WHERE lifecycle_state = 'active'
	`)
	if err != nil {
		return nil, fmt.Errorf("dispatch/poller: query tenants: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("dispatch/poller: scan tenant id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dispatch/poller: rows err: %w", err)
	}
	return out, nil
}

// freshSnapshots queries the per-tenant graph for Snapshot vlabels
// committed since `since` (RFC3339Nano-formatted timestamp). Returns a
// slice of Hit ready to push to the dispatch channel.
//
// Cypher MATCH shape uses a property-comparison filter (no temporal
// arithmetic in AGE 1.6 — we splice the timestamp string and rely on
// the lexicographic comparison of RFC3339Nano strings which works
// because the format is fixed-width).
func freshSnapshots(
	ctx context.Context,
	gc *graph.GraphClient,
	tenantID string,
	since string,
) ([]Hit, error) {
	var hits []Hit
	err := gc.ExecuteInTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stmt := fmt.Sprintf(
			`MATCH (s:Snapshot) WHERE s.committed_at > '%s' RETURN s.metadata_location LIMIT %d`,
			strings.ReplaceAll(since, "'", "\\'"),
			pollerMaxPerTenant,
		)
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
			stmt,
		)
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("dispatch/poller: cypher query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var loc string
			if err := rows.Scan(&loc); err != nil {
				return fmt.Errorf("dispatch/poller: scan: %w", err)
			}
			loc = stripAgtypeQuotes(loc)
			if loc == "" {
				continue
			}
			hits = append(hits, Hit{
				TenantID:         tenantID,
				MetadataLocation: loc,
				Source:           "poller",
			})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("dispatch/poller: rows err: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// stripAgtypeQuotes removes JSON-style surrounding quotes from a scalar
// agtype string result (mirrors internal/policy/store/age.go's helper —
// duplicated to avoid a cross-package dependency).
func stripAgtypeQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}
