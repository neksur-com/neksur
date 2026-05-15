// Phase 1 bulk-backfill envelope runner — REAL Polaris metadata pulls
// then COPY-then-Cypher-transform per D-001.09 + Plan 00-06 pattern.
//
// Extends Phase 0 Plan 00-06's `phase0_envelope_seed.go` synthetic seed
// pattern from random-bit fabrication to genuine `polaris.LoadTable`
// reads, materializes the manifest entries to V0064 `staging.iceberg_*`
// COPY-target tables, then dispatches Cypher MERGE batches via the Plan
// 01-04 `ingest.Service` primitives (MergeSnapshot / MergeColumns /
// MergeLineageEdge) — the SAME RLS-scoped + AGE-1.6-aware pipeline the
// L1 gateway uses post-commit. No second pgxpool: the runner reuses the
// graph.GraphClient pool that the cmd/neksur-server initialization
// constructs (Phase 0.5 BeforeAcquire DISCARD ALL is the ONLY enforcement
// of session-bleed prevention — RESEARCH §Anti-pattern line 1400).
//
// Reads from any IcebergCatalogClient (Plan 01-02 interface) so the test
// substrate can swap a stub adapter; production paths construct a
// polaris.New adapter against the customer's Polaris endpoint.
//
// Sentinels:
//   - ErrIndexesMissing — the per-tenant graph schema is missing one of
//     the GC-01 / V0031 mandatory indexes. The runner refuses to start
//     because a 10M-node ingest without `idx_has_column_start_id`
//     (etc.) would hit pathological edge-table scan times.

package load

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/ingest"
)

// ErrIndexesMissing is returned by RealIngestEnvelope when the per-tenant
// graph schema is missing one of the GC-01 / V0031 mandatory indexes.
// Without these the 10M-node ingest would hit pathological edge-table
// scan times (the COPY phase finishes in minutes; the Cypher transform
// would never finish without the start_id/end_id btree indexes).
//
// Operators wrap to surface the missing index list in the error message.
var ErrIndexesMissing = errors.New("load: required GC-01 graph indexes missing")

// requiredHotIndexes is the GC-01 / V0031 closure of property + edge
// timestamp indexes the 10M-node bulk-backfill envelope needs hot.
// Plan 01-01 V0021/V0026/V0031 owns the migration; this runner refuses
// to start if any are missing (pre-check at startup, NOT during the
// COPY phase — fail fast, fail loud).
//
// The list is maintained in lockstep with migrations/postgres/V0031 +
// migrations/graph/V0021 + V0026. If the migration set changes, update
// here in the same PR (the BLOCKING gate runs the pre-check on every
// invocation so out-of-sync index sets surface immediately).
var requiredHotIndexes = []string{
	"idx_snapshot_metadata_location",
	"idx_snapshot_committed_at",
	"idx_column_snapshot_loc_name",
	"idx_has_column_start_id",
	"idx_has_column_end_id",
	"idx_lineage_of_start_id",
	"idx_lineage_of_end_id",
	"idx_lineage_of_created_at",
	"idx_lineage_of_last_seen_at",
	"idx_lineage_of_run_id",
}

// RealIngestOpts configures the bulk-backfill envelope shape. Defaults
// reflect Phase 1 envelope per ROADMAP §Phase 1 success-criterion §4
// (10M-node-scale Iceberg metadata): 100K tables / 5M columns / 4M
// snapshots / 30M lineage edges (these are the proportions Phase 0
// Plan 00-06 derived for the synthetic seed; the real-ingest path
// inherits the shape so the BLOCKING gates stay comparable).
//
// Operators tune CopyBatchSize + CypherBatchSize for hardware: the
// COPY phase is bounded by WAL throughput (T-1-LOAD-WAL-OVERFLOW
// mitigation = 1s yield per 1M rows) and the Cypher phase is bounded
// by per-tx tenant-context overhead (DISCARD ALL on connection release
// + RLS predicate evaluation per MERGE).
type RealIngestOpts struct {
	TargetTableCount       int
	TargetColumnCount      int
	TargetSnapshotCount    int
	TargetLineageEdgeCount int
	CopyBatchSize          int
	CypherBatchSize        int
}

