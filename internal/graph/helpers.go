// GC-02 typed bounded-traversal helpers — compile/init-time enforcement
// of D-001.08's "no unbounded VLP" + the GC-02 mitigation for AGE 1.6's
// 3-hop VLP planner gap (CONTEXT line 165).
//
// The Phase 0 client.go already rejects unbounded `*`, `*N..`, `*..`
// at the gateway via ValidateTraversalDepth. These helpers add the
// next layer of defence: callers that build dynamic Cypher fragments
// for VLP queries (e.g., the future Phase 2 impact-analysis API)
// route through `BoundedAncestors` / `BoundedDescendants` instead of
// concatenating raw `*1..N` patterns. The helper panics if the caller
// asks for a depth greater than BoundedDepthMax — surfacing the
// programmer error at the call site rather than running an
// unexpectedly-deep walk in production.
//
// Why panic and not return an error? The hop count is a compile-time
// constant in 99% of call sites; passing a value > BoundedDepthMax is
// always a programming bug, not a runtime input. Panic at init or
// startup is the right shape (and tests can monkey-patch the const
// via a build-time override if they ever need to).

package graph

import "fmt"

// BoundedDepthMax is the maximum VLP depth this package will emit. Per
// D-001.08 the upper bound is 5 (the cycle pre-check uses *1..5), but
// general-purpose traversal queries should stay at 3 (the Phase 1
// per-tenant latency budget; matches the cypher_p99 default depth
// from Phase 0). Callers that need *1..5 should use the per-package
// ingest constants instead — those are explicit, not derived from
// this helper.
const BoundedDepthMax = 3

// init force-references the Phase 0 sentinel so the package will not
// compile if ErrUnboundedTraversal is removed. The exists check is the
// GC-02 enforcement: if a future refactor removes the Phase 0 ban,
// this package's tests will fail to build.
func init() {
	_ = ErrUnboundedTraversal
}

// BoundedAncestors returns a Cypher fragment matching ancestors of a
// node via `*1..N` LINEAGE_OF traversal, where N is clamped to
// BoundedDepthMax. Use as a building block for queries that walk
// upstream lineage.
//
// Example:
//
//	frag := BoundedAncestors("Table", 3)
//	// frag == "(n:Table)<-[:LINEAGE_OF*1..3]-(ancestor)"
//
// Panics if depth > BoundedDepthMax (programmer error; see package doc).
// Panics on depth < 1 (a 0-hop traversal is the trivial identity).
//
// The variable names `n` (anchor) and `ancestor` (downstream) are
// fixed so callers compose fragments by string concatenation without
// having to re-bind variables across fragment boundaries.
func BoundedAncestors(label string, depth int) string {
	if depth < 1 {
		panic(fmt.Sprintf("graph.BoundedAncestors: depth %d < 1", depth))
	}
	if depth > BoundedDepthMax {
		panic(fmt.Sprintf("graph.BoundedAncestors: depth %d exceeds BoundedDepthMax = %d (D-001.08 / GC-02)",
			depth, BoundedDepthMax))
	}
	return fmt.Sprintf("(n:%s)<-[:LINEAGE_OF*1..%d]-(ancestor)", label, depth)
}

// BoundedDescendants is the downstream symmetric of BoundedAncestors —
// matches descendants reachable via outgoing LINEAGE_OF edges, depth
// clamped to BoundedDepthMax. Use for impact-analysis queries.
//
// Panics on the same conditions as BoundedAncestors.
func BoundedDescendants(label string, depth int) string {
	if depth < 1 {
		panic(fmt.Sprintf("graph.BoundedDescendants: depth %d < 1", depth))
	}
	if depth > BoundedDepthMax {
		panic(fmt.Sprintf("graph.BoundedDescendants: depth %d exceeds BoundedDepthMax = %d (D-001.08 / GC-02)",
			depth, BoundedDepthMax))
	}
	return fmt.Sprintf("(n:%s)-[:LINEAGE_OF*1..%d]->(descendant)", label, depth)
}
