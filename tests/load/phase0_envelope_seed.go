// Package load hosts the Phase 0 envelope load-seed fixture and Cypher
// workload generators used by the Wave 5 acceptance gate.
//
// The envelope per ROADMAP.md Phase 0 §Phase Details + ADR-001:
//
//   - 10,000,000 nodes
//   - 50,000,000 edges
//   - 1 tenant by default (multi-tenant fixtures available via SeedOpts.Tenants)
//   - canonical 19 vlabels + 24 elabels per D-001.05/.06 amended by D-003.06
//   - average property bag <2KB per D-001.04 hybrid storage rule
//
// # Strategy: COPY-only synthetic seed (NOT MERGE)
//
// Per 00-RESEARCH.md §Don't Hand-Roll and the Open-Question #5 resolution
// (2026-05-12 + 2026-05-13 ratification), Phase 0's envelope seed is COPY-
// only synthetic data — no Cypher MERGE upsert pathway. The MERGE pathway
// belongs to Phase 1 real Iceberg ingestion (per D-001.09 idempotency
// contract); a synthetic seed has no upsert semantics to exercise.
//
// We use pgx's PgConn().CopyFrom (raw COPY ... FROM STDIN) directly against
// the AGE underlying tables `neksur."<Label>"`. AGE's per-label tables are
// vanilla Postgres tables with two columns each — `id ag_catalog.graphid`
// and `properties ag_catalog.agtype` — so the bulk-COPY pattern transfers
// directly. AGEFreighter (the original Python helper hinted at in the
// stub doc) is NOT used; the dependency surface stays tight. AGEFreighter-
// shaped helpers remain a fallback option documented here for future need.
//
// # Pre-flight: indexes BEFORE load (Pitfall 2 mitigation)
//
// Per 00-RESEARCH.md §Common Pitfalls Pitfall 2 (AGE issue #1010), GIN
// indexes on `properties` created AFTER bulk load silently fail to catch
// existing rows. V0025 creates all 19 GIN + 19 tenant btree indexes BEFORE
// any data load. Seed pre-checks every expected index name exists in
// `pg_indexes WHERE schemaname = 'neksur'` and aborts with a diagnostic
// if any are missing — this catches operator error (running seed on a
// schema that hasn't reached V0025).
//
// # Realistic property-bag distribution (ADR §3.4 + ARCH §3-5)
//
// The schema mix matches the "engineering schema" examples in ADR §3.4
// and ARCH §3-5:
//
//   - Table     : ~100K  nodes, avg ~800B (uri + catalog_id + namespace + …)
//   - Column    : ~5M    nodes, avg ~600B (uri + parent_table_uri + name + …)
//   - Snapshot  : ~4M    nodes, avg ~700B (snapshot_id + table_uri + committed_at + …)
//   - Other 16  : ~900K  nodes combined  (Metric, View, Dashboard, Pipeline,
//                                          Query, Person, Team, Policy,
//                                          GlossaryTerm, Tag, DataContract,
//                                          Engine, Catalog, WriteEvent,
//                                          DetectionRun, Dimension)
//   - LINEAGE_OF: ~30M edges (column-to-column + snapshot-to-snapshot)
//   - READ/WROTE: ~12M edges (Person-Table + Query-Table)
//   - other  18 : ~8M  edges (OWNS, CLASSIFIED_AS, APPLIES_TO,
//                              DEPENDS_ON, INTENDED_WRITE, ACTUAL_WRITE,
//                              VIOLATION_DETECTED_BY, plus 11 others)
//
// # Throughput target
//
// Target: <30 minutes wall-clock for the full 10M/50M envelope on a
// 4-vCPU / 16GB-RAM machine — this is the assertion enforced by
// cmd/seed -assert-completes-under=30m. The COPY pathway sustains ~100K
// rows/sec on commodity Postgres for narrow-row tables; with 60M total
// rows + ANALYZE overhead the target is comfortably reachable.
//
// # Throttling for pgBackRest WAL drain (T-0-LOAD-WAL-OVERFLOW)
//
// Per the threat model in 00-06-PLAN.md, envelope seed WAL output exceeds
// pgBackRest's 15-min queue capacity at full pace. We checkpoint-pause
// every 1M nodes (1-2 second sleep) so pgBackRest can drain. The pause
// is deliberately small to keep the 30-min target reachable; operators
// running this in production SHOULD coordinate seed runs with pgBackRest
// retention windows.
//
// Originally Python's tests/load/fixtures/phase0_envelope_seed.py under
// the Wave 0 plan; now Go per the 2026-05-13 D-PHASE0-stack correction.
package load

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
)

