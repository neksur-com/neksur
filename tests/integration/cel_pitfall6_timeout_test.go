//go:build integration

// Plan 02-01 Wave-0 BLOCKING — Pitfall 6 cel-go interrupt retrofit.
//
// TestCEL_Pitfall6_TimeoutTriggers503 proves that the Pitfall 6
// retrofit (compile.go cel.InterruptCheckFrequency + eval.go
// context.WithTimeout) actually interrupts an unbounded CEL
// computation within the 100ms EvalTimeout budget, surfaces a
// non-nil error wrapping ErrPolicyEvalFailed (cel-go converts
// context cancellation into an eval error), and preserves the
// D-1.09 fail-closed contract (gateway maps to 503 +
// commit_rejected_total{reason="policy_engine_unavailable"}).
//
// The pathology: a CEL expression that iterates over a 10K-element
// list with .all() over a sleep-equivalent — cel-go's interpreter
// must respect InterruptCheckFrequency or this loops far beyond the
// 100ms budget. The previous (Phase 1) behavior had NO budget — the
// gateway event loop would block until the loop completed (~500ms+ on
// a Phase 2 ABAC binding iterating a real claims array).
//
// Note: this is an INTEGRATION test even though it doesn't require a
// container — it lives in tests/integration/ alongside the other
// Phase 2 Wave-0 stubs so the BLOCKING gate is co-located with the
// related test files. The `//go:build integration` tag matches.

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neksur-com/neksur/internal/policy/cel"
)

