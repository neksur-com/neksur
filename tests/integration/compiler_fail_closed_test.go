//go:build integration

// Plan 02-04 Task BLOCKING — TestCompiler_FailClosedOnCompileError.
//
// Hands the compiler a CEL body with a syntactic parse error (missing
// closing paren after location.region(snapshot) — `snapshot` is also
// an undeclared identifier, but the parser will trip on the missing
// `)` first). Asserts:
//
//   1. CompileAll returns per-engine results where the err on each
//      result wraps ErrCompileFailed (the inner CEL compile error
//      bubbles up via CompileCELArtifact → wrapped with
//      ErrCompileFailed).
//   2. Each CompiledPolicy is persisted with status = compile_failed
//      (NOT silently omitted — the planner needs to see the marker).
//   3. LoadCompiledForTable returns the failed-status node so a
//      future fail-closed code path can deny commits when a known
//      policy has no enforceable artifact.

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	policycel "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/compiler"
	"github.com/neksur-com/neksur/internal/policy/compiler/dialect"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const failClosedTenant = "fa11c105-0204-4222-8a22-222222222222"

// TestCompiler_FailClosedOnCompileError — see file header.
func TestCompiler_FailClosedOnCompileError(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, failClosedTenant)
	_ = fx.ProvisionEngineRegistry(t, failClosedTenant,
		[]string{"trino", "spark"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Malformed CEL: `location.region(snapshot` — missing closing paren.
	// CEL parser will trip during cel.Compile before evaluation is
	// attempted.
	const policyID = "fail-closed-policy"
	const malformedCEL = `location.region(snapshot`
	// We deliberately splice the body via the schema seeder; the AGE
	// store routes through MustSanitizeCypherLiteral so we don't need
	// to escape further. (The malformed CEL has no quotes/backslashes.)
	seedPolicyOfKind(t, gc, failClosedTenant, policyID, malformedCEL,
		"orders", "test", "Policy", "residency", "RESIDENCY_GOVERNS")

	tenantUUID := uuid.MustParse(failClosedTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	celEnv, _ := policycel.NewEnv()
	celComp, _ := policycel.NewCompiler(celEnv, 16)

	comp, err := compiler.NewCompiler(compiler.CompilerConfig{
		Dialects: map[string]dialect.DialectCompiler{
			"trino": dialect.NewTrinoCompiler(),
			"spark": dialect.NewSparkCompiler(),
		},
		Probes:         compiler.NewProbeRunner(nil),
		CompiledStore:  store.NewCompiledStore(gc),
		EngineRegistry: store.NewEngineRegistry(gc),
		CELEnv:         celEnv,
		CELCompiler:    celComp,
	})
	if err != nil {
		t.Fatalf("compiler.NewCompiler: %v", err)
	}

	// PolicySource with the malformed CEL body. The CEL compile path
	// fires when DefinitionCEL is non-empty; DefinitionSQL stays empty.
	src := compiler.PolicySource{
		PolicyID:      policyID,
		PolicyKind:    "residency",
		DefinitionCEL: malformedCEL,
	}
	ref := iceberg.TableRef{Namespace: []string{"test"}, Name: "orders"}

	results, err := comp.CompileAll(ctx, src, ref)
	if err != nil {
		t.Fatalf("CompileAll returned top-level err: %v (per-engine errs should surface in Results)", err)
	}
	if len(results) != 2 {
		t.Fatalf("results: got %d; want 2", len(results))
	}
	for _, r := range results {
		if r.Status != store.CompiledPolicyStatusCompileFailed {
			t.Errorf("engine %q status = %q; want compile_failed",
				r.EngineKind, r.Status)
		}
		if r.Err == nil {
			t.Errorf("engine %q: nil err on compile_failed (want wrapped ErrCompileFailed)", r.EngineKind)
			continue
		}
		if !errors.Is(r.Err, compiler.ErrCompileFailed) {
			t.Errorf("engine %q err = %v; want errors.Is ErrCompileFailed", r.EngineKind, r.Err)
		}
	}

	// LoadCompiledForTable should return BOTH failed-status nodes (the
	// planner sees the marker; fail-closed downstream).
	cstore := store.NewCompiledStore(gc)
	loaded, err := cstore.LoadCompiledForTable(ctx, ref)
	if err != nil {
		t.Fatalf("LoadCompiledForTable: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded: got %d; want 2 failed-status nodes (silent omission would yield 0)",
			len(loaded))
	}
	for _, cp := range loaded {
		if cp.Status != store.CompiledPolicyStatusCompileFailed {
			t.Errorf("loaded CompiledPolicy(%s) status = %q; want compile_failed",
				cp.EngineKind, cp.Status)
		}
	}
}
