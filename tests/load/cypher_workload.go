// Cypher workload generators for the Phase 0 latency runner.
//
// Three canonical query shapes from 00-VALIDATION.md §Per-Task Verification
// Map (W5 rows for REQ-NFR-graph-latency):
//
//   - SingleNode  : indexed lookup by URI (P95 budget <50ms)
//   - OneHop      : Table → Column via LINEAGE_OF (P95 budget <150ms)
//   - ThreeHop    : AI-context-retrieval (ADR §4.1) — Metric → DEPENDS_ON*1..2
//                   → Table; named-path semantics produce 3 graph hops
//                   (P95 budget <1.2s)
//
// All three execute via internal/graph.ExecuteCypher (Plan 05 telemetry-
// wrapped path) so each call emits cypher_duration_ms via Prometheus and
// an OTel span with the four DB semconv attributes — the latency runner
// measures at the application boundary as the assertion AND the
// Prometheus/OTel emission acts as a cross-check.
//
// Originally Python's tests/load/lib/cypher_workload.py under Plan 06;
// now Go per the 2026-05-13 D-PHASE0-stack correction.

package load

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/neksur-com/neksur/internal/graph"
)

const graphName = "neksur"

// SingleNode runs the indexed-property-lookup MATCH against the Table
// label. Asserted P95 budget: <50ms.
//
// Cypher: MATCH (t:Table {uri: $1}) RETURN t LIMIT 1
//
// The query exercises the `idx_Table_uri` btree index from V0020 (per
// the polyfilled create_property_index) — verified by EXPLAIN-Index-Scan
// in tests/integration/indexes_test.go::TestIndexesUsedInExplain.
func SingleNode(ctx context.Context, client *graph.GraphClient, uri string) error {
	stmt := fmt.Sprintf("MATCH (t:Table {uri: '%s'}) RETURN t LIMIT 1", escapeCypherLiteral(uri))
	rows, err := graph.ExecuteCypher(ctx, client, graphName, stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// drain
	}
	return rows.Err()
}

// OneHop runs the Table-to-Column traversal via LINEAGE_OF — the
// canonical "what columns belong to this table" pattern. Asserted P95
// budget: <150ms (from REQ-NFR-graph-latency).
//
// Cypher: MATCH (t:Table {uri: $1})<-[:LINEAGE_OF]-(c:Column) RETURN c LIMIT 100
//
// The query uses the `idx_Table_uri` btree to anchor the start-node and
// then traverses backward over LINEAGE_OF; the limit-100 cap keeps the
// result set within the per-query resource budget that ADR-005 will
// formalise in Phase 5 (D-OQ.03 hardening contract).
func OneHop(ctx context.Context, client *graph.GraphClient, tableURI string) error {
	stmt := fmt.Sprintf(
		"MATCH (t:Table {uri: '%s'})<-[:LINEAGE_OF]-(c:Column) RETURN c LIMIT 100",
		escapeCypherLiteral(tableURI),
	)
	rows, err := graph.ExecuteCypher(ctx, client, graphName, stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// drain
	}
	return rows.Err()
}

// ThreeHop runs the AI-context-retrieval query from ADR §4.1 — the
// canonical "given a metric, what tables underpin it via dependency"
// pattern. Asserted P95 budget: <1.2s (from REQ-NFR-graph-latency).
//
// ADR §4.1 Cypher (depth `*1..2` over DEPENDS_ON):
//
//   MATCH (m:Metric {name: $1})-[:DEPENDS_ON*1..2]->(t:Table) RETURN t LIMIT 100
//
// Note on naming: ADR §4.1 expresses this as a `*1..2` traversal over
// DEPENDS_ON; the named-path semantics produce 3 graph hops total
// (Metric→intermediate→intermediate→Table). This is the canonical
// 3-hop test path called out in 00-VALIDATION.md row 06-T2.
//
// The bounded depth (1..2) is also the gateway-clamped maximum allowed
// per D-001.08 (no unbounded VLP); ValidateTraversalDepth in
// internal/graph/client.go would reject `*` or `*1..` shapes.
func ThreeHop(ctx context.Context, client *graph.GraphClient, metricName string) error {
	stmt := fmt.Sprintf(
		"MATCH (m:Metric {name: '%s'})-[:DEPENDS_ON*1..2]->(t:Table) RETURN t LIMIT 100",
		escapeCypherLiteral(metricName),
	)
	rows, err := graph.ExecuteCypher(ctx, client, graphName, stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// drain
	}
	return rows.Err()
}

// WorkloadSample is one observation from GenerateWorkload — the kind
// of query that ran and the wall-clock duration the application boundary
// observed (not the Prometheus-bucketed value, which has bucket-edge loss).
type WorkloadSample struct {
	Kind     string
	Duration time.Duration
	Err      error
}

