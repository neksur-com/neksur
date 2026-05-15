//go:build integration

// Plan 01-05 Task 3 [BLOCKING] — fail-closed semantics across the full
// engine surface (D-1.09 + VALIDATION lines 59-69).
//
// Three tests:
//
//   - TestEvaluatorFailClosedOnCELPanic — a panicking custom binding
//     triggers the defence-in-depth fail-closed path at the engine
//     layer; assert ErrEvalPanic OR ErrPolicyEvalFailed surfaces (cel-go
//     may catch the panic itself and convert to eval err — both paths
//     are fail-closed at the gateway layer per CONTEXT D-1.09). The
//     gateway-HTTP-level test for this path is TestGateway503OnCELPanic
//     (Plan 01-06; gateway_503_unavailable_test.go).
//
//   - TestEvalErrorWrapsCompileFailure — malformed CEL surfaces as
//     *EvalError wrapping ErrCompileFailed.
//
//   - TestLoadPoliciesForTableErrorBubblesUp — graph client closed
//     mid-test; LoadPoliciesForTable returns non-nil err. Plan 01-06
//     wraps this with metricRejected.WithLabelValues(
//     "policy_engine_unavailable").Inc() + 503.

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

// TestEvaluatorFailClosedOnCELPanic exercises the D-1.09 fail-closed
// path at the cel.Evaluator engine layer when a custom binding panics.
// cel-go installs its own recover that converts non-runtime.Error
// panics to eval errors; runtime.Error panics propagate to our outer
// defer/recover. Either path satisfies the fail-closed contract.
//
// Plan 01-06 deviation [Rule 1 — naming collision]: the test was
// originally named TestGateway503OnCELPanic in Plan 01-05 (which
// scoped fail-closed only to the engine layer); Plan 01-06 added a
// HTTP-level test that needed the same name. Renamed to make the
// engine-vs-HTTP distinction explicit:
//
//   - TestEvaluatorFailClosedOnCELPanic (HERE) — engine-level: assert
//     Evaluator.Evaluate returns wrapped ErrEvalPanic / ErrPolicyEvalFailed.
//   - TestGateway503OnCELPanic (gateway_503_unavailable_test.go) —
//     HTTP-level: assert the L1 gateway maps the engine error to
//     503 + commit_rejected_total{reason=policy_engine_unavailable} increment.
func TestEvaluatorFailClosedOnCELPanic(t *testing.T) {
	// Custom env with a runtime.Error-panicking binding. We use nil
	// pointer dereference because cel-go's internal recover re-throws
	// runtime.Error panics — guaranteeing the panic reaches our outer
	// defer/recover.
	env, err := cel.NewEnv(
		cel.Variable("table", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("commit", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		cel.Function("manifest.boom",
			cel.Overload("manifest_boom",
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
	comp, err := celpkg.NewCompiler(env, 4)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	ev := celpkg.NewEvaluator(comp)

	dec, err := ev.Evaluate(context.Background(),
		celpkg.Policy{ID: "p-boom", Kind: "schema", Text: "manifest.boom(table)"},
		&celpkg.Inputs{Table: map[string]any{}})

	if err == nil {
		t.Fatalf("expected error on panic; got nil decision=%+v", dec)
	}
	if dec != nil {
		t.Fatalf("expected nil decision on panic; got %+v", dec)
	}
	if !errors.Is(err, celpkg.ErrEvalPanic) && !errors.Is(err, celpkg.ErrPolicyEvalFailed) {
		t.Errorf("expected ErrEvalPanic or ErrPolicyEvalFailed; got %v", err)
	}
}

// TestEvalErrorWrapsCompileFailure — malformed CEL must surface as
// *EvalError + errors.Is ErrCompileFailed.
func TestEvalErrorWrapsCompileFailure(t *testing.T) {
	env, err := celpkg.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	comp, err := celpkg.NewCompiler(env, 4)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	ev := celpkg.NewEvaluator(comp)

	_, err = ev.Evaluate(context.Background(),
		celpkg.Policy{ID: "p-bad", Text: "this is not CEL"},
		&celpkg.Inputs{})
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	var ee *celpkg.EvalError
	if !errors.As(err, &ee) {
		t.Errorf("expected *EvalError wrap; got %T (err=%v)", err, err)
	}
	if !errors.Is(err, celpkg.ErrCompileFailed) {
		t.Errorf("errors.Is ErrCompileFailed = false; want true (err=%v)", err)
	}
}

// TestLoadPoliciesForTableErrorBubblesUp — close the graph client
// mid-test (simulates Postgres unreachable); LoadPoliciesForTable
// returns non-nil err. Plan 01-06 wraps this with the
// commit_rejected_total{reason=policy_engine_unavailable} increment +
// 503 response.
func TestLoadPoliciesForTableErrorBubblesUp(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantID = "44444444-4444-4444-8444-444444444445"
	_ = fx.ProvisionTenant(t, tenantID)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	// Close BEFORE the LoadPoliciesForTable call to simulate an
	// unavailable backend.
	gc.Close()

	tenantUUID := uuid.MustParse(tenantID)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	s := store.NewAGEStore(gc)
	policies, err := s.LoadPoliciesForTable(ctx, iceberg.TableRef{
		Namespace: []string{"test"},
		Name:      "orders",
	})
	if err == nil {
		t.Fatalf("expected error after Close; got policies=%+v", policies)
	}
	if policies != nil {
		t.Errorf("expected nil policies on err; got %+v", policies)
	}
}
