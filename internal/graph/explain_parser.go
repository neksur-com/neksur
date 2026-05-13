// EXPLAIN ANALYZE plan-tree parser for AGE-wrapped Cypher — D-001.14 part 3/4.
//
// AGE's Cypher executor wraps every Cypher statement in a Postgres
// outer query of the shape `SELECT * FROM cypher('<graph>', $$ ... $$)
// AS (r agtype)`. As a consequence, `EXPLAIN (FORMAT JSON, ANALYZE)
// SELECT * FROM cypher(...)` returns a JSON plan tree where the outer
// node is a Postgres FunctionScan over `cypher`, and the actual AGE
// pattern-match operators (which AGE renames as Postgres ScanState
// nodes against the per-label tables `_ag_label_vertex`,
// `_ag_label_edge`, plus the user-defined vlabel/elabel underlying
// tables) live inside an `InitPlan` or `SubPlan` subtree.
//
// The parser walks the whole subtree and:
//   - Sums `Actual Rows` for any node whose `Relation Name` matches a
//     vertex-shaped table (`_ag_label_vertex` or matches the
//     `<graph>_<vlabel>` pattern AGE uses internally). The sum gives
//     `nodesVisited`.
//   - Sums `Actual Rows` for any node whose `Relation Name` matches an
//     edge-shaped table (`_ag_label_edge` or `<graph>_<elabel>`). The
//     sum gives `edgesTraversed`.
//
// This is intentionally permissive about label-table naming because AGE
// 1.5.0 vs 1.6.0 differ slightly in how they materialise per-label
// tables under `ag_catalog`. The fall-through is the two reserved
// names `_ag_label_vertex` / `_ag_label_edge` which AGE always uses.
//
// Returns an *ExplainPlanError when planJSON cannot be unmarshalled or
// is malformed (empty / not an array / no Plan root). Returns (0, 0,
// nil) on a syntactically-valid plan that simply contained no graph
// access — this is normal and not an error (e.g., a Cypher RETURN
// 1 query).

package graph

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExplainPlanError is returned by ParseExplain when the input JSON is
// not a recognisable Postgres EXPLAIN plan tree.
type ExplainPlanError struct {
	msg string
}

func (e *ExplainPlanError) Error() string { return e.msg }

// newExplainPlanError formats a structured error message; useful for
// log/metric label cardinality (msg is the only field, so reflection
// based error_type labeling stays bounded).
func newExplainPlanError(format string, args ...any) *ExplainPlanError {
	return &ExplainPlanError{msg: fmt.Sprintf(format, args...)}
}

// ParseExplain walks the EXPLAIN (FORMAT JSON, ANALYZE) output for an
// AGE-wrapped Cypher statement and returns the summed vertex- and
// edge-table actual-row counts. See package-doc for the AGE plan-tree
// shape rationale.
//
// On a well-formed plan that contains no graph-table accesses the
// function returns (0, 0, nil) — that is not an error condition;
// it simply means the query did not touch the labelled tables.
func ParseExplain(planJSON []byte) (nodesVisited int64, edgesTraversed int64, err error) {
	if len(planJSON) == 0 {
		return 0, 0, newExplainPlanError("empty plan JSON")
	}

	// Postgres EXPLAIN (FORMAT JSON) returns a top-level JSON array of
	// objects, each with a "Plan" key. Most invocations return a
	// single-element array.
	var root []map[string]any
	if err := json.Unmarshal(planJSON, &root); err != nil {
		return 0, 0, newExplainPlanError("unmarshal plan JSON: %v", err)
	}
	if len(root) == 0 {
		return 0, 0, newExplainPlanError("plan JSON array is empty")
	}

	planNode, ok := root[0]["Plan"].(map[string]any)
	if !ok {
		return 0, 0, newExplainPlanError("plan JSON root has no \"Plan\" object")
	}

	var nodes, edges int64
	walkPlanNode(planNode, &nodes, &edges)
	return nodes, edges, nil
}

// walkPlanNode recursively traverses the plan tree, accumulating vertex
// and edge actual-row counts. The traversal follows three plan-tree
// edges that Postgres uses to attach subplans:
//
//   - "Plans"     — children of a join / aggregate / etc.
//   - "InitPlan"  — eager subplans
//   - "SubPlan"   — lazy correlated subplans (this is where AGE
//                   typically attaches the Cypher pattern-match graph)
//
// All three are JSON arrays of plan-node objects.
func walkPlanNode(node map[string]any, nodes, edges *int64) {
	if node == nil {
		return
	}

	// Classify this node by its Relation Name (if present). We use the
	// `Actual Rows` field, which the EXPLAIN ANALYZE output emits for
	// every executor node, as the "rows touched" estimate.
	if rel, ok := node["Relation Name"].(string); ok && rel != "" {
		actualRows, _ := node["Actual Rows"].(float64) // JSON numbers decode to float64
		switch classifyRelation(rel) {
		case relationVertex:
			*nodes += int64(actualRows)
		case relationEdge:
			*edges += int64(actualRows)
		}
	}

	// Descend into the three subplan-attachment points.
	for _, key := range []string{"Plans", "InitPlan", "SubPlan"} {
		raw, ok := node[key]
		if !ok {
			continue
		}
		children, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, c := range children {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			walkPlanNode(cm, nodes, edges)
		}
	}
}

type relationKind int

const (
	relationOther relationKind = iota
	relationVertex
	relationEdge
)

// classifyRelation maps a Postgres Relation Name to either the vertex,
// edge, or "other" bucket. AGE uses two reserved table names plus a
// per-label naming convention; we recognise both styles.
//
//   - `_ag_label_vertex`        → vertex (reserved)
//   - `_ag_label_edge`          → edge   (reserved)
//   - relation name contains `_v_` or ends with `_vertex` → vertex
//     (per-label inherited tables that AGE produces in 1.6.0)
//   - relation name contains `_e_` or ends with `_edge`   → edge
//
// Anything else is "other" — including the ag_catalog metadata tables
// and any user-defined Postgres tables that a Cypher LOAD CSV step
// might touch.
func classifyRelation(rel string) relationKind {
	switch rel {
	case "_ag_label_vertex":
		return relationVertex
	case "_ag_label_edge":
		return relationEdge
	}
	switch {
	case strings.Contains(rel, "_v_") || strings.HasSuffix(rel, "_vertex"):
		return relationVertex
	case strings.Contains(rel, "_e_") || strings.HasSuffix(rel, "_edge"):
		return relationEdge
	default:
		return relationOther
	}
}