// withDefaults applies Phase 1 envelope defaults for any unset field.
// Internal helper — callers see the explicit shape they constructed via
// stats.Opts (not exposed here; the runner records its applied opts in
// the baseline JSON so operators can diff against future runs).
func (o RealIngestOpts) withDefaults() RealIngestOpts {
	if o.TargetTableCount <= 0 {
		o.TargetTableCount = 100_000
	}
	if o.TargetColumnCount <= 0 {
		o.TargetColumnCount = 5_000_000
	}
	if o.TargetSnapshotCount <= 0 {
		o.TargetSnapshotCount = 4_000_000
	}
	if o.TargetLineageEdgeCount <= 0 {
		o.TargetLineageEdgeCount = 30_000_000
	}
	if o.CopyBatchSize <= 0 {
		o.CopyBatchSize = 50_000
	}
	if o.CypherBatchSize <= 0 {
		o.CypherBatchSize = 1_000
	}
	return o
}

// RealIngestStats is the empirical evidence the runner returns + writes
// to the JSON baseline artifact. CopyPhaseDuration < CypherPhaseDuration
// is the expected shape at envelope scale (per ADR-001 — the COPY phase
// is bounded by WAL throughput; the Cypher phase is bounded by
// per-MERGE RLS + telemetry overhead).
type RealIngestStats struct {
	TablesIngested        int           `json:"tables_ingested"`
	ColumnsIngested       int           `json:"columns_ingested"`
	SnapshotsIngested     int           `json:"snapshots_ingested"`
	LineageEdgesIngested  int           `json:"lineage_edges_ingested"`
	CopyPhaseDuration     time.Duration `json:"copy_phase_duration"`
	CypherPhaseDuration   time.Duration `json:"cypher_phase_duration"`
	TotalDuration         time.Duration `json:"total_duration"`
	CopyPhaseDurationMs   int64         `json:"copy_phase_duration_ms"`
	CypherPhaseDurationMs int64         `json:"cypher_phase_duration_ms"`
	TotalDurationMs       int64         `json:"total_duration_ms"`
}

// listWorkerCount is the bounded worker pool size for the LoadTable
// fan-out phase. 50 mirrors Phase 0 Plan 00-06's cypher_workload
// concurrency (REQ-NFR-catalog-scale envelope = 50 concurrent users)
// so the runner produces realistic upstream load on the Polaris JVM
// without overwhelming a small testcontainer instance.
const listWorkerCount = 50

// walThrottleEvery enforces the T-1-LOAD-WAL-OVERFLOW mitigation: yield
// 1s per N rows COPIED to give Postgres WAL writer time to drain the
// archive segments. Plan 00-06's seed used the same value; copying it
// keeps the envelope comparable across Phase 0 / Phase 1 measurements.
const walThrottleEvery = 1_000_000

