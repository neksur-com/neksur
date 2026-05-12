// Package testfixture spins up an apache/age:release_PG16_1.6.0
// testcontainer, applies the Phase 0 migration set (V0001 → V0010 →
// V0020 → V0025 → V0030), creates a `neksur_app` non-superuser role,
// and returns connection credentials usable by tests/integration and
// tests/security.
//
// Why a shared fixture package?
//   - `go test` runs each test package's TestMain independently;
//     a session-scoped Postgres container shared across packages would
//     require either an external orchestrator or os-package-level
//     coordination. We don't need that complexity for Phase 0 — each
//     test package gets its own container via Start, paid once at
//     TestMain via sync.Once. Containers are cheap; the AGE image is
//     pre-pulled in CI (see .github/workflows/integration.yml).
//   - The fixture is the Go counterpart of tests/integration/conftest.py
//     in the Python tier — same exact behaviour (apply migrations,
//     create neksur_app, grant CRUD + USAGE + EXECUTE), just in Go.
//
// Threading: tests use SubTest naming and t.Parallel() at their own
// discretion. The pgx pool is concurrency-safe; tests that mutate
// shared graph state should serialize themselves via a t.Cleanup hook
// that ROLLBACKs (the typical pattern).
package testfixture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// AGEImage is the canonical Postgres 16 + AGE 1.6.0 image, locked by
// ADR-001 D-001.01 and confirmed in 00-RESEARCH.md §Standard Stack.
// Do not upgrade without an ADR amendment.
const AGEImage = "apache/age:release_PG16_1.6.0"

// Migrations are applied in the order required by the Phase 0 plan:
//
//	V0001 — extensions (age, pg_stat_statements, pgaudit-if-available)
//	V0010 — graph + 19 vlabels + 24 elabels
//	V0020 — D-001.07 property + edge indexes (polyfilled create_property_index*)
//	V0025 — per-vlabel tenant btree + GIN — BEFORE-load mitigation for AGE #1010
//	V0030 — RLS policies (43 × FORCE + 4 policies + tenant_id CHECK)
//
// The list is kept in sync with infra/migrations/run-migrations.sh; both
// paths consume the same SQL files.
var Migrations = []string{
	"postgres/V0001__enable_extensions.sql",
	"graph/V0010__create_graph_and_labels.sql",
	"graph/V0020__property_indexes.sql",
	"graph/V0025__tenant_indexes_and_gin.sql",
	"postgres/V0030__rls_policies.sql",
}

// AGEContainer wraps the running testcontainer plus its credentials.
type AGEContainer struct {
	Container        testcontainers.Container
	Host             string
	Port             string
	SuperuserDSN     string // postgres/neksur_test — used for fixture setup
	AppDSN           string // neksur_app/neksur_app — used by tests to exercise RLS
	MigratedSuperDSN string // same as SuperuserDSN; the schema has been applied
}

// Start spins up a fresh AGE 1.6.0 container, applies migrations, and
// creates the neksur_app non-superuser role. Returns an AGEContainer
// the test should defer .Terminate() on. The context applies to the
// entire lifecycle including waiting for ready.
//
// Total cold-start cost: ~5-8s on a warm-image laptop, ~15-25s on a
// cold-image CI runner. Tests using this should declare a longer
// -timeout (default 10m is fine).
func Start(ctx context.Context) (*AGEContainer, error) {
	req := testcontainers.ContainerRequest{
		Image:        AGEImage,
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "postgres",
			"POSTGRES_PASSWORD": "neksur_test",
			"POSTGRES_DB":       "postgres",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(90 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("testfixture: start container: %w", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: get host: %w", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: get mapped port: %w", err)
	}
	superDSN := fmt.Sprintf("postgres://postgres:neksur_test@%s:%s/postgres?sslmode=disable", host, port.Port())
	appDSN := fmt.Sprintf("postgres://neksur_app:neksur_app@%s:%s/postgres?sslmode=disable", host, port.Port())

	if err := applyMigrations(ctx, superDSN); err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: apply migrations: %w", err)
	}
	if err := createAppRole(ctx, superDSN); err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("testfixture: create neksur_app: %w", err)
	}

	return &AGEContainer{
		Container:        c,
		Host:             host,
		Port:             port.Port(),
		SuperuserDSN:     superDSN,
		AppDSN:           appDSN,
		MigratedSuperDSN: superDSN,
	}, nil
}