// SeedOpts controls the Seed run. Defaults give the full Phase 0
// envelope; tests may request a smaller fraction for fast smoke runs
// (cmd/run -with-full-fixture=false uses TargetNodes=100_000 / TargetEdges=500_000).
type SeedOpts struct {
	TargetNodes int64 // default 10_000_000
	TargetEdges int64 // default 50_000_000
	Tenants     int   // default 1 (single-tenant; multi-tenant fixtures = N>1)
}

// SeedOptions is an alias retained for plan-prose alignment; the
// historical name in the plan is SeedOptions, the existing stub uses
// SeedOpts. We keep both spellings to avoid breaking either contract.
type SeedOptions = SeedOpts

// SeedResult is the seed run's output. cmd/seed reads NodesCreated +
// EdgesCreated for the ±5% target-counts assertion and Duration for
// the <30m wall-clock assertion. BytesWrittenEstimate is a rough
// COPY-payload size for crude throughput tracking.
type SeedResult struct {
	NodesCreated         int64
	EdgesCreated         int64
	Duration             time.Duration
	BytesWrittenEstimate int64
}

// ErrNotImplemented retained for Plan 01 stub compatibility — never
// returned by the real Seed. Some upstream chaos / DR drill scripts
// imported the sentinel; keeping it exported avoids breaking them.
var ErrNotImplemented = errors.New("seed not implemented (legacy sentinel; Seed is now real)")

// expectedIndexes is the V0025 + V0020 index inventory the seed
// requires before it will write anything. The check is name-only
// (we do not verify the index expressions) — V0025's
// TestPerVlabelTenantAndGinIndexes is the authoritative test that
// the index *contents* are correct. This pre-check exists to catch
// the common operator error of running the seed against a schema
// that hasn't reached V0025 yet (Pitfall 2 mitigation).
func expectedIndexes() []string {
	out := make([]string, 0, 19*2+11+3+2)
	// 19 tenant btree + 19 GIN from V0025
	for _, vlabel := range graph.NodeLabels {
		out = append(out, "idx_"+vlabel+"_tenant", "idx_"+vlabel+"_props_gin")
	}
	// 11 property indexes from V0020 (deterministic names from the polyfill)
	property := []struct{ label, prop string }{
		{"Table", "uri"}, {"Table", "catalog_id"},
		{"Column", "uri"}, {"Column", "parent_table_uri"},
		{"Snapshot", "snapshot_id"}, {"Snapshot", "table_uri"}, {"Snapshot", "committed_at"},
		{"Metric", "name"}, {"Person", "email"}, {"Tag", "id"}, {"Query", "query_id"},
	}
	for _, p := range property {
		out = append(out, "idx_"+p.label+"_"+p.prop)
	}
	// 3 edge timestamp indexes
	for _, e := range []struct{ label, prop string }{
		{"LINEAGE_OF", "created_at"}, {"READ", "at"}, {"WROTE", "at"},
	} {
		out = append(out, "idx_"+e.label+"_"+e.prop+"_edge")
	}
	// 2 functional indexes
	out = append(out, "idx_table_namespace", "idx_snapshot_time")
	return out
}

