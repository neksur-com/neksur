// Package lineage hosts the OpenLineage events consumer. Per
// docs/phase-0-stack.md §6 it will contain consumer.go (HTTP endpoint
// receiving OpenLineage POSTs from engines / OL-emitting tooling) and
// parser.go (event → graph edges via internal/graph). OpenLineage
// producer (Neksur emitting events) is Phase 0.5 per §2.10.
//
// Phase 0 status: placeholder. M3 lands the consumer + graph-ingest path.
package lineage
