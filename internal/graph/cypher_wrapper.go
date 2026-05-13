// Cypher telemetry middleware — D-001.14 contract end-to-end.
//
// ExecuteCypher is the canonical entry-point for any production caller
// that wants Cypher execution PLUS the four D-001.14 metrics, an OTel
// trace span carrying the standard DB semantic-convention attributes,
// and sampled EXPLAIN-based node/edge counting.
//
// Layering: this file sits ABOVE internal/graph/client.go::Cypher —
// it delegates to that method for the actual SQL execution. Existing
// callers of GraphClient.Cypher continue to work unchanged; the
// telemetry layer is opt-in through ExecuteCypher (or its
// receiver-method alias GraphClient.CypherWithTelemetry).
//
// Execution path:
//
//  1. Start an OTel span "cypher.execute" with the four DB semconv
//     attributes (db.system.name / db.namespace / db.operation.name /
//     db.query.text). This validates Assumption A6 — the OTel DB
//     semantic-conventions vocabulary fits AGE-wrapped Postgres at
//     the application layer with no extension required.
//  2. Record start wall-clock.
//  3. Delegate to GraphClient.Cypher.
//  4. Observe cypher_duration_ms (every call).
//  5. If the call was slow (>500ms) OR caught by the 1% sampler,
//     run a second `EXPLAIN (FORMAT JSON, ANALYZE)` pass against the
//     same statement, parse via ParseExplain, observe
//     cypher_nodes_visited + cypher_edges_traversed.
//  6. On error path: increment cypher_errors_total{error_type=...},
//     record the error on the span, return the error wrapped in
//     CypherExecutionError so callers can `errors.As` to recover the
//     original query text without losing the cause.

package graph

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// CypherExecutionError wraps the underlying pgx (or pre-parser) error
// alongside the Cypher statement text that produced it. Callers can
// use errors.As to recover the wrapper, or errors.Unwrap (via
// Unwrap()) to get the original Cause. The Query field MUST NOT be
// emitted to high-cardinality metric labels — it is captured here for
// log / span context only.
type CypherExecutionError struct {
	Cause error
	Query string
}

func (e *CypherExecutionError) Error() string {
	return fmt.Sprintf("cypher: %v (query=%q)", e.Cause, e.Query)
}

func (e *CypherExecutionError) Unwrap() error { return e.Cause }