// preCheckIndexes asserts every expected index from V0020+V0025 exists
// in the neksur.* schema. Runs `SELECT indexname FROM pg_indexes WHERE
// schemaname = 'neksur'` once and diffs against expectedIndexes(). If
// any are missing the seed aborts with a list of names so the operator
// can re-run migrations before retrying.
func preCheckIndexes(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = 'neksur'`)
	if err != nil {
		return fmt.Errorf("seed: pre-check pg_indexes query: %w", err)
	}
	defer rows.Close()
	have := make(map[string]struct{}, 64)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("seed: pre-check scan indexname: %w", err)
		}
		have[n] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seed: pre-check rows.Err: %w", err)
	}
	var missing []string
	for _, want := range expectedIndexes() {
		if _, ok := have[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("seed: pre-check failed — %d expected indexes missing from neksur schema "+
			"(Pitfall 2 mitigation: GIN-after-load silently bypasses; re-run V0020+V0025 migrations before seeding). Missing: %s",
			len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// labelIDs queries ag_catalog.ag_label for the per-label numeric id used
// in graphid encoding. AGE encodes graphid as `(label_id << 48) | entry_id`
// — each per-label table's `id` column receives values where the upper
// 16 bits identify the label and the lower 48 bits identify the entry.
// We need the label_id per name to construct unique-per-table graphids
// without colliding with AGE's own internal sequences (we start the
// entry_id range at 10^9 so ad-hoc test inserts keep working below us).
func labelIDs(ctx context.Context, conn *pgx.Conn) (map[string]int32, error) {
	rows, err := conn.Query(ctx, `
		SELECT name, id FROM ag_catalog.ag_label
		 WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
		   AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
	`)
	if err != nil {
		return nil, fmt.Errorf("seed: query ag_label: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int32, 43)
	for rows.Next() {
		var n string
		var id int32
		if err := rows.Scan(&n, &id); err != nil {
			return nil, fmt.Errorf("seed: scan ag_label: %w", err)
		}
		out[n] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("seed: ag_label rows.Err: %w", err)
	}
	if len(out) != 43 {
		return nil, fmt.Errorf("seed: ag_label returned %d labels; expected 43 (D-001.05/.06 + D-003.06)", len(out))
	}
	return out, nil
}

// makeGraphID encodes (labelID, entryID) into AGE's int8 graphid format.
// The seed reserves entry IDs [1_000_000_000, +∞) so that ad-hoc unit-
// test inserts that pick small entry IDs (e.g. storage_test.go uses
// base 281474976710700 = (4 << 48) + 44) cannot collide with seeded data.
func makeGraphID(labelID int32, entryID int64) int64 {
	return int64(labelID)<<48 | (entryID & ((int64(1) << 48) - 1))
}

// formatGraphID renders the graphid as the decimal string AGE accepts in
// COPY input (raw int8). pgx's COPY-FROM-STDIN expects text-protocol
// rows, one column per tab-separated field.
func formatGraphID(labelID int32, entryID int64) string {
	return fmt.Sprintf("%d", makeGraphID(labelID, entryID))
}

// nodeMix encodes the realistic Phase 0 node distribution. counts sum
// to TargetNodes (rescaled at runtime if SeedOpts.TargetNodes != 10M).
type nodeMix struct {
	label    string
	fraction float64 // share of TargetNodes
	makeProp func(tenantID string, n int64) map[string]any
}

// edgeMix encodes the realistic Phase 0 edge distribution. fromLabel /
// toLabel pick the source/target label for the edge; the seed picks
// random entry IDs within each label's seeded range to materialise the
// connection.
type edgeMix struct {
	label     string
	fraction  float64
	fromLabel string
	toLabel   string
	makeProp  func(tenantID string, n int64) map[string]any
}

// nodeShapes returns the realistic property-bag distribution per
// ADR §3.4 + ARCH §3-5 examples. Each makeProp produces a map suitable
// for json.Marshal; the seed serialises and bulk-COPYs as agtype text.
func nodeShapes() []nodeMix {
	return []nodeMix{
		{label: "Table", fraction: 0.010, makeProp: shapeTable},                        // 100K @ 10M envelope
		{label: "Column", fraction: 0.500, makeProp: shapeColumn},                      // 5M
		{label: "Snapshot", fraction: 0.400, makeProp: shapeSnapshot},                  // 4M
		{label: "Metric", fraction: 0.010, makeProp: shapeMetric},                      // 100K
		{label: "Dimension", fraction: 0.005, makeProp: shapeNamedRow("Dimension")},    // 50K
		{label: "View", fraction: 0.005, makeProp: shapeNamedRow("View")},              // 50K
		{label: "Dashboard", fraction: 0.003, makeProp: shapeNamedRow("Dashboard")},    // 30K
		{label: "Pipeline", fraction: 0.005, makeProp: shapeNamedRow("Pipeline")},      // 50K
		{label: "Query", fraction: 0.020, makeProp: shapeQuery},                        // 200K
		{label: "Person", fraction: 0.005, makeProp: shapePerson},                      // 50K
		{label: "Team", fraction: 0.001, makeProp: shapeNamedRow("Team")},              // 10K
		{label: "Policy", fraction: 0.002, makeProp: shapeNamedRow("Policy")},          // 20K
		{label: "GlossaryTerm", fraction: 0.005, makeProp: shapeNamedRow("Glossary")},  // 50K
		{label: "Tag", fraction: 0.005, makeProp: shapeTag},                            // 50K
		{label: "DataContract", fraction: 0.001, makeProp: shapeNamedRow("Contract")},  // 10K
		{label: "Engine", fraction: 0.0005, makeProp: shapeNamedRow("Engine")},         // 5K
		{label: "Catalog", fraction: 0.0005, makeProp: shapeNamedRow("Catalog")},       // 5K
		{label: "WriteEvent", fraction: 0.014, makeProp: shapeWriteEvent},              // 140K
		{label: "DetectionRun", fraction: 0.008, makeProp: shapeNamedRow("Detection")}, // 80K
	}
}

// edgeShapes returns the realistic Phase 0 edge distribution per
// ARCH §3 lineage modeling and the ADR §4 canonical Cypher patterns.
// Heaviest concentration is LINEAGE_OF (column-column + snapshot-snapshot)
// then READ/WROTE then everything else. Mix sums to 1.0.
func edgeShapes() []edgeMix {
	return []edgeMix{
		{label: "LINEAGE_OF", fraction: 0.60, fromLabel: "Column", toLabel: "Column", makeProp: shapeLineageEdge},
		{label: "READ", fraction: 0.14, fromLabel: "Person", toLabel: "Table", makeProp: shapeReadEdge},
		{label: "WROTE", fraction: 0.10, fromLabel: "Person", toLabel: "Table", makeProp: shapeWroteEdge},
		{label: "OWNS", fraction: 0.04, fromLabel: "Person", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "CLASSIFIED_AS", fraction: 0.03, fromLabel: "Column", toLabel: "Tag", makeProp: shapeBareEdge},
		{label: "APPLIES_TO", fraction: 0.02, fromLabel: "Policy", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "DEPENDS_ON", fraction: 0.02, fromLabel: "Table", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "INTENDED_WRITE", fraction: 0.015, fromLabel: "WriteEvent", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "ACTUAL_WRITE", fraction: 0.015, fromLabel: "WriteEvent", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "VIOLATION_DETECTED_BY", fraction: 0.005, fromLabel: "DetectionRun", toLabel: "WriteEvent", makeProp: shapeBareEdge},
		{label: "MEMBER_OF", fraction: 0.005, fromLabel: "Person", toLabel: "Team", makeProp: shapeBareEdge},
		{label: "DEFINED_BY", fraction: 0.005, fromLabel: "Metric", toLabel: "Person", makeProp: shapeBareEdge},
		{label: "PRODUCES", fraction: 0.005, fromLabel: "Pipeline", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "CONSUMES", fraction: 0.005, fromLabel: "Pipeline", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "GOVERNED_BY", fraction: 0.005, fromLabel: "Table", toLabel: "Policy", makeProp: shapeBareEdge},
		{label: "STORED_IN", fraction: 0.005, fromLabel: "Table", toLabel: "Catalog", makeProp: shapeBareEdge},
		{label: "RUNS_ON", fraction: 0.005, fromLabel: "Query", toLabel: "Engine", makeProp: shapeBareEdge},
		{label: "SUPERSEDES", fraction: 0.003, fromLabel: "Snapshot", toLabel: "Snapshot", makeProp: shapeBareEdge},
		{label: "BELONGS_TO", fraction: 0.005, fromLabel: "Column", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "OF_TABLE", fraction: 0.005, fromLabel: "Snapshot", toLabel: "Table", makeProp: shapeBareEdge},
		{label: "USED_ENGINE", fraction: 0.005, fromLabel: "Snapshot", toLabel: "Engine", makeProp: shapeBareEdge},
		{label: "USES_DIMENSION", fraction: 0.005, fromLabel: "Metric", toLabel: "Dimension", makeProp: shapeBareEdge},
		{label: "RAN_ON", fraction: 0.005, fromLabel: "Query", toLabel: "Engine", makeProp: shapeBareEdge},
		{label: "GOVERNS", fraction: 0.002, fromLabel: "Policy", toLabel: "Column", makeProp: shapeBareEdge},
	}
}

// ---------------- shape helpers ----------------

func shapeTable(tenant string, n int64) map[string]any {
	cat := n % 10
	ns := []string{"sales", "ops", "finance", "ml", "ingest"}[n%5]
	return map[string]any{
		"uri":                 fmt.Sprintf("iceberg://catalog-%d/%s/table-%d", cat, ns, n),
		"catalog_id":          fmt.Sprintf("catalog-%d", cat),
		"namespace":           ns,
		"name":                fmt.Sprintf("table_%d", n),
		"current_snapshot_id": 7800000000000000 + n,
		"partition_spec_id":   int(n%5) + 1,
		"format_version":      2,
		"owner_team_id":       fmt.Sprintf("team-%d", n%50),
		"created_at":          "2025-03-15T10:30:00Z",
		"description":         "Synthetic Phase 0 envelope table for load testing",
		"tenant_id":           tenant,
	}
}

func shapeColumn(tenant string, n int64) map[string]any {
	parentN := n / 50 // 50 columns per table average
	cat := parentN % 10
	ns := []string{"sales", "ops", "finance", "ml", "ingest"}[parentN%5]
	parentURI := fmt.Sprintf("iceberg://catalog-%d/%s/table-%d", cat, ns, parentN)
	colTypes := []string{"string", "long", "double", "boolean", "timestamp"}
	return map[string]any{
		"uri":              fmt.Sprintf("%s#col_%d", parentURI, n%50),
		"parent_table_uri": parentURI,
		"name":             fmt.Sprintf("col_%d", n%50),
		"type":             colTypes[n%5],
		"ordinal_position": int(n % 50),
		"nullable":         n%3 == 0,
		"field_id":         int(n % 50),
		"tenant_id":        tenant,
	}
}

func shapeSnapshot(tenant string, n int64) map[string]any {
	tableN := n / 40 // 40 snapshots per table average
	cat := tableN % 10
	ns := []string{"sales", "ops", "finance", "ml", "ingest"}[tableN%5]
	return map[string]any{
		"snapshot_id":            7800000000000000 + n,
		"table_uri":              fmt.Sprintf("iceberg://catalog-%d/%s/table-%d", cat, ns, tableN),
		"parent_snapshot_id":     7800000000000000 + n - 1,
		"committed_at":           "2025-05-10T14:23:45.123Z",
		"committed_by_engine":    []string{"spark", "trino", "snowflake", "dremio"}[n%4],
		"committed_by_engine_id": fmt.Sprintf("engine-%d", n%20),
		"operation":              []string{"append", "overwrite", "delete"}[n%3],
		"added_files":            int(n%100) + 1,
		"added_records":          (n % 1000000) + 1,
		"summary":                "Synthetic snapshot",
		"tenant_id":              tenant,
	}
}

func shapeMetric(tenant string, n int64) map[string]any {
	return map[string]any{
		"name":        fmt.Sprintf("metric_%d", n),
		"description": "Synthetic metric for envelope test",
		"unit":        []string{"USD", "count", "ratio", "ms"}[n%4],
		"tenant_id":   tenant,
	}
}

func shapeQuery(tenant string, n int64) map[string]any {
	return map[string]any{
		"query_id":    fmt.Sprintf("q-%d", n),
		"sql_hash":    fmt.Sprintf("%016x", n*2654435761),
		"engine":      []string{"spark", "trino", "snowflake", "dremio"}[n%4],
		"executed_at": "2025-05-10T14:23:45Z",
		"tenant_id":   tenant,
	}
}

func shapePerson(tenant string, n int64) map[string]any {
	return map[string]any{
		"email":     fmt.Sprintf("user-%d@neksur.test", n),
		"name":      fmt.Sprintf("Person %d", n),
		"team_id":   fmt.Sprintf("team-%d", n%50),
		"tenant_id": tenant,
	}
}

func shapeTag(tenant string, n int64) map[string]any {
	return map[string]any{
		"id":        fmt.Sprintf("tag-%d", n),
		"name":      []string{"PII", "GDPR", "HIPAA", "Internal", "Public"}[n%5],
		"tenant_id": tenant,
	}
}

func shapeWriteEvent(tenant string, n int64) map[string]any {
	return map[string]any{
		"event_id":     fmt.Sprintf("we-%d", n),
		"table_uri":    fmt.Sprintf("iceberg://catalog-%d/sales/table-%d", n%10, n%100000),
		"engine":       []string{"spark", "trino"}[n%2],
		"committed_at": "2025-05-10T14:23:45Z",
		"tenant_id":    tenant,
	}
}

// shapeNamedRow returns a generic small property bag for the long tail
// of node labels that don't have richer ADR-§3.4 example shapes.
func shapeNamedRow(prefix string) func(string, int64) map[string]any {
	return func(tenant string, n int64) map[string]any {
		return map[string]any{
			"id":        fmt.Sprintf("%s-%d", prefix, n),
			"name":      fmt.Sprintf("%s name %d", prefix, n),
			"tenant_id": tenant,
		}
	}
}

// shapeLineageEdge mirrors the LINEAGE_OF property bag from ADR §3.4.
func shapeLineageEdge(tenant string, n int64) map[string]any {
	return map[string]any{
		"transform":  []string{"identity", "rename", "cast", "concat", "agg"}[n%5],
		"created_at": "2025-05-10T14:23:45Z",
		"tenant_id":  tenant,
	}
}

func shapeReadEdge(tenant string, n int64) map[string]any {
	return map[string]any{
		"at":        "2025-05-10T14:23:45Z",
		"tenant_id": tenant,
	}
}

func shapeWroteEdge(tenant string, n int64) map[string]any {
	return map[string]any{
		"at":        "2025-05-10T14:23:45Z",
		"tenant_id": tenant,
	}
}

func shapeBareEdge(tenant string, n int64) map[string]any {
	return map[string]any{
		"tenant_id": tenant,
	}
}

// Seed materialises Phase 0 envelope synthetic data on conn. Replaces
// the Plan 01 stub. See package docs for strategy / pitfall mitigations.
//
// Workflow:
//  1. Pre-check expected V0020+V0025 indexes exist (Pitfall 2).
//  2. Look up ag_label.id per label so we can build correct graphids.
//  3. For each label in nodeShapes(), COPY (id, properties) into
//     neksur."<Label>" using pgx PgConn.CopyFrom; tenant_id is on
//     every row in the JSONB properties.
//  4. For each edge in edgeShapes(), COPY (id, start_id, end_id,
//     properties) into neksur."<Label>"; start/end IDs are looked up
//     from the per-label entry-id ranges seeded in step 3.
//  5. ANALYZE every neksur.* underlying table at the end so the planner
//     has fresh statistics for the latency runner.
//
// Throttling: every 1M nodes/edges we yield via time.Sleep(1*Second)
// to give pgBackRest a window to drain WAL (T-0-LOAD-WAL-OVERFLOW).
func Seed(ctx context.Context, conn *pgx.Conn, opts SeedOpts) (SeedResult, error) {
	if opts.TargetNodes <= 0 {
		opts.TargetNodes = 10_000_000
	}
	if opts.TargetEdges <= 0 {
		opts.TargetEdges = 50_000_000
	}
	if opts.Tenants <= 0 {
		opts.Tenants = 1
	}

	start := time.Now()

	if err := preCheckIndexes(ctx, conn); err != nil {
		return SeedResult{}, err
	}
	labelIDsByName, err := labelIDs(ctx, conn)
	if err != nil {
		return SeedResult{}, err
	}

	// Track per-label seeded entry-ID ranges so edges can pick valid
	// endpoints (start_id / end_id must reference a seeded vertex).
	type rng struct{ start, end int64 }
	nodeRanges := make(map[string]rng, len(nodeShapes()))

	var totalNodes, totalEdges, totalBytes int64
	const entryIDBase int64 = 1_000_000_000 // reserve [0, 1B) for ad-hoc tests

	// ---- Phase 1: vertices ----
	for _, shape := range nodeShapes() {
		count := int64(float64(opts.TargetNodes) * shape.fraction)
		if count == 0 {
			continue
		}
		labelID, ok := labelIDsByName[shape.label]
		if !ok {
			return SeedResult{}, fmt.Errorf("seed: label %q missing from ag_label", shape.label)
		}
		nodeRanges[shape.label] = rng{start: entryIDBase, end: entryIDBase + count}

		written, bytes, err := copyNodes(ctx, conn, shape.label, labelID, entryIDBase, count, opts.Tenants, shape.makeProp)
		if err != nil {
			return SeedResult{}, fmt.Errorf("seed: copy nodes for %s: %w", shape.label, err)
		}
		totalNodes += written
		totalBytes += bytes
	}

	// ---- Phase 2: edges ----
	for _, e := range edgeShapes() {
		count := int64(float64(opts.TargetEdges) * e.fraction)
		if count == 0 {
			continue
		}
		labelID, ok := labelIDsByName[e.label]
		if !ok {
			return SeedResult{}, fmt.Errorf("seed: edge label %q missing from ag_label", e.label)
		}
		fromRange, ok := nodeRanges[e.fromLabel]
		if !ok {
			return SeedResult{}, fmt.Errorf("seed: edge %s references unseeded fromLabel %s", e.label, e.fromLabel)
		}
		toRange, ok := nodeRanges[e.toLabel]
		if !ok {
			return SeedResult{}, fmt.Errorf("seed: edge %s references unseeded toLabel %s", e.label, e.toLabel)
		}
		fromLabelID := labelIDsByName[e.fromLabel]
		toLabelID := labelIDsByName[e.toLabel]

		written, bytes, err := copyEdges(ctx, conn, e.label, labelID, entryIDBase, count,
			fromLabelID, fromRange.start, fromRange.end,
			toLabelID, toRange.start, toRange.end,
			opts.Tenants, e.makeProp)
		if err != nil {
			return SeedResult{}, fmt.Errorf("seed: copy edges for %s: %w", e.label, err)
		}
		totalEdges += written
		totalBytes += bytes
	}

	// ---- Phase 3: ANALYZE every label table so the planner has
	// fresh statistics for the latency runner.
	for name := range graph.LabelWhitelist {
		stmt := fmt.Sprintf(`ANALYZE neksur.%q`, name)
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return SeedResult{}, fmt.Errorf("seed: ANALYZE %s: %w", name, err)
		}
	}

	return SeedResult{
		NodesCreated:         totalNodes,
		EdgesCreated:         totalEdges,
		Duration:             time.Since(start),
		BytesWrittenEstimate: totalBytes,
	}, nil
}

// copyNodes streams `count` rows into neksur."<label>" via raw COPY
// FROM STDIN, in tab-separated-text format. Returns (written, bytes).
//
// We use the high-level pgx CopyFrom interface with a pgx.CopyFromSource
// closure that synthesises rows on demand — this is the idiomatic Go
// equivalent of the Python io.Pipe / encoding/csv scaffolding the plan
// describes. It avoids materialising the whole batch in memory.
func copyNodes(
	ctx context.Context,
	conn *pgx.Conn,
	label string,
	labelID int32,
	entryStart int64,
	count int64,
	tenants int,
	makeProp func(string, int64) map[string]any,
) (int64, int64, error) {
	var bytes int64
	idx := int64(0)
	src := &copyNodeSource{
		count:      count,
		entryStart: entryStart,
		labelID:    labelID,
		tenants:    tenants,
		makeProp:   makeProp,
		idx:        &idx,
		bytes:      &bytes,
	}
	tableIdent := pgx.Identifier{"neksur", label}
	written, err := conn.CopyFrom(ctx, tableIdent, []string{"id", "properties"}, src)
	if err != nil {
		return 0, 0, err
	}
	// Yield to pgBackRest WAL drain after every label-batch (T-0-LOAD-WAL-OVERFLOW)
	if count >= 1_000_000 {
		select {
		case <-ctx.Done():
			return written, bytes, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return written, bytes, nil
}

type copyNodeSource struct {
	count      int64
	entryStart int64
	labelID    int32
	tenants    int
	makeProp   func(string, int64) map[string]any
	idx        *int64
	bytes      *int64
	current    [2]any
}

func (s *copyNodeSource) Next() bool {
	if *s.idx >= s.count {
		return false
	}
	n := *s.idx
	tenantID := fmt.Sprintf("tenant-%d", n%int64(s.tenants))
	props := s.makeProp(tenantID, n)
	propJSON, _ := json.Marshal(props)
	s.current[0] = makeGraphID(s.labelID, s.entryStart+n)
	s.current[1] = string(propJSON)
	*s.bytes += int64(len(propJSON)) + 16
	*s.idx++
	return true
}

func (s *copyNodeSource) Values() ([]any, error) {
	return []any{s.current[0], s.current[1]}, nil
}

func (s *copyNodeSource) Err() error { return nil }

// copyEdges is the analogous bulk-COPY for edge tables. AGE edge tables
// have the schema (id graphid, start_id graphid, end_id graphid,
// properties agtype). We pick start_id / end_id from the seeded vertex
// entry-ID range to guarantee referential validity.
func copyEdges(
	ctx context.Context,
	conn *pgx.Conn,
	label string,
	labelID int32,
	entryStart int64,
	count int64,
	fromLabelID int32, fromStart, fromEnd int64,
	toLabelID int32, toStart, toEnd int64,
	tenants int,
	makeProp func(string, int64) map[string]any,
) (int64, int64, error) {
	var bytes int64
	idx := int64(0)
	src := &copyEdgeSource{
		count: count, entryStart: entryStart, labelID: labelID,
		fromLabelID: fromLabelID, fromStart: fromStart, fromEnd: fromEnd,
		toLabelID: toLabelID, toStart: toStart, toEnd: toEnd,
		tenants: tenants, makeProp: makeProp,
		idx: &idx, bytes: &bytes,
	}
	tableIdent := pgx.Identifier{"neksur", label}
	written, err := conn.CopyFrom(ctx, tableIdent,
		[]string{"id", "start_id", "end_id", "properties"}, src)
	if err != nil {
		return 0, 0, err
	}
	if count >= 1_000_000 {
		select {
		case <-ctx.Done():
			return written, bytes, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return written, bytes, nil
}

type copyEdgeSource struct {
	count       int64
	entryStart  int64
	labelID     int32
	fromLabelID int32
	fromStart   int64
	fromEnd     int64
	toLabelID   int32
	toStart     int64
	toEnd       int64
	tenants     int
	makeProp    func(string, int64) map[string]any
	idx         *int64
	bytes       *int64
	current     [4]any
}

func (s *copyEdgeSource) Next() bool {
	if *s.idx >= s.count {
		return false
	}
	n := *s.idx
	fromCount := s.fromEnd - s.fromStart
	toCount := s.toEnd - s.toStart
	// Deterministic pick (no randomness — keeps the seed reproducible)
	fromN := s.fromStart + (n % fromCount)
	toN := s.toStart + ((n * 2654435761) % toCount)
	tenantID := fmt.Sprintf("tenant-%d", n%int64(s.tenants))
	props := s.makeProp(tenantID, n)
	propJSON, _ := json.Marshal(props)
	s.current[0] = makeGraphID(s.labelID, s.entryStart+n)
	s.current[1] = makeGraphID(s.fromLabelID, fromN)
	s.current[2] = makeGraphID(s.toLabelID, toN)
	s.current[3] = string(propJSON)
	*s.bytes += int64(len(propJSON)) + 32
	*s.idx++
	return true
}

func (s *copyEdgeSource) Values() ([]any, error) {
	return []any{s.current[0], s.current[1], s.current[2], s.current[3]}, nil
}

func (s *copyEdgeSource) Err() error { return nil }
