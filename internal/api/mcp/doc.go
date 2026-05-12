// Package mcp hosts the Model Context Protocol server. Per
// docs/phase-0-stack.md §6 this package will contain server.go (stdio +
// SSE per MCP spec via anthropics/mcp-go SDK) and tools.go (the actual
// MCP tools — discover.search, model.list_metrics, graph.traverse).
// The graph.cypher tool's hardening contract is the Phase 5 ADR-004
// target; internal/graph provides the Phase 0 floor that this package
// will compose at M2.
//
// Phase 0 status: placeholder. M2 lands the server + initial tool set.
package mcp