// sampler is the package-scoped RNG used by ExecuteCypher's 1% sampler.
// We use math/rand (not crypto/rand) deliberately — the sampling
// decision is not security-sensitive, and the math/rand cost is
// negligible vs the SQL round-trip we are deciding about.
var (
	samplerMu sync.Mutex
	sampler   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// sampleNow returns true with probability ~1%. Mutex-guarded because
// rand.Rand is not safe for concurrent use; the contention is bounded
// by Cypher call rate which the upstream pool throttles to <10K/s.
func sampleNow() bool {
	samplerMu.Lock()
	defer samplerMu.Unlock()
	return sampler.Float64() < 0.01
}

// slowQueryThresholdMs is the duration above which we ALWAYS run an
// EXPLAIN ANALYZE follow-up regardless of the sampler. 500ms matches
// the bucket boundary in CypherDurationMs and the threshold called out
// in 00-RESEARCH.md §Pattern 3.
const slowQueryThresholdMs = 500.0

// ExecuteCypher is the telemetry-instrumented wrapper around
// GraphClient.Cypher. See package-level docs for the full execution
// path. The caller closes the returned pgx.Rows; on the error path
// the returned Rows is nil.
//
// Span attributes set (OTel DB semconv 1.27+ — validates A6):
//   - db.system.name   = "postgresql"
//   - db.namespace     = "<graph>|ag_catalog"
//   - db.operation.name = "cypher"
//   - db.query.text    = <statement> (parameterised; not sanitised per
//     the OTel semconv guidance: only sanitise un-parameterised text)
func ExecuteCypher(
	ctx context.Context,
	client *GraphClient,
	graph string,
	stmt string,
	args ...any,
) (pgx.Rows, error) {
	tracer := otel.Tracer("neksur.graph")
	ctx, span := tracer.Start(ctx, "cypher.execute")
	defer span.End()

	span.SetAttributes(
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.namespace", graph+"|ag_catalog"),
		attribute.String("db.operation.name", "cypher"),
		attribute.String("db.query.text", stmt),
	)

	start := time.Now()
	rows, err := client.Cypher(ctx, graph, stmt, args...)
	durationMs := float64(time.Since(start).Milliseconds())
	CypherDurationMs.WithLabelValues(graph).Observe(durationMs)

	if err != nil {
		CypherErrorsTotal.WithLabelValues(graph, errorTypeName(err)).Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, &CypherExecutionError{Cause: err, Query: stmt}
	}

	// Sampling decision: slow queries (>500ms) ALWAYS get EXPLAIN
	// ANALYZE follow-up; otherwise the 1% random sampler. We tolerate
	// the EXPLAIN call failing — observability MUST NOT fail the
	// caller's query — and only emit nodes/edges histograms on
	// successful parse.
	if durationMs > slowQueryThresholdMs || sampleNow() {
		if nodes, edges, perr := explainAndParse(ctx, client, graph, stmt, args...); perr == nil {
			CypherNodesVisited.WithLabelValues(graph).Observe(float64(nodes))
			CypherEdgesTraversed.WithLabelValues(graph).Observe(float64(edges))
		}
		// On EXPLAIN parse failure we deliberately swallow the error;
		// the primary Rows result is what the caller cares about. A
		// future enhancement could add a counter for parse failures
		// to track AGE plan-shape regressions.
	}

	return rows, nil
}

// CypherWithTelemetry is the receiver-method form of ExecuteCypher.
// New call sites in cmd/neksur-server SHOULD prefer this form;
// existing callers of GraphClient.Cypher (which are observability-
// agnostic) continue to work unchanged.
func (g *GraphClient) CypherWithTelemetry(
	ctx context.Context,
	graph string,
	stmt string,
	args ...any,
) (pgx.Rows, error) {
	return ExecuteCypher(ctx, g, graph, stmt, args...)
}

// explainAndParse runs `EXPLAIN (FORMAT JSON, ANALYZE) <wrapped-cypher>`
// against the same connection pool the primary call used, fetches the
// single resulting JSON row, and delegates to ParseExplain.
//
// EXPLAIN ANALYZE re-EXECUTES the query — this is intentional. The
// 1% + slow-only sampler bounds the doubled-execution cost. A
// production tuning lever is the slowQueryThresholdMs constant.
func explainAndParse(
	ctx context.Context,
	client *GraphClient,
	graph string,
	stmt string,
	args ...any,
) (nodes int64, edges int64, err error) {
	// We deliberately re-quote the graph name and re-wrap the cypher
	// statement here so the EXPLAIN target matches the original call
	// shape exactly. The pgx positional binding handles args.
	wrapped := fmt.Sprintf(
		"EXPLAIN (FORMAT JSON, ANALYZE) SELECT * FROM cypher(%s, $$ %s $$) AS (result ag_catalog.agtype)",
		quoteSQLString(graph), stmt,
	)
	row := client.pool.QueryRow(ctx, wrapped, args...)
	var planJSON []byte
	if err := row.Scan(&planJSON); err != nil {
		return 0, 0, fmt.Errorf("explain scan: %w", err)
	}
	return ParseExplain(planJSON)
}

// errorTypeName produces a bounded-cardinality label string identifying
// the error's concrete Go type. We prefer the inner pgx.PgError code
// (e.g., "pg:42601") when available because that's the most actionable
// fingerprint for on-call. Otherwise we use reflect.TypeOf(err).String()
// to capture the wrapper type (e.g., "*fmt.wrapError"). A truly nil
// error returns "none" — this branch is only hit by belt-and-braces
// defensive callers because the error path is gated upstream.
func errorTypeName(err error) string {
	if err == nil {
		return "none"
	}
	// Unwrap to find a *pgconn.PgError if present — its SQLSTATE code
	// is the most actionable triage label.
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return "pg:" + pgErr.SQLState()
	}
	// Unwrap to the bottom of the chain so a bare fmt.Errorf wrapper
	// doesn't dominate the label cardinality.
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}
	t := reflect.TypeOf(err)
	if t == nil {
		return "error"
	}
	return t.String()
}