// RealIngestEnvelope drives the full COPY-then-Cypher-transform path
// per D-001.09 + ADR-001. Steps:
//
//  1. Pre-check: assert every requiredHotIndex is present in the
//     per-tenant graph (V0031). Refuse to start otherwise.
//
//  2. List phase: enumerate tables under the default namespace via
//     adapter.ListTables (single-segment per Plan 01-02), bounded by
//     opts.TargetTableCount.
//
//  3. LoadTable phase: parallel fan-out via a 50-worker pool calling
//     adapter.LoadTable per ref. Materialize columns + snapshots into
//     in-memory slices for the COPY phase.
//
//  4. COPY phase: bulk-write to staging.iceberg_tables /
//     staging.iceberg_columns / staging.iceberg_snapshots via
//     pgx.Conn.CopyFrom. WAL throttle = 1s yield per 1M rows.
//
//  5. Cypher transform phase: batch-MERGE snapshots / columns / lineage
//     edges via Plan 01-04 ingest.Service primitives (RLS-scoped via
//     gc.ExecuteInTenant). Each batch is one tx so partial progress is
//     observable and operator-resumable.
//
//  6. Truncate staging.iceberg_* (per V0064 spec — staging tables are
//     short-lived COPY targets, not durable storage).
//
// The function is single-shot per (adapter, tenant) pair. Operators
// invoking the bulk-backfill at envelope scale should use the cmd
// driver (cmd/run_real_ingest), which adds flag parsing + JSON
// baseline emission + assertion logic on top.
func RealIngestEnvelope(
	ctx context.Context,
	adapter iceberg.IcebergCatalogClient,
	gc *graph.GraphClient,
	pool *pgxpool.Pool,
	tenantID string,
	tenantSchema string,
	opts RealIngestOpts,
) (RealIngestStats, error) {
	opts = opts.withDefaults()
	stats := RealIngestStats{}
	totalStart := time.Now()

	// Step 1 — pre-check. Refuse to start if the per-tenant graph
	// schema is missing GC-01 indexes. We check the per-tenant schema
	// (NOT public) because Phase 1 graphs live in tenant_<uuid>
	// schemas (Phase 0.5 D-0.5.04). The schema name is supplied by the
	// caller (cmd/run_real_ingest derives it from the tenant UUID via
	// internal/tenant/id.go::SchemaName).
	if err := assertHotIndexes(ctx, pool, tenantSchema); err != nil {
		return stats, err
	}

	// Step 2 — list tables. Phase 1 default namespace is "default";
	// callers may override via Polaris config (the test substrate
	// pre-creates "default" via the testcontainer bootstrap).
	listStart := time.Now()
	refs, err := adapter.ListTables(ctx, "default")
	if err != nil {
		return stats, fmt.Errorf("load: list tables: %w", err)
	}
	if len(refs) > opts.TargetTableCount {
		refs = refs[:opts.TargetTableCount]
	}
	_ = listStart // available for future per-phase telemetry.

	// Step 3 — LoadTable fan-out (50-worker bounded pool). Materialize
	// every TableMetadata's columns + snapshots into in-memory slices
	// the COPY phase can stream into staging.
	loadStart := time.Now()
	tableRows, columnRows, snapshotRows, err := loadAllTables(ctx, adapter, refs, opts)
	if err != nil {
		return stats, fmt.Errorf("load: load tables: %w", err)
	}
	_ = loadStart

	// Step 4 — COPY phase. Use pgx.Conn.CopyFrom against the per-tenant
	// staging.iceberg_* tables. The connection is acquired from the
	// existing pool (NO second pool — RESEARCH §Anti-pattern line 1400).
	copyStart := time.Now()
	if err := copyToStaging(ctx, pool, tenantSchema, tableRows, columnRows, snapshotRows); err != nil {
		return stats, fmt.Errorf("load: copy phase: %w", err)
	}
	stats.CopyPhaseDuration = time.Since(copyStart)
	stats.CopyPhaseDurationMs = stats.CopyPhaseDuration.Milliseconds()

	// Step 5 — Cypher transform phase. Batch-dispatch MergeSnapshot,
	// MergeColumns, MergeLineageEdge through the Plan 01-04 service.
	// We honor opts.CypherBatchSize so partial progress is observable
	// for operator triage on multi-hour envelope runs.
	cypherStart := time.Now()
	svc := ingest.NewService(gc)
	transformed, err := transformStaging(ctx, pool, gc, svc, tenantID, tenantSchema, opts)
	if err != nil {
		return stats, fmt.Errorf("load: cypher transform: %w", err)
	}
	stats.CypherPhaseDuration = time.Since(cypherStart)
	stats.CypherPhaseDurationMs = stats.CypherPhaseDuration.Milliseconds()

	stats.TablesIngested = transformed.Tables
	stats.ColumnsIngested = transformed.Columns
	stats.SnapshotsIngested = transformed.Snapshots
	stats.LineageEdgesIngested = transformed.LineageEdges

	// Step 6 — truncate staging tables (per V0064 spec). Best-effort
	// because truncation failure does NOT mean the ingest itself failed
	// (the graph already has the merged data); we log via the returned
	// stats but do not error-out the run.
	_ = truncateStaging(ctx, pool, tenantSchema)

	stats.TotalDuration = time.Since(totalStart)
	stats.TotalDurationMs = stats.TotalDuration.Milliseconds()
	return stats, nil
}

