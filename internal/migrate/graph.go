package migrate

// graph.go — per-tenant AGE graph migration runner.
//
// Resolves Open Question 1 (Phase 1 RESEARCH §Open Questions lines 1669-1672):
// "Where do graph migrations live and how are they applied per-tenant?".
//
// Decision: graph migrations sit OUTSIDE Atlas (Atlas's exclude pattern
// `ag_catalog.*` in migrations/atlas.hcl ensures Atlas never sees graph
// DDL). They live at /Users/evgeny/neksur-core/migrations/graph/V00*.sql
// and are applied per-tenant by ApplyTenantGraph (this file), called
// from cmd/migrate/main.go's tenant loop immediately after Atlas's
// ApplyTenant returns.
//
// Embedding note: the obvious shape would be a single
//
//   //go:embed migrations/graph/*.sql
//
// directive on this file — but Go's embed system disallows parent-path
// traversal, and migrations/graph/ is a sibling of internal/migrate/.
// We work around this by hosting the embed.FS in the sibling package
// `migrations/graph` (package graphmigrations) and consuming its
// exported FS variable here. Same compile-time guarantee, no parent-path
// hack.

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	graphmigrations "github.com/neksur-com/neksur/migrations/graph"
)

// graphMigrationFilePattern matches `V<digits>__<slug>.sql` filenames.
// Capture group 1 is the digits (used as the graph_schema_revisions PK).
var graphMigrationFilePattern = regexp.MustCompile(`^V0*(\d+)__.+\.sql$`)

// ApplyTenantGraph applies the embedded graph migrations to a single
// tenant's schema. Steps:
//
//  1. Acquire admin connection from `pool` (LOAD 'age' requires superuser).
//  2. Begin a tx; SET LOCAL search_path = ag_catalog, <schema>, …; LOAD 'age'.
//  3. CREATE TABLE IF NOT EXISTS <schema>.graph_schema_revisions(
//        version    text PRIMARY KEY,
//        applied_at timestamptz NOT NULL DEFAULT now()).
//  4. Walk the embedded FS, picking up files matching V<digits>__*.sql
//     in lexicographic order.
//  5. For each file whose 4-digit version is NOT in graph_schema_revisions:
//     strip the wrapping `BEGIN;` / `COMMIT;` (the file's own tx markers
//     are intended for direct psql apply; we're already inside an
//     externally-managed transaction) then Exec the remaining body.
//     INSERT (version, now()) into graph_schema_revisions.
//  6. Commit.
//
// Idempotent: re-running on an already-current tenant is a no-op (the
// per-file version check skips applied files). Re-running mid-way after
// a previous failure replays from the first unapplied version (V0030's
// individual create_vlabel / create_elabel calls are guarded by
// ag_catalog.ag_label existence probes; V0031 uses CREATE INDEX IF NOT
// EXISTS; V0032 is plain CREATE POLICY which is gated by the revision
// table itself).
//
// Phase1MaxVersion / GraphPhase1MaxVersion in migrate.go track the
// expected high-water marks for relational and graph migrations
// respectively.
func ApplyTenantGraph(ctx context.Context, pool *pgxpool.Pool, schemaName string) error {
	const op = "ApplyTenantGraph"
	if pool == nil {
		return fmt.Errorf("migrate: %s: pool is nil", op)
	}
	if schemaName == "" {
		return fmt.Errorf("migrate: %s: schema is empty", op)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %s: acquire: %w", op, err)
	}
	defer conn.Release()

	qSchema := pgx.Identifier{schemaName}.Sanitize()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "LOAD 'age'"); err != nil {
		return fmt.Errorf("migrate: %s: LOAD age: %w", op, err)
	}
	// search_path has ag_catalog FIRST so create_vlabel / create_elabel /
	// the V0020 create_property_index polyfill resolve unqualified; the
	// tenant schema comes second so neksur.* identifiers in the body
	// (V0031/V0032 reference neksur."HAS_COLUMN" etc.) resolve into the
	// per-tenant graph.
	setPathSQL := fmt.Sprintf(`SET LOCAL search_path = ag_catalog, %s, "$user", public`, qSchema)
	if _, err := tx.Exec(ctx, setPathSQL); err != nil {
		return fmt.Errorf("migrate: %s: set search_path: %w", op, err)
	}

	// Per-tenant revisions table — created inside the tenant schema, NOT
	// in public. This keeps graph-migration state isolated per tenant
	// (parallel to public.atlas_schema_revisions, which Atlas shares,
	// but Plan 04 already decided per-tenant revisions for the relational
	// side via the --revisions-schema flag; graph follows that lead).
	revisionsTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.graph_schema_revisions (
        version    text PRIMARY KEY,
        applied_at timestamptz NOT NULL DEFAULT now()
    )`, qSchema)
	if _, err := tx.Exec(ctx, revisionsTable); err != nil {
		return fmt.Errorf("migrate: %s: ensure revisions table: %w", op, err)
	}

	files, err := fs.ReadDir(graphmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("migrate: %s: read embed: %w", op, err)
	}
	type entry struct {
		version string
		name    string
	}
	var entries []entry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		m := graphMigrationFilePattern.FindStringSubmatch(f.Name())
		if m == nil {
			continue
		}
		v := m[1]
		for len(v) < 4 {
			v = "0" + v
		}
		entries = append(entries, entry{version: v, name: f.Name()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].version < entries[j].version })

	// Read already-applied versions in one query (lookup map for the loop).
	appliedRows, err := tx.Query(ctx,
		fmt.Sprintf(`SELECT version FROM %s.graph_schema_revisions`, qSchema))
	if err != nil {
		return fmt.Errorf("migrate: %s: read revisions: %w", op, err)
	}
	applied := map[string]bool{}
	for appliedRows.Next() {
		var v string
		if err := appliedRows.Scan(&v); err != nil {
			appliedRows.Close()
			return fmt.Errorf("migrate: %s: scan revision: %w", op, err)
		}
		applied[v] = true
	}
	appliedRows.Close()
	if err := appliedRows.Err(); err != nil {
		return fmt.Errorf("migrate: %s: revisions rows.Err: %w", op, err)
	}

	for _, e := range entries {
		if applied[e.version] {
			continue
		}
		body, err := fs.ReadFile(graphmigrations.FS, e.name)
		if err != nil {
			return fmt.Errorf("migrate: %s: read %s: %w", op, e.name, err)
		}
		sql := stripWrappingTx(string(body))
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("migrate: %s: apply %s: %w", op, e.name, err)
		}
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s.graph_schema_revisions (version) VALUES ($1)`, qSchema),
			e.version,
		); err != nil {
			return fmt.Errorf("migrate: %s: record %s: %w", op, e.version, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: %s: commit: %w", op, err)
	}
	return nil
}

// stripWrappingTx removes any standalone `BEGIN;` or `COMMIT;` line
// from a migration body so the file content can be Exec'd inside an
// externally-managed transaction. The graph migration files (V0030,
// V0032) wrap their bodies in BEGIN/COMMIT for direct-psql apply; the
// runner owns the outer transaction and needs the inner markers removed
// (nested BEGIN is a no-op SAVEPOINT in Postgres, but inner COMMIT
// commits the outer tx and breaks the runner's atomicity contract).
//
// Only top-level standalone tx markers are stripped — embedded "BEGIN"
// keywords inside DO-block plpgsql blocks are unaffected because they
// share a line with `END` or appear after whitespace+keyword context
// (e.g., `BEGIN ... END`).
func stripWrappingTx(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "BEGIN;" || trim == "COMMIT;" {
			out.WriteString("\n")
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