func TestCEL_Pitfall6_TimeoutTriggers503(t *testing.T) {
	env, err := cel.NewEnv()
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	compiler, err := cel.NewCompiler(env, 16)
	if err != nil {
		t.Fatalf("cel.NewCompiler: %v", err)
	}
	evaluator := cel.NewEvaluator(compiler)

	// Build a CEL expression with nested iteration: the outer .all()
	// over a 2K-element claims array invokes a string-concat + contains
	// over EVERY element of the same 2K array. That's 4M concat+contains
	// operations — comfortably >500ms without an interrupt budget. With
	// the Pitfall 6 retrofit, cel-go interrupts at the 100ms
	// EvalTimeout deadline (InterruptCheckFrequency=100 opcodes between
	// interrupt checks → ~1-5ms interrupt granularity in practice).
	//
	// The shape mirrors what a real Phase 2 ABAC binding could end up
	// with — `principal.attribute("groups").all(g, g in principal.roles)`
	// is a perfectly valid expression a policy author might write that
	// quadratically blows up on large claims arrays.
	// Pathology: outer `.all()` predicate uses a NEVER-true inner
	// `.exists()` that scans every element. Trick: each outer `r` is
	// guaranteed to be in the array (it IS the array we're iterating!),
	// so the inner `.exists(r2, r2 == r)` always finds a match — but
	// only AFTER scanning every element up to and including itself.
	// Combined with the `false` tail of the boolean AND, the outer .all
	// keeps going through every element. Net: ~roles.size()^2 / 2
	// element-comparisons over LONG strings.
	//
	// We use `r2 == r && r2 != r` — the && short-circuits to false,
	// so the inner `.exists` is always false; the outer `.all` then
	// short-circuits to false on the FIRST element. To prevent that,
	// we use an outer `.all` over a NEVER-trues predicate that DOES
	// touch every element (boolean OR with a false tail):
	//
	// `roles.all(r, roles.exists(r2, r2 == r) || roles.exists(r2, false))`
	//
	// The first .exists fires once per outer element (short-circuit on
	// match) — but Phase 2 ABAC bindings will iterate fully, so we
	// need both halves to iterate; force a never-true second .exists
	// and OR — the `||` short-circuits on the first true. Use AND to
	// force eval of BOTH:
	//
	// `roles.all(r, roles.exists(r2, r2 == r) && !roles.all(r2, r2 == "nope"))`
	//
	// Here:
	//   - outer .all over `roles`: every element of roles.
	//   - inner .exists(r2, r2 == r): always true (short-circuits early
	//     but ≤ size iters per outer call).
	//   - inner !.all(r2, r2 == "nope"): always true (negation of
	//     all-elements-equal-to-nope which is FALSE because no element
	//     is "nope"); the all() visits every element looking for a
	//     mismatch to short-circuit on. Each element is "role-xxxxx..."
	//     so r2 == "nope" is always false → all() returns false → ! is
	//     true → outer && holds → outer .all keeps iterating.
	//   - The all() inside doesn't short-circuit because it's
	//     all-false (.all returns false on first false, which is
	//     element 0 — so this whole pathology DOES short-circuit fast.)
	//
	// Let me use the simpler iteration pattern: outer .all where the
	// predicate compares each pair of elements via concat. Force NO
	// short-circuit by checking r itself ends with a specific tail
	// — true for every element so .all visits every element. The
	// inner workload is the pair-comparison via concat.
	//
	// `roles.all(r, r.endsWith("xxx") && roles.all(r2, r2.endsWith("xxx")))`
	//
	// This is roles.size() * roles.size() operations: O(N^2) endsWith
	// over 250-char strings. On 5K elements: 25M endsWith calls.
	const pathologyExpr = `principal.roles.all(r, r.endsWith("xxx") && principal.roles.all(r2, r2.endsWith("xxx")))`

	// Seed a 5K-element principal.roles array of LONG strings that all
	// end with "xxx" — every endsWith returns true so .all visits every
	// element. Long strings (250 chars) make each endsWith proportionally
	// more expensive.
	roles := make([]any, 5000)
	for i := range roles {
		roles[i] = "role-" + strings.Repeat("a", 250) + "xxx"
	}

	pol := cel.Policy{ID: "p-pitfall6", Kind: "abac", Text: pathologyExpr}
	in := &cel.Inputs{
		Principal: map[string]any{
			"sub":   "alice",
			"roles": roles,
		},
	}

	start := time.Now()
	dec, err := evaluator.Evaluate(context.Background(), pol, in)
	elapsed := time.Since(start)

	// CONTRACT 1: the evaluator MUST return within ~150ms — the 100ms
	// EvalTimeout plus a ~50ms slack for interrupt-check granularity.
	// A failure here means the retrofit didn't actually wire up.
	const budget = 150 * time.Millisecond
	if elapsed > budget {
		t.Errorf("evaluate took %s; want <%s (Pitfall 6 retrofit didn't trigger)", elapsed, budget)
	}

	// CONTRACT 2: the evaluator MUST return a non-nil error with a
	// nil decision (fail-closed). The gateway translates this to 503.
	if err == nil {
		t.Fatalf("expected error on timeout; got nil (decision=%+v)", dec)
	}
	if dec != nil {
		t.Errorf("expected nil decision on timeout; got %+v", dec)
	}

	// CONTRACT 3: the error MUST be wrapped as *EvalError and
	// errors.Is into one of {ErrPolicyEvalFailed, ErrEvalPanic}.
	// cel-go converts context.DeadlineExceeded into a runtime eval
	// error — surfaces as ErrPolicyEvalFailed via the eval.go wrap.
	// If a future cel-go version routes deadlines through a different
	// error path, ErrEvalPanic is an acceptable fallback (the panic
	// recovery path also captures runtime errors). Both surface as 503
	// per Plan 01-06 gateway translator.
	var evErr *cel.EvalError
	if !errors.As(err, &evErr) {
		t.Errorf("expected *cel.EvalError wrap; got %T (err=%v)", err, err)
	}
	if !errors.Is(err, cel.ErrPolicyEvalFailed) && !errors.Is(err, cel.ErrEvalPanic) {
		t.Errorf("expected ErrPolicyEvalFailed or ErrEvalPanic; got err=%v", err)
	}

	t.Logf("Pitfall 6 retrofit OK: evaluator returned %s with %T", elapsed, err)
}
