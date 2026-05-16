// Package dialect defines the per-engine SQL emitter surface for the
// cross-engine policy compiler (D-2.04 / Plan 02-04).
//
// Each engine (Trino, Spark, Dremio, Snowflake) implements
// DialectCompiler. The cross-engine compiler dispatches by engine
// kind (read from public.engine_registry — see store/engine.go) and
// asks the dialect to lower a parsed fragment AST into an engine-
// native SQL string.
//
// Phase 2 ships:
//   - trino.go:  live, used by the SQL proxy reference engine.
//   - spark.go:  live, used by the Spark Extension (Scala) over a
//                cross-process gRPC contract — the Scala side asks
//                us to emit the SQL fragment it will splice into its
//                generated SparkSession.sql() call.
//   - dremio.go: stub, returns ErrDialectStub. Live emitter lands in
//                Phase 3 plan 03-XX.
//   - snowflake: deferred to Phase 5 (no stub in Phase 2 because the
//                engine isn't even in the registry CHECK allowlist
//                yet — V0070 admits it but the gateway doesn't dispatch).
//
// The package intentionally avoids importing the parent compiler
// package — emitters take the AST node types directly. This avoids
// an import cycle (compiler → dialect → compiler) and lets the
// dialect tests live without spinning up the full compiler.

package dialect

import (
	"errors"
)

// FragmentKind mirrors compiler.FragmentKind. Duplicated here (rather
// than imported) to keep the dialect package import-cycle-free.
type FragmentKind int

const (
	// FragmentRowFilter is a boolean predicate (WHERE-clause body).
	FragmentRowFilter FragmentKind = iota + 1
	// FragmentColumnMask is a list of column projections.
	FragmentColumnMask
)

// Node is the AST interface — concrete node types are defined by the
// parent compiler package. The dialect emitter type-switches on
// these to produce SQL.
type Node interface{ AstNode() }

// Marker tags so the parent package's node types satisfy Node
// without re-declaring the entire AST here. The compiler package's
// types implement AstNode() directly.

// ErrDialectStub is returned by stubs (Dremio in Phase 2) so the
// cross-engine compiler can short-circuit and mark the artifact as
// compile_failed without attempting a probe.
var ErrDialectStub = errors.New("dialect: stub emitter")

// DialectCompiler is the per-engine SQL emitter. Each implementation
// is stateless and safe for concurrent use — the cross-engine
// compiler holds a single shared instance per engine kind across all
// CompileAll invocations.
type DialectCompiler interface {
	// Kind returns the engine kind string this emitter handles
	// ("trino", "spark", "dremio", "snowflake"). The cross-engine
	// compiler uses this to index its dispatch table.
	Kind() string

	// CompileRowFilter lowers a predicate AST into an engine-native
	// WHERE-clause body (without the leading "WHERE "). The caller
	// is responsible for splicing the result into the generated
	// SQL statement.
	//
	// Returns ErrDialectStub when the implementation is a stub.
	CompileRowFilter(table string, predicate any) (string, error)

	// CompileColumnMask lowers a column-mask AST into a comma-
	// separated SELECT-projection string (without the leading
	// "SELECT "). Caller splices into the generated SQL.
	//
	// `mask` is a []MaskProject from the parent compiler package
	// (passed as `any` to avoid the import cycle); each emitter
	// type-asserts internally.
	CompileColumnMask(table string, mask any) (string, error)
}