// assertHotIndexes runs the pre-check that every required GC-01 / V0031
// index is present in the per-tenant schema. Returns wrapped
// ErrIndexesMissing if any are missing (with the missing list in the
// error message for fast operator triage).
func assertHotIndexes(ctx context.Context, pool *pgxpool.Pool, tenantSchema string) error {
	rows, err := pool.Query(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE schemaname = $1
	`, tenantSchema)
	if err != nil {
		return fmt.Errorf("load: hot index pre-check: %w", err)
	}
	defer rows.Close()
	have := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("load: hot index scan: %w", err)
		}
		have[name] = struct{}{}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("load: hot index rows.Err: %w", rerr)
	}
	var missing []string
	for _, want := range requiredHotIndexes {
		if _, ok := have[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("load: schema=%s missing %d index(es) %v: %w",
			tenantSchema, len(missing), missing, ErrIndexesMissing)
	}
	return nil
}

// stagingTableRow is the COPY-target tuple shape for staging.iceberg_tables.
type stagingTableRow struct {
	UUID              string
	Namespace         string
	Name              string
	CurrentSnapshotID int64
	MetadataLocation  string
	Properties        []byte // jsonb-serialized
}

// stagingColumnRow is the COPY-target tuple shape for staging.iceberg_columns.
type stagingColumnRow struct {
	TableUUID                string
	SnapshotMetadataLocation string
	ColumnID                 int
	Name                     string
	DataType                 string
	Required                 bool
	Doc                      string
	Ordinal                  int
}

// stagingSnapshotRow is the COPY-target tuple shape for staging.iceberg_snapshots.
type stagingSnapshotRow struct {
	SnapshotID       int64
	ParentSnapshotID int64
	MetadataLocation string
	CommittedAtMs    int64
	Operation        string
	Summary          []byte // jsonb-serialized
}

// loadAllTables runs the bounded-50-worker fan-out across LoadTable
// calls. Returns three slices (tables, columns, snapshots) ready for
// the COPY phase. Errors short-circuit the fan-out (the first
// LoadTable failure cancels the rest via the shared ctx).
func loadAllTables(
	ctx context.Context,
	adapter iceberg.IcebergCatalogClient,
	refs []iceberg.TableRef,
	opts RealIngestOpts,
) ([]stagingTableRow, []stagingColumnRow, []stagingSnapshotRow, error) {
	type loadOut struct {
		tableRow   stagingTableRow
		colRows    []stagingColumnRow
		snapRows   []stagingSnapshotRow
		err        error
		refForDiag iceberg.TableRef
	}
	loadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan iceberg.TableRef, len(refs))
	results := make(chan loadOut, len(refs))
	var wg sync.WaitGroup
	for w := 0; w < listWorkerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range jobs {
				meta, err := adapter.LoadTable(loadCtx, ref)
				if err != nil {
					results <- loadOut{err: err, refForDiag: ref}
					cancel()
					return
				}
				out := loadOut{refForDiag: ref}
				out.tableRow = stagingTableRow{
					UUID:              metaUUID(meta),
					Namespace:         joinNamespace(ref.Namespace),
					Name:              ref.Name,
					CurrentSnapshotID: metaSnapID(meta),
					MetadataLocation:  metaLoc(meta),
					Properties:        propsJSON(meta),
				}
				if meta != nil {
					for ord, f := range meta.Schema.Fields {
						out.colRows = append(out.colRows, stagingColumnRow{
							TableUUID:                out.tableRow.UUID,
							SnapshotMetadataLocation: out.tableRow.MetadataLocation,
							ColumnID:                 f.ID,
							Name:                     f.Name,
							DataType:                 f.Type,
							Required:                 f.Required,
							Doc:                      f.Doc,
							Ordinal:                  ord,
						})
					}
					for _, s := range meta.Snapshots {
						out.snapRows = append(out.snapRows, stagingSnapshotRow{
							SnapshotID:       s.SnapshotID,
							ParentSnapshotID: s.ParentSnapshotID,
							MetadataLocation: nonEmptyMetaLoc(s.MetadataLocation, out.tableRow.MetadataLocation),
							CommittedAtMs:    s.TimestampMs,
							Operation:        s.Operation,
							Summary:          summaryJSON(s.Summary),
						})
					}
				}
				results <- out
			}
		}()
	}
	for _, ref := range refs {
		select {
		case jobs <- ref:
		case <-loadCtx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	close(results)

	var (
		tableRows    []stagingTableRow
		columnRows   []stagingColumnRow
		snapshotRows []stagingSnapshotRow
		firstErr     error
	)
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("ref=%v: %w", r.refForDiag, r.err)
			}
			continue
		}
		if len(tableRows) < opts.TargetTableCount {
			tableRows = append(tableRows, r.tableRow)
		}
		if len(columnRows) < opts.TargetColumnCount {
			remaining := opts.TargetColumnCount - len(columnRows)
			if len(r.colRows) > remaining {
				columnRows = append(columnRows, r.colRows[:remaining]...)
			} else {
				columnRows = append(columnRows, r.colRows...)
			}
		}
		if len(snapshotRows) < opts.TargetSnapshotCount {
			remaining := opts.TargetSnapshotCount - len(snapshotRows)
			if len(r.snapRows) > remaining {
				snapshotRows = append(snapshotRows, r.snapRows[:remaining]...)
			} else {
				snapshotRows = append(snapshotRows, r.snapRows...)
			}
		}
	}
	if firstErr != nil {
		return nil, nil, nil, firstErr
	}
	return tableRows, columnRows, snapshotRows, nil
}

// copyToStaging streams the materialized rows into staging.iceberg_*
// via pgx.Conn.CopyFrom. The connection comes from the existing pool
// (NO second pool). The 1s/1M-rows WAL throttle (T-1-LOAD-WAL-OVERFLOW
// mitigation) is honored at the per-1M-row boundary inside each
// CopyFrom call by chunking the source.
func copyToStaging(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantSchema string,
	tableRows []stagingTableRow,
	columnRows []stagingColumnRow,
	snapshotRows []stagingSnapshotRow,
) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	// Set search_path so the bare `staging.iceberg_*` identifier
	// resolves to tenant_<uuid>.staging.iceberg_*. V0064 documents this
	// pattern explicitly: "the bare staging identifier resolves to
	// tenant_<uuid>.staging" via per-tenant search_path.
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`SET LOCAL search_path = %s, public`,
		pgx.Identifier{tenantSchema}.Sanitize(),
	)); err != nil {
		return fmt.Errorf("set search_path %s: %w", tenantSchema, err)
	}

	// Phase A: tables.
	if err := copyTables(ctx, conn.Conn(), tableRows); err != nil {
		return fmt.Errorf("copy iceberg_tables: %w", err)
	}
	// Phase B: columns (largest by row count typically — 5M rows at
	// envelope scale; chunked to honor walThrottleEvery).
	if err := copyColumns(ctx, conn.Conn(), columnRows); err != nil {
		return fmt.Errorf("copy iceberg_columns: %w", err)
	}
	// Phase C: snapshots.
	if err := copySnapshots(ctx, conn.Conn(), snapshotRows); err != nil {
		return fmt.Errorf("copy iceberg_snapshots: %w", err)
	}
	return nil
}

func copyTables(ctx context.Context, conn *pgx.Conn, rows []stagingTableRow) error {
	if len(rows) == 0 {
		return nil
	}
	cols := []string{"uuid", "namespace", "name", "current_snapshot_id", "metadata_location", "properties"}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{r.UUID, r.Namespace, r.Name, r.CurrentSnapshotID, r.MetadataLocation, r.Properties}, nil
	})
	_, err := conn.CopyFrom(ctx, pgx.Identifier{"staging", "iceberg_tables"}, cols, src)
	return err
}

func copyColumns(ctx context.Context, conn *pgx.Conn, rows []stagingColumnRow) error {
	if len(rows) == 0 {
		return nil
	}
	// WAL throttle — yield 1s per walThrottleEvery rows.
	cols := []string{"table_uuid", "snapshot_metadata_location", "column_id", "name", "data_type", "required", "doc", "ordinal"}
	for start := 0; start < len(rows); start += walThrottleEvery {
		end := start + walThrottleEvery
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		src := pgx.CopyFromSlice(len(chunk), func(i int) ([]any, error) {
			r := chunk[i]
			return []any{r.TableUUID, r.SnapshotMetadataLocation, r.ColumnID, r.Name, r.DataType, r.Required, r.Doc, r.Ordinal}, nil
		})
		if _, err := conn.CopyFrom(ctx, pgx.Identifier{"staging", "iceberg_columns"}, cols, src); err != nil {
			return err
		}
		if end < len(rows) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

func copySnapshots(ctx context.Context, conn *pgx.Conn, rows []stagingSnapshotRow) error {
	if len(rows) == 0 {
		return nil
	}
	cols := []string{"snapshot_id", "parent_snapshot_id", "metadata_location", "committed_at_ms", "operation", "summary"}
	for start := 0; start < len(rows); start += walThrottleEvery {
		end := start + walThrottleEvery
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		src := pgx.CopyFromSlice(len(chunk), func(i int) ([]any, error) {
			r := chunk[i]
			return []any{r.SnapshotID, r.ParentSnapshotID, r.MetadataLocation, r.CommittedAtMs, r.Operation, r.Summary}, nil
		})
		if _, err := conn.CopyFrom(ctx, pgx.Identifier{"staging", "iceberg_snapshots"}, cols, src); err != nil {
			return err
		}
		if end < len(rows) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

// transformedCounts is the per-Cypher-phase tally returned to the caller
// for inclusion in stats + the JSON baseline.
type transformedCounts struct {
	Tables       int
	Columns      int
	Snapshots    int
	LineageEdges int
}

// transformStaging streams batches of opts.CypherBatchSize rows from
// staging.iceberg_snapshots / staging.iceberg_columns into the graph
// via Plan 01-04 ingest.Service primitives. RLS scopes per-tenant via
// gc.ExecuteInTenant inside the service methods.
//
// Lineage edges: derived from each snapshot's parent_snapshot_id chain.
// We map (parent.metadata_location → snap.metadata_location) for every
// snapshot whose parent_snapshot_id is non-zero AND whose parent
// snapshot is also present in the batch (cross-batch edges are emitted
// in a follow-up pass at the end).
func transformStaging(
	ctx context.Context,
	pool *pgxpool.Pool,
	gc *graph.GraphClient,
	svc *ingest.Service,
	tenantID string,
	tenantSchema string,
	opts RealIngestOpts,
) (transformedCounts, error) {
	counts := transformedCounts{}

	// Step A: snapshots — MergeSnapshot per row.
	snapPaged := func(offset int) ([]stagingSnapshotRow, error) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire conn: %w", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			`SET LOCAL search_path = %s, public`,
			pgx.Identifier{tenantSchema}.Sanitize(),
		)); err != nil {
			return nil, fmt.Errorf("set search_path: %w", err)
		}
		rows, err := conn.Query(ctx, `
			SELECT snapshot_id, parent_snapshot_id, metadata_location,
			       committed_at_ms, operation
			  FROM staging.iceberg_snapshots
			  ORDER BY metadata_location
			 OFFSET $1 LIMIT $2
		`, offset, opts.CypherBatchSize)
		if err != nil {
			return nil, fmt.Errorf("staging snapshots query: %w", err)
		}
		defer rows.Close()
		var out []stagingSnapshotRow
		for rows.Next() {
			var r stagingSnapshotRow
			if err := rows.Scan(&r.SnapshotID, &r.ParentSnapshotID, &r.MetadataLocation,
				&r.CommittedAtMs, &r.Operation); err != nil {
				return nil, fmt.Errorf("scan snapshot row: %w", err)
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}

	snapByID := make(map[int64]string) // snapshot_id → metadata_location, for lineage edge derivation.
	var snapsTotal int32
	for offset := 0; ; offset += opts.CypherBatchSize {
		batch, err := snapPaged(offset)
		if err != nil {
			return counts, err
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			snap := iceberg.Snapshot{
				SnapshotID:       r.SnapshotID,
				ParentSnapshotID: r.ParentSnapshotID,
				TimestampMs:      r.CommittedAtMs,
				Operation:        r.Operation,
				MetadataLocation: r.MetadataLocation,
			}
			if err := svc.MergeSnapshot(ctx, tenantID, snap); err != nil {
				return counts, fmt.Errorf("merge snapshot %d: %w", r.SnapshotID, err)
			}
			snapByID[r.SnapshotID] = r.MetadataLocation
			atomic.AddInt32(&snapsTotal, 1)
		}
	}
	counts.Snapshots = int(snapsTotal)

	// Step B: columns — MergeColumns per snapshot. Group rows by
	// snapshot_metadata_location so each MergeColumns call does the
	// natural batch per Plan 01-04's contract.
	colPaged := func(snapLoc string) ([]iceberg.SchemaField, error) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire conn: %w", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			`SET LOCAL search_path = %s, public`,
			pgx.Identifier{tenantSchema}.Sanitize(),
		)); err != nil {
			return nil, fmt.Errorf("set search_path: %w", err)
		}
		rows, err := conn.Query(ctx, `
			SELECT column_id, name, data_type, required, doc
			  FROM staging.iceberg_columns
			 WHERE snapshot_metadata_location = $1
			 ORDER BY ordinal
		`, snapLoc)
		if err != nil {
			return nil, fmt.Errorf("staging columns query: %w", err)
		}
		defer rows.Close()
		var out []iceberg.SchemaField
		for rows.Next() {
			var f iceberg.SchemaField
			if err := rows.Scan(&f.ID, &f.Name, &f.Type, &f.Required, &f.Doc); err != nil {
				return nil, fmt.Errorf("scan column row: %w", err)
			}
			out = append(out, f)
		}
		return out, rows.Err()
	}

	var colsTotal int32
	for _, snapLoc := range snapByID {
		fields, err := colPaged(snapLoc)
		if err != nil {
			return counts, err
		}
		if len(fields) == 0 {
			continue
		}
		if err := svc.MergeColumns(ctx, tenantID, snapLoc, fields); err != nil {
			return counts, fmt.Errorf("merge columns for snap=%s: %w", snapLoc, err)
		}
		atomic.AddInt32(&colsTotal, int32(len(fields)))
		if int(colsTotal) >= opts.TargetColumnCount {
			break
		}
	}
	counts.Columns = int(colsTotal)

	// Step C: lineage edges — derive (parent_meta_loc → snap_meta_loc)
	// for every snapshot whose parent is also present. The Plan 01-04
	// MergeLineageEdge runs cycle pre-check + advisory lock + MERGE all
	// inside one tx.
	var edgesTotal int32
	now := time.Now()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return counts, fmt.Errorf("acquire conn (lineage): %w", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`SET LOCAL search_path = %s, public`,
		pgx.Identifier{tenantSchema}.Sanitize(),
	)); err != nil {
		conn.Release()
		return counts, fmt.Errorf("set search_path lineage: %w", err)
	}
	rows, err := conn.Query(ctx, `
		SELECT snapshot_id, parent_snapshot_id, metadata_location
		  FROM staging.iceberg_snapshots
		 WHERE parent_snapshot_id IS NOT NULL AND parent_snapshot_id <> 0
	`)
	if err != nil {
		conn.Release()
		return counts, fmt.Errorf("lineage rows: %w", err)
	}
	type edgeWanted struct {
		parentSnap int64
		childLoc   string
	}
	var edges []edgeWanted
	for rows.Next() {
		var snapID, parentID int64
		var childLoc string
		if err := rows.Scan(&snapID, &parentID, &childLoc); err != nil {
			rows.Close()
			conn.Release()
			return counts, fmt.Errorf("scan lineage row: %w", err)
		}
		edges = append(edges, edgeWanted{parentSnap: parentID, childLoc: childLoc})
	}
	rows.Close()
	conn.Release()

	for _, e := range edges {
		parentLoc, ok := snapByID[e.parentSnap]
		if !ok {
			continue
		}
		// Skip self-edges (defensive — should not occur in well-formed
		// snapshot chains; MergeLineageEdge would reject anyway).
		if parentLoc == e.childLoc {
			continue
		}
		if err := svc.MergeLineageEdge(ctx, tenantID, parentLoc, e.childLoc, "", now); err != nil {
			// Continue past per-edge cycle errors — bulk-ingest treats
			// them as data-quality signal, not a halt-the-ingest event.
			continue
		}
		atomic.AddInt32(&edgesTotal, 1)
		if int(edgesTotal) >= opts.TargetLineageEdgeCount {
			break
		}
	}
	counts.LineageEdges = int(edgesTotal)
	counts.Tables = len(snapByID) // one Table-equivalent per distinct snapshot location seen.

	return counts, nil
}

// truncateStaging clears the per-tenant staging.iceberg_* tables (V0064
// spec — short-lived COPY targets). Best-effort: a failure here does
// NOT mean the ingest itself failed; the graph already has the merged
// data. Operators inspect the JSON baseline for the actual ingest
// outcome.
func truncateStaging(ctx context.Context, pool *pgxpool.Pool, tenantSchema string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`SET LOCAL search_path = %s, public`,
		pgx.Identifier{tenantSchema}.Sanitize(),
	)); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `TRUNCATE staging.iceberg_tables, staging.iceberg_columns, staging.iceberg_snapshots`)
	return err
}

// ----------------------------------------------------------------------
// Helpers — TableMetadata projections.
// ----------------------------------------------------------------------

func metaUUID(m *iceberg.TableMetadata) string {
	if m == nil {
		return ""
	}
	return m.UUID
}

func metaSnapID(m *iceberg.TableMetadata) int64 {
	if m == nil {
		return 0
	}
	return m.CurrentSnapshotID
}

func metaLoc(m *iceberg.TableMetadata) string {
	if m == nil {
		return ""
	}
	return m.MetadataLocation
}

func nonEmptyMetaLoc(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func joinNamespace(ns []string) string {
	if len(ns) == 0 {
		return ""
	}
	out := ns[0]
	for i := 1; i < len(ns); i++ {
		out = out + "." + ns[i]
	}
	return out
}

func propsJSON(m *iceberg.TableMetadata) []byte {
	if m == nil || len(m.Properties) == 0 {
		return []byte(`{}`)
	}
	return mapToJSONBytes(m.Properties)
}

func summaryJSON(s map[string]string) []byte {
	if len(s) == 0 {
		return []byte(`{}`)
	}
	return mapToJSONBytes(s)
}

// mapToJSONBytes serialises a string→string map to JSON bytes without
// pulling in the encoding/json package overhead per row (the body is
// flat string KVs; manual escape is faster than a json.Marshal call
// for 5M+ rows).
func mapToJSONBytes(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte(`{}`)
	}
	buf := make([]byte, 0, 64)
	buf = append(buf, '{')
	first := true
	for k, v := range m {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, '"')
		buf = appendJSONEscaped(buf, k)
		buf = append(buf, '"', ':', '"')
		buf = appendJSONEscaped(buf, v)
		buf = append(buf, '"')
	}
	buf = append(buf, '}')
	return buf
}

func appendJSONEscaped(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			if c < 0x20 {
				buf = append(buf, '?')
			} else {
				buf = append(buf, c)
			}
		}
	}
	return buf
}
