// Package sqlproxy hosts the pgwire SQL proxy. Per docs/phase-0-stack.md
// §6 it will contain server.go (pgwire listener), parser.go (SQL AST),
// and rewriter.go (row-filter / column-mask injection at the SQL layer
// for read-path enforcement on Trino, M3).
//
// Phase 0 status: placeholder. M3 lands the pgwire listener + Trino
// adapter wiring per ADR-003 read-path enforcement diagram.
package sqlproxy
