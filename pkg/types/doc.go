// Package types defines public, importable shared types used across the
// Neksur monorepo and by future client libraries (Phase 1+ Go client per
// docs/phase-0-stack.md §2.3). Per §6 this package will hold policy.go,
// catalog.go, etc. — the wire / DTO types for the REST API and MCP tools.
//
// Anything in internal/* is private to the monorepo; anything here is
// importable by external Go programs. Keep the surface deliberately small.
//
// Phase 0 status: placeholder. M1 lands the first concrete types as the
// REST + MCP surfaces solidify.
package types
