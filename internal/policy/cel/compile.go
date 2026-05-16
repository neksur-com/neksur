// CEL Compiler with LRU compile cache — D-1.07 hot-path optimization.
//
// Cold compile is ~5-15ms (parse + AST + program assembly); warm eval is
// ~5-50µs (interpreter dispatch only). The L1 gateway calls into this
// package on every commit, so caching compiled programs is load-bearing
// for the <5% overhead target (ADR-003 §3 performance target).
//
// Cache shape:
//
//   - Key: (PolicyID, SHA-256(text)). The text hash invalidates entries
//     when a Policy node's `definition_cel` is updated — the new
//     compile-or-get call hashes the new text, misses the cache, and
//     populates a fresh entry. Old entries age out via LRU.
//
//   - Value: cel.Program (the compiled, optimized program ready for
//     ContextEval). Program is documented thread-safe for concurrent
//     evaluation.
//
//   - Backing store: hashicorp/golang-lru/v2.Cache[K,V] — generic-typed,
//     thread-safe, non-blocking. Default size 4096 entries (covers ~all
//     P1/P2/P3 policies a Phase 1 tenant authors; more than that and the
//     LRU eviction is the natural pressure-relief).

package cel

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/google/cel-go/cel"
	lru "github.com/hashicorp/golang-lru/v2"
)

// defaultCacheSize is the LRU cache capacity used when NewCompiler is
// called with size <= 0. 4096 is the hashicorp/golang-lru/v2 default
// and matches RESEARCH §Pattern 6 line 1032.
const defaultCacheSize = 4096

// programInterruptCheckFrequency is the cel-go opcode-count budget
// between interrupt checks. Per Phase 2 RESEARCH Pitfall 6 (line 776):
// cel-go has no resource budget by default — a CEL `.all()` / `.exists()`
// over a 10K-element list runs unbounded without InterruptCheckFrequency.
// Phase 1 didn't need this (P1/P2/P3 policies don't iterate); Phase 2's
// ABAC + classification bindings DO iterate over claims arrays, so
// retrofit is required BEFORE adding new bindings (Plan 02-03).
//
// 100 opcodes is the cel-go recommended frequency per [CITED: cel-go
// ContextEval docs](https://pkg.go.dev/github.com/google/cel-go/cel) —
// frequent enough to interrupt within 1-2ms of context cancellation,
// rare enough to avoid measurable hot-path overhead on small policies.
const programInterruptCheckFrequency = 100

// cacheKey is the LRU key shape: (PolicyID, SHA-256(text)). Both fields
// are comparable (string + [32]byte) so the struct itself is comparable,
// which is required for golang-lru/v2's generic K constraint.
//
// Hashing the text (not just the PolicyID) is the load-bearing
// invalidation mechanism: when a policy's CEL text is updated, the new
// text's SHA-256 produces a fresh key — the old compile is left in the
// cache to age out via LRU. There is no stale-program window.
type cacheKey struct {
	PolicyID string
	ASTHash  [32]byte
}

// Compiler is the per-process CEL compiler with LRU-backed compile cache.
// Construct ONCE per process via NewCompiler; share the instance across
// all evaluators. Thread-safe — the underlying lru.Cache is concurrent.
type Compiler struct {
	env   *cel.Env
	cache *lru.Cache[cacheKey, cel.Program]
}

// NewCompiler constructs a Compiler against the given env. size is the
// LRU capacity in entries; values <= 0 default to defaultCacheSize (4096).
//
// Returns an error only if the underlying lru.New construction fails
// (which only happens when size is negative AFTER defaulting — i.e.,
// effectively never).
func NewCompiler(env *cel.Env, size int) (*Compiler, error) {
	if size <= 0 {
		size = defaultCacheSize
	}
	cache, err := lru.New[cacheKey, cel.Program](size)
	if err != nil {
		return nil, fmt.Errorf("cel: new lru cache: %w", err)
	}
	return &Compiler{env: env, cache: cache}, nil
}

// CompileOrGet returns the compiled cel.Program for the given policy
// text. Hot path (cache hit): returns in microseconds. Cold path:
// compiles + caches + returns (~5-15ms).
//
// Errors wrap ErrCompileFailed via errors.Join when the policy text is
// not valid CEL — callers MUST treat any error here as fail-closed
// (D-1.09).
//
// Thread-safe: the underlying lru.Cache is concurrent. Two goroutines
// asking for the same uncached (policyID, text) simultaneously may both
// compile (a small wasted CPU cost), but cel.Env.Compile + Program are
// thread-safe so the result is correct.
func (c *Compiler) CompileOrGet(policyID, text string) (cel.Program, error) {
	h := sha256.Sum256([]byte(text))
	key := cacheKey{PolicyID: policyID, ASTHash: h}
	if p, ok := c.cache.Get(key); ok {
		return p, nil
	}
	// Cold path. env.Compile returns (*cel.Ast, *cel.Issues); Issues is
	// non-nil but Issues.Err() is nil when there are no errors.
	ast, issues := c.env.Compile(text)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("cel: compile policy %s: %w",
			policyID, errors.Join(ErrCompileFailed, issues.Err()))
	}
	// Pitfall 6 retrofit (Phase 2 RESEARCH line 776): pass
	// InterruptCheckFrequency so Program.ContextEval honors context
	// cancellation / deadline-exceeded mid-evaluation. Without this the
	// runtime cannot interrupt a `.all()` over a 10K-element ABAC claims
	// array — the gateway event loop blocks until the loop finishes.
	//
	// CR-A2 closure (Plan 02-11): also pass principalAttributeDecorator()
	// — a CustomDecorator ProgramOption that wraps every
	// `principal.attribute(principal, name)` call site with an
	// activation-aware Interpretable so the AttributeResolver stashed
	// under activation["__resolver"] (eval.go:217-220) is actually
	// reached, making Layers 2 (graph) + 3 (tenant defaults) of D-2.10
	// observable from the CEL expression. Without this decorator the
	// binding was Layer-1-only — iteration-2 review finding CR-A2.
	prog, err := c.env.Program(ast,
		cel.InterruptCheckFrequency(programInterruptCheckFrequency),
		principalAttributeDecorator(),
	)
	if err != nil {
		return nil, fmt.Errorf("cel: program policy %s: %w",
			policyID, errors.Join(ErrCompileFailed, err))
	}
	// Add() may evict the LRU's tail if at capacity — that's correct
	// behavior; the next call for the evicted entry recompiles.
	c.cache.Add(key, prog)
	return prog, nil
}
