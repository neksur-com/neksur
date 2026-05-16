// CEL predicate compile-artifact serialization (D-2.04 / Plan 02-04).
//
// ABAC and three-layer policies (P5/P6) are expressed in CEL, not SQL.
// For these the "compiled artifact" is the cel.Program produced by
// internal/policy/cel.Compiler.CompileOrGet plus a *binding manifest*
// that lists the symbol bindings the program references. The gateway
// uses the binding manifest at evaluation time to know which
// AttributeResolver layers to populate.
//
// Serialization shape:
//
//   {
//     "type":     "cel",
//     "policy_id": "<uuid>",
//     "version":   1,
//     "source":   "<the CEL source text — hashing trigger for cache invalidation>",
//     "bindings": ["principal","table","manifest","claims",...],
//     "checksum": "<sha256(source)>"
//   }
//
// We do NOT serialize the cel.Program itself — cel-go's ast / program
// surface is in-process only (no stable binary export format in v0.28).
// Recompiling from source on cold-cache-miss is the documented pattern;
// the LRU cache in lru.go makes the warm path effectively free.
//
// The manifest is the artifact_body stored in CompiledPolicy. Storing
// the source + checksum (not just an opaque blob) gives operators a
// human-debuggable record on `SELECT ... FROM cypher(...)` queries and
// lets a Phase 3 cross-process compile cache (Redis) key off the
// checksum directly.

package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"

	policycel "github.com/neksur-com/neksur/internal/policy/cel"
)

// CELArtifact is the JSON-serializable manifest for a CEL CompiledPolicy.
type CELArtifact struct {
	Type     string   `json:"type"`     // always "cel"
	PolicyID string   `json:"policy_id"`
	Version  int      `json:"version"`  // schema version (1 in Phase 2)
	Source   string   `json:"source"`   // canonical CEL source text
	Bindings []string `json:"bindings"` // sorted list of referenced symbols
	Checksum string   `json:"checksum"` // SHA-256(source), hex-encoded
}

// celArtifactSchemaVersion is bumped when we change the JSON shape.
// Phase 2 ships v1; Phase 3's binding-typing extension will bump to v2.
const celArtifactSchemaVersion = 1

// CompileCELArtifact compiles the given CEL source against the Phase
// 2 env, extracts the referenced symbol bindings, and returns a
// serializable CELArtifact + the live cel.Program. The Program is
// returned alongside the artifact so the caller can warm its in-
// process LRU without a second compile RTT.
//
// Returns ErrCompileFailed (wrapped) on any cel.Compile / cel.Program
// failure — the CompiledPolicy.status is set to compile_failed by the
// caller and the probe is skipped.
func CompileCELArtifact(env *cel.Env, comp *policycel.Compiler, policyID, source string) (*CELArtifact, cel.Program, error) {
	if env == nil {
		return nil, nil, fmt.Errorf("%w: nil cel env", ErrCompileFailed)
	}
	// Use the parent compiler's cache-aware path so we share the
	// per-process LRU and pay compile cost at most once per source.
	prog, err := comp.CompileOrGet(policyID, source)
	if err != nil {
		// CompileOrGet already wraps with policycel.ErrCompileFailed;
		// join with the compiler-package sentinel so callers in this
		// package can branch on errors.Is(err, ErrCompileFailed).
		return nil, nil, fmt.Errorf("%w: %v", ErrCompileFailed, err)
	}

	// Compile a second pass to recover the AST — env.Compile is the
	// only path that yields the AST; CompileOrGet's program drops it
	// (program is opaque). The compile is fast on the cold path
	// (~5-15ms per cel-go internals) and unused on the warm path
	// because Bindings extraction only runs at CompiledPolicy write
	// time, not on every evaluation.
	a, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, nil, fmt.Errorf("%w: extract ast for bindings: %v", ErrCompileFailed, issues.Err())
	}
	bindings := extractBindings(a)

	sum := sha256.Sum256([]byte(source))
	art := &CELArtifact{
		Type:     "cel",
		PolicyID: policyID,
		Version:  celArtifactSchemaVersion,
		Source:   source,
		Bindings: bindings,
		Checksum: hex.EncodeToString(sum[:]),
	}
	return art, prog, nil
}

// Encode returns the artifact's canonical JSON serialization. Used by
// the AGE writer to populate CompiledPolicy.artifact_body.
func (a *CELArtifact) Encode() (string, error) {
	if a == nil {
		return "", fmt.Errorf("compiler: nil CELArtifact")
	}
	buf, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("compiler: marshal cel artifact: %w", err)
	}
	return string(buf), nil
}

// DecodeCELArtifact parses a serialized CELArtifact. Returns
// ErrCompileFailed (wrapped) on invalid JSON or schema-version skew.
func DecodeCELArtifact(s string) (*CELArtifact, error) {
	var a CELArtifact
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return nil, fmt.Errorf("%w: decode cel artifact: %v", ErrCompileFailed, err)
	}
	if a.Type != "cel" {
		return nil, fmt.Errorf("%w: cel artifact type=%q (want %q)", ErrCompileFailed, a.Type, "cel")
	}
	if a.Version != celArtifactSchemaVersion {
		return nil, fmt.Errorf("%w: cel artifact version=%d (want %d)", ErrCompileFailed, a.Version, celArtifactSchemaVersion)
	}
	return &a, nil
}

// extractBindings walks the compiled CEL AST and returns the sorted,
// deduplicated list of top-level identifier references (e.g.
// "principal", "table", "manifest"). The walk is conservative — it
// includes any identifier the AST mentions, not just those declared
// in the env. The gateway then asks the AttributeResolver / Inputs
// builder for each named binding; missing values resolve to "" per
// the Pitfall-8 contract documented in store/attribute.go.
func extractBindings(a *cel.Ast) []string {
	if a == nil {
		return nil
	}
	seen := map[string]struct{}{}
	// cel-go v0.28 exposes the parsed AST tree via ast.NavigateAST +
	// ast.PreOrderVisit + ast.NewExprVisitor (the visitor callback
	// takes ast.Expr; we type-switch on Kind() to collect IdentKind
	// references). Select chains naturally descend through
	// PreOrderVisit so we capture the base ident of a select.
	walker := func(e ast.Expr) {
		if e.Kind() == ast.IdentKind {
			seen[e.AsIdent()] = struct{}{}
		}
	}
	navAST := ast.NavigateAST(a.NativeRep())
	ast.PreOrderVisit(navAST, ast.NewExprVisitor(walker))
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