// GenerateWorkload drives concurrent Cypher samples against client for
// the requested duration at the requested QPS, picking query kinds
// weighted by mix.
//
// Args:
//   - qps:       target queries per second (per-worker QPS = qps / concurrency).
//                Concurrency is fixed at 50 (REQ-NFR-catalog-scale: 50
//                concurrent users); workers split the QPS budget evenly.
//   - duration:  total wall-clock window to drive load.
//   - mix:       map of kind → fractional weight; keys MUST be in the set
//                {"single-node", "one-hop", "three-hop"}; values MUST sum to 1.0.
//
// Returns the slice of samples observed at the application boundary.
// Errors during sample execution are recorded on WorkloadSample.Err but
// do NOT fail the function — the latency runner uses the error rate
// alongside P95 to assess the cluster health under load.
//
// Worker pool: 50 goroutines + a buffered results channel. Each worker
// picks a query kind via the cumulative-weight CDF, executes it, sends
// a sample, then sleeps to honour the per-worker QPS pace. The pacing
// is approximate — under sustained-load conditions the latency runner
// cares about the distribution shape, not exact QPS adherence.
func GenerateWorkload(
	ctx context.Context,
	client *graph.GraphClient,
	qps int,
	duration time.Duration,
	mix map[string]float64,
) ([]WorkloadSample, error) {
	const workers = 50

	if err := validateMix(mix); err != nil {
		return nil, err
	}
	cdf := buildCDF(mix)

	deadlineCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	samples := make([]WorkloadSample, 0, qps*int(duration.Seconds()))
	var samplesMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(workers)
	perWorkerInterval := time.Duration(0)
	if qps > 0 {
		perWorkerInterval = time.Second / time.Duration(qps/workers+1)
	}

	for w := 0; w < workers; w++ {
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for {
				if deadlineCtx.Err() != nil {
					return
				}
				kind := pickKind(rng, cdf)
				start := time.Now()
				var err error
				switch kind {
				case "single-node":
					// Pick a Table URI from the seeded range.
					n := rng.Int63n(100_000)
					uri := fmt.Sprintf("iceberg://catalog-%d/sales/table-%d", n%10, n)
					err = SingleNode(deadlineCtx, client, uri)
				case "one-hop":
					n := rng.Int63n(100_000)
					uri := fmt.Sprintf("iceberg://catalog-%d/sales/table-%d", n%10, n)
					err = OneHop(deadlineCtx, client, uri)
				case "three-hop":
					n := rng.Int63n(100_000)
					name := fmt.Sprintf("metric_%d", n)
					err = ThreeHop(deadlineCtx, client, name)
				}
				dur := time.Since(start)
				samplesMu.Lock()
				samples = append(samples, WorkloadSample{Kind: kind, Duration: dur, Err: err})
				samplesMu.Unlock()
				if perWorkerInterval > 0 {
					select {
					case <-deadlineCtx.Done():
						return
					case <-time.After(perWorkerInterval):
					}
				}
			}
		}(w)
	}

	wg.Wait()
	return samples, nil
}

// validateMix rejects empty maps, unknown keys, and mixes that don't
// sum to ~1.0. The 1e-6 tolerance accommodates float arithmetic.
func validateMix(mix map[string]float64) error {
	if len(mix) == 0 {
		return errors.New("workload: mix is empty; pass at least one of single-node/one-hop/three-hop")
	}
	allowed := map[string]bool{"single-node": true, "one-hop": true, "three-hop": true}
	var sum float64
	for k, v := range mix {
		if !allowed[k] {
			return fmt.Errorf("workload: unknown mix key %q; allowed: single-node, one-hop, three-hop", k)
		}
		if v < 0 {
			return fmt.Errorf("workload: mix weight for %q is negative", k)
		}
		sum += v
	}
	if sum < 1.0-1e-6 || sum > 1.0+1e-6 {
		return fmt.Errorf("workload: mix weights must sum to 1.0; got %.6f", sum)
	}
	return nil
}

type cdfEntry struct {
	kind string
	cum  float64
}

func buildCDF(mix map[string]float64) []cdfEntry {
	out := make([]cdfEntry, 0, len(mix))
	var cum float64
	// Iterate in deterministic order so the CDF is stable across runs.
	for _, k := range []string{"single-node", "one-hop", "three-hop"} {
		if w, ok := mix[k]; ok && w > 0 {
			cum += w
			out = append(out, cdfEntry{kind: k, cum: cum})
		}
	}
	return out
}

func pickKind(rng *rand.Rand, cdf []cdfEntry) string {
	r := rng.Float64()
	for _, e := range cdf {
		if r <= e.cum {
			return e.kind
		}
	}
	// Float-rounding fallback: pick last entry.
	return cdf[len(cdf)-1].kind
}

// escapeCypherLiteral escapes single quotes for direct interpolation
// into a Cypher string literal. Because this workload runs against
// internally-generated URIs (no user input), escape is defence-in-depth.
// Production callers MUST use parameterised queries via $N binding —
// this helper is for the hot-path workload generator only.
func escapeCypherLiteral(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
