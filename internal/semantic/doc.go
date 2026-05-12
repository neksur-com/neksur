// Package semantic hosts the Semantic Engine — the metric / dimension
// compiler that takes YAML semantic models and emits engine-specific SQL
// (Trino, Spark). Per docs/phase-0-stack.md §6 this package will contain
// ast.go, compiler.go, and dialects/{trino,spark}.go.
//
// Phase 0 status: placeholder. M2-M3 lands the AST + Trino dialect; M4
// adds the Spark dialect.
package semantic
