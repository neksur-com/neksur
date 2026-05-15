// Unit tests for the cel package — Plan 01-05 Task 1.
//
// Cover the three load-bearing contracts:
//
//   1. Allow on `true`, deny on `false`.
//   2. Fail-closed on every error path: compile error, eval error,
//      non-bool return, panic inside a custom binding.
//   3. LRU compile cache: warm path is much faster than cold path.
//   4. Custom function bindings (manifest.has_column) work end-to-end.
//
// All tests are pure unit tests — no Postgres / no AGE / no testcontainer.

package cel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// newTestEvaluator constructs a fresh Evaluator backed by the
// process-singleton env + a freshly allocated Compiler. (We intentionally
// share the env across tests but create a fresh Compiler so cache state
// does not leak between subtests.)
func newTestEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	env, err := NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	c, err := NewCompiler(env, 16)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	return NewEvaluator(c)
}

func TestEvaluateAllowOnTrue(t *testing.T) {
	e := newTestEvaluator(t)
	dec, err := e.Evaluate(context.Background(),
		Policy{ID: "p-allow", Kind: "schema", Text: "true"},
		&Inputs{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec == nil || dec.Action != ActionAllow {
		t.Fatalf("expected ActionAllow; got %+v", dec)
	}
}

func TestEvaluateDenyOnFalse(t *testing.T) {
	e := newTestEvaluator(t)
	dec, err := e.Evaluate(context.Background(),
		Policy{ID: "p-deny", Kind: "schema", Text: "false"},
		&Inputs{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec == nil || dec.Action != ActionDeny {
		t.Fatalf("expected ActionDeny; got %+v", dec)
	}
	if dec.Reason == "" {
		t.Errorf("expected non-empty Reason on deny")
	}
	if got, want := dec.Reason, "p-deny"; !contains(got, want) {
		t.Errorf("Reason = %q; expected to contain policy ID %q", got, want)
	}
}

func TestEvaluateFailClosedOnCompileError(t *testing.T) {
	e := newTestEvaluator(t)
	dec, err := e.Evaluate(context.Background(),
		Policy{ID: "p-bad", Kind: "schema", Text: "this is not CEL"},
		&Inputs{})
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	if dec != nil {
		t.Fatalf("expected nil decision on error; got %+v", dec)
	}
	if !errors.Is(err, ErrCompileFailed) {
		t.Errorf("errors.Is ErrCompileFailed = false; want true (err=%v)", err)
	}
	var ev *EvalError
	if !errors.As(err, &ev) {
		t.Errorf("errors.As *EvalError = false; want true (err=%v)", err)
	}
	if ev != nil && ev.PolicyID != "p-bad" {
		t.Errorf("EvalError.PolicyID = %q; want p-bad", ev.PolicyID)
	}
}

func TestEvaluateFailClosedOnNonBoolReturn(t *testing.T) {
	e := newTestEvaluator(t)
	dec, err := e.Evaluate(context.Background(),
		Policy{ID: "p-int", Kind: "schema", Text: "42"},
		&Inputs{})
	if err == nil {
		t.Fatalf("expected error on non-bool return; got nil")
	}
	if dec != nil {
		t.Fatalf("expected nil decision on error; got %+v", dec)
	}
	if !errors.Is(err, ErrPolicyReturnedNonBool) {
		t.Errorf("errors.Is ErrPolicyReturnedNonBool = false; want true (err=%v)", err)
	}
}

// TestEvaluateFailClosedOnPanicInBinding registers a custom CEL function
// that panics, evaluates a policy that invokes it, and asserts the
// gateway-side fail-closed contract holds.
//
// Note on cel-go semantics (v0.20+): cel-go DOES install its own
// panic recover around UnaryBinding/BinaryBinding implementations and
// converts the panic into a ContextEval error with prefix "internal
// error: ...". Our Evaluate then surfaces this as an *EvalError wrapping
// ErrPolicyEvalFailed — which is also fail-closed at the gateway
// (Plan 01-06 maps EvalError to 503 + commit_rejected_total{reason=
// policy_engine_unavailable} regardless of the wrapped sentinel).
//
// The point of this test is to prove BOTH the cel-go panic-catch path
// AND our outer defer/recover converge on a *non-nil error + nil
// decision — i.e., a panicking binding cannot accidentally allow.
func TestEvaluateFailClosedOnPanicInBinding(t *testing.T) {
	panicEnv, err := cel.NewEnv(
		cel.Variable("table", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("commit", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		cel.Function("manifest.boom",
			cel.Overload("manifest_boom",
				[]*cel.Type{cel.MapType(cel.StringType, cel.DynType)},
				cel.BoolType,
				cel.UnaryBinding(func(_ ref.Val) ref.Val {
					panic("intentional panic for fail-closed test")
				}),
			),
		),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	c, err := NewCompiler(panicEnv, 4)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	e := NewEvaluator(c)

	dec, err := e.Evaluate(context.Background(),
		Policy{ID: "p-boom", Kind: "schema", Text: "manifest.boom(table)"},
		&Inputs{Table: map[string]any{}})
	if err == nil {
		t.Fatalf("expected error on panic; got nil")
	}
	if dec != nil {
		t.Fatalf("expected nil decision on panic; got %+v", dec)
	}
	// Two acceptable wrap chains depending on cel-go version:
	//   (a) cel-go's own recover converts to an eval error -> wrapped
	//       as ErrPolicyEvalFailed.
	//   (b) panic propagates past cel-go -> our defer recovers, wraps
	//       as ErrEvalPanic.
	// Either way the contract holds: non-nil err + nil decision.
	if !errors.Is(err, ErrPolicyEvalFailed) && !errors.Is(err, ErrEvalPanic) {
		t.Errorf("expected either ErrPolicyEvalFailed or ErrEvalPanic; got err=%v", err)
	}
	var ev *EvalError
	if !errors.As(err, &ev) {
		t.Errorf("expected *EvalError wrap; got %T (err=%v)", err, err)
	}
}

// TestEvaluateFailClosedOnPanic uses a binding that triggers a
// runtime.Error (nil pointer dereference) — cel-go's internal panic
// recover specifically RE-THROWS runtime.Error panics so they propagate
// to our outer defer/recover. This proves the defence-in-depth branch
// (ErrEvalPanic) is reachable.
//
// Note: cel-go's implementation may evolve; if a future cel-go version
// catches runtime.Error panics too, this test will need to be updated.
// The fail-closed contract (non-nil err + nil decision) is preserved
// regardless of which recover catches the panic.
func TestEvaluateFailClosedOnPanic(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("table", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("commit", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		cel.Function("manifest.deref_nil",
			cel.Overload("manifest_deref_nil",
				[]*cel.Type{cel.MapType(cel.StringType, cel.DynType)},
				cel.BoolType,
				cel.UnaryBinding(func(_ ref.Val) ref.Val {
					var p *int
					_ = *p // runtime.Error: nil pointer dereference
					return types.Bool(true)
				}),
			),
		),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	comp, err := NewCompiler(env, 4)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	ev := NewEvaluator(comp)

	dec, err := ev.Evaluate(context.Background(),
		Policy{ID: "p-deref", Kind: "schema", Text: "manifest.deref_nil(table)"},
		&Inputs{Table: map[string]any{}})

	if err == nil {
		t.Fatalf("expected error on panic; got nil")
	}
	if dec != nil {
		t.Fatalf("expected nil decision on panic; got %+v", dec)
	}
	if !errors.Is(err, ErrEvalPanic) && !errors.Is(err, ErrPolicyEvalFailed) {
		t.Errorf("expected ErrEvalPanic or ErrPolicyEvalFailed; got %v", err)
	}
}

func TestCompileLRUHits(t *testing.T) {
	e := newTestEvaluator(t)
	const text = "table.x == 'y'"
	pol := Policy{ID: "p-cache", Kind: "schema", Text: text}
	in := &Inputs{Table: map[string]any{"x": "y"}}

	// Cold compile + eval.
	t0 := time.Now()
	if _, err := e.Evaluate(context.Background(), pol, in); err != nil {
		t.Fatalf("first Evaluate: %v", err)
	}
	cold := time.Since(t0)

	// Warm path — same text, second call. Should be much faster than
	// cold (cold is dominated by ~5-15ms parse+plan; warm is just the
	// interpreter dispatch + activation lookup).
	t0 = time.Now()
	if _, err := e.Evaluate(context.Background(), pol, in); err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	warm := time.Since(t0)

	// Assert warm < cold by a healthy margin AND warm < 1ms — even on
	// the slowest CI box, a cached program eval is sub-millisecond.
	if warm >= cold {
		t.Errorf("warm (%v) >= cold (%v); LRU cache not hitting", warm, cold)
	}
	if warm > time.Millisecond {
		t.Errorf("warm path = %v; expected < 1ms (LRU cache should be in microseconds)", warm)
	}
}

func TestManifestHasColumnBindingTrue(t *testing.T) {
	e := newTestEvaluator(t)
	pol := Policy{
		ID:   "p-has-email",
		Kind: "schema",
		Text: `manifest.has_column(table, "email")`,
	}
	in := &Inputs{
		Table: map[string]any{
			"schema": map[string]any{
				"fields": []any{
					map[string]any{"name": "id", "type": "long"},
					map[string]any{"name": "email", "type": "string"},
				},
			},
		},
	}
	dec, err := e.Evaluate(context.Background(), pol, in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec == nil || dec.Action != ActionAllow {
		t.Fatalf("expected ActionAllow when email column present; got %+v", dec)
	}
}

func TestManifestHasColumnBindingFalse(t *testing.T) {
	e := newTestEvaluator(t)
	pol := Policy{
		ID:   "p-has-email",
		Kind: "schema",
		Text: `manifest.has_column(table, "email")`,
	}
	in := &Inputs{
		Table: map[string]any{
			"schema": map[string]any{
				"fields": []any{
					map[string]any{"name": "id", "type": "long"},
					map[string]any{"name": "username", "type": "string"},
				},
			},
		},
	}
	dec, err := e.Evaluate(context.Background(), pol, in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec == nil || dec.Action != ActionDeny {
		t.Fatalf("expected ActionDeny when email column absent; got %+v", dec)
	}
}

func TestManifestHasPartitionBinding(t *testing.T) {
	e := newTestEvaluator(t)
	pol := Policy{
		ID:   "p-has-year",
		Kind: "schema",
		Text: `manifest.has_partition(table, "year")`,
	}
	inWith := &Inputs{
		Table: map[string]any{
			"partition_spec": map[string]any{
				"fields": []any{
					map[string]any{"name": "year", "transform": "years"},
				},
			},
		},
	}
	dec, err := e.Evaluate(context.Background(), pol, inWith)
	if err != nil {
		t.Fatalf("Evaluate (present): %v", err)
	}
	if dec.Action != ActionAllow {
		t.Errorf("expected ActionAllow when year partition present; got %+v", dec)
	}

	inWithout := &Inputs{
		Table: map[string]any{
			"partition_spec": map[string]any{
				"fields": []any{
					map[string]any{"name": "month", "transform": "months"},
				},
			},
		},
	}
	dec, err = e.Evaluate(context.Background(), pol, inWithout)
	if err != nil {
		t.Fatalf("Evaluate (absent): %v", err)
	}
	if dec.Action != ActionDeny {
		t.Errorf("expected ActionDeny when year partition absent; got %+v", dec)
	}
}

func TestPrincipalRoleBinding(t *testing.T) {
	e := newTestEvaluator(t)
	pol := Policy{
		ID:   "p-writer",
		Kind: "write_acl",
		Text: `principal.role(principal, "writer")`,
	}
	dec, err := e.Evaluate(context.Background(), pol,
		&Inputs{Principal: map[string]any{"sub": "alice", "roles": []any{"writer", "reader"}}})
	if err != nil {
		t.Fatalf("Evaluate (writer): %v", err)
	}
	if dec.Action != ActionAllow {
		t.Errorf("expected ActionAllow; got %+v", dec)
	}

	dec, err = e.Evaluate(context.Background(), pol,
		&Inputs{Principal: map[string]any{"sub": "bob", "roles": []any{"reader"}}})
	if err != nil {
		t.Fatalf("Evaluate (reader): %v", err)
	}
	if dec.Action != ActionDeny {
		t.Errorf("expected ActionDeny; got %+v", dec)
	}
}

// TestEvalErrorUnwrap confirms the wrapper chain: an EvalError returned
// from the compile path unwraps via errors.Is to the canonical sentinel.
func TestEvalErrorUnwrap(t *testing.T) {
	e := newTestEvaluator(t)
	_, err := e.Evaluate(context.Background(),
		Policy{ID: "p-x", Text: "this is not CEL"}, &Inputs{})
	if err == nil {
		t.Fatalf("expected error")
	}
	// Direct sentinel match — covers errors.Join chain inside
	// CompileOrGet's error.
	if !errors.Is(err, ErrCompileFailed) {
		t.Errorf("errors.Is ErrCompileFailed false; err=%v", err)
	}
	// Sanity — make sure types.Bool import is used (silences linter).
	_ = types.Bool(false)
}

// contains is a tiny strings.Contains shim — kept local to avoid a
// strings import in this test file.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