// Terminate stops and removes the container. Safe to call multiple times.
func (a *AGEContainer) Terminate(ctx context.Context) error {
	if a == nil || a.Container == nil {
		return nil
	}
	return a.Container.Terminate(ctx)
}

// migrationsDir resolves to the repo's migrations/ directory. The test
// binary lives at tests/{integration,security}/ depth-2 from repo root,
// or this helper package is at tests/testfixture/. We walk up from the
// CWD until we find a `migrations/` sibling. CWD when `go test` runs
// is the package directory.
func migrationsDir() (string, error) {
	// Walk up from the working dir looking for `migrations/postgres/V0001__enable_extensions.sql`.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cur := wd
	for i := 0; i < 6; i++ { // safety bound
		marker := filepath.Join(cur, "migrations", "postgres", "V0001__enable_extensions.sql")
		if _, err := os.Stat(marker); err == nil {
			return filepath.Join(cur, "migrations"), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", fmt.Errorf("testfixture: could not locate migrations/ relative to %s", wd)
}

func applyMigrations(ctx context.Context, dsn string) error {
	mdir, err := migrationsDir()
	if err != nil {
		return err
	}

	// AGE migrations are multi-statement and use $$ dollar-quoting for
	// the CREATE-vlabel DO-block etc. The simple-query protocol
	// (`conn.Exec` with a single multi-statement string) handles those
	// natively — same behaviour as the Python tier's pgconn.exec_.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	for _, rel := range Migrations {
		path := filepath.Join(mdir, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", rel, err)
		}
	}
	return nil
}

func createAppRole(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// Same SQL as the Python tier's conftest.py — create neksur_app
	// without BYPASSRLS, grant USAGE on neksur + ag_catalog, grant
	// CRUD on existing + future tables in neksur, grant SELECT on
	// ag_catalog tables and EXECUTE on ag_catalog functions.
	stmts := []string{
		`DO $$
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'neksur_app') THEN
		         CREATE ROLE neksur_app NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD 'neksur_app';
		     END IF;
		 END
		 $$`,
		`GRANT USAGE ON SCHEMA neksur TO neksur_app`,
		`GRANT USAGE ON SCHEMA ag_catalog TO neksur_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA neksur TO neksur_app`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA neksur GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO neksur_app`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA ag_catalog TO neksur_app`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO neksur_app`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("exec %q: %w", trim(s), err)
		}
	}
	return nil
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return s
}

// Skip is a small helper for tests that need to opt out cleanly when
// Docker is unavailable — e.g., on a developer laptop without Docker
// installed. Returns nil if SKIP_DOCKER is not set.
func SkipIfNoDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") == "1" {
		t.Skip("SKIP_DOCKER=1 — skipping testcontainers-based test")
	}
}

// NewAGEPool builds a pgxpool.Pool against the given DSN, with the
// canonical AGE prelude wired into AfterConnect:
//   - LOAD 'age'                                       (the AGE session-state initialiser)
//   - SET search_path = ag_catalog, "$user", public    (so cypher() and graphid resolve)
//
// This mirrors internal/graph.GraphClient's pool wiring exactly. Without
// this AfterConnect hook tests will see "function cypher(unknown, unknown)
// does not exist" and "operator does not exist: ag_catalog.graphid =
// ag_catalog.graphid" errors — both are search_path / session-state
// failures.
//
// We also disable the statement-cache mode that conflicts with DISCARD
// ALL (TestSessionVarBleed runs DISCARD ALL which invalidates pgx's
// server-side prepared statements — switching to a simpler protocol
// mode avoids prepared-statement lookups that would fail post-DISCARD).
func NewAGEPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("testfixture: parse DSN: %w", err)
	}
	// `describe_exec` instead of `cache_statement` removes pgx's per-
	// connection prepared-statement cache that would break after the
	// session-var-bleed test runs DISCARD ALL. Tests pay a small per-
	// query overhead; production code paths use the default cache via
	// internal/graph.NewGraphClient where DISCARD ALL is only called
	// at pool-return boundaries (the connection is released
	// immediately after, so the cache invalidation is harmless).
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
			return fmt.Errorf("testfixture: LOAD 'age': %w", err)
		}
		if _, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
			return fmt.Errorf("testfixture: SET search_path: %w", err)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("testfixture: pgxpool.NewWithConfig: %w", err)
	}
	return pool, nil
}
