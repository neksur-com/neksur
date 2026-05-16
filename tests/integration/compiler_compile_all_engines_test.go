//go:build integration

// Plan 02-04 Task BLOCKING — TestCompiler_CompileAllEngines.
//
// Verifies the cross-engine compiler emits one CompiledPolicy node per
// (Policy × Engine) pair when a CEL-bodied Policy is recompiled
// against the trino/spark/dremio engine registry. Trino + Spark land
// with `status: active` (CEL artifacts bypass the dialect emitter
// entirely); Dremio's CEL persistence is identical (CEL body is
// engine-agnostic at the artifact layer), so all three CompiledPolicy
// nodes land active for a CEL body.
//
// NOTE: The plan objective text says the Dremio row should be
// `compile_failed` via ErrDialectStub — but the compiler code (see
// compileArtifact CEL branch in compiler.go) only consults the
// dialect emitter for SQL fragments. For CEL bodies all engines share
// a single compile path. We therefore use a SQL row-filter Policy
// (kind=row_filter) so the dialect dispatch fires and Dremio's stub
// emitter returns ErrDialectStub → compile_failed marker, exactly as
// the plan describes.

package integration

import (
	"context"
	"fmt"
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

const compileAllEnginesTenant = "c0c0c0c0-0204-4111-8a11-111111111111"

// TestCompiler_CompileAllEngines — see file header.
func TestCompiler_CompileAllEngines(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, compileAllEnginesTenant)
	_ = fx.ProvisionEngineRegistry(t, compileAllEnginesTenant,
		[]string{"trino", "spark", "dremio"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Seed Policy + Table + ROW_FILTER_GOVERNS edge. The compiler
	// itself doesn't load via the edge in this test (we hand the
	// PolicySource directly), but the graph shape is what Plan 02-05's
	// trigger-driven path consumes — keeping it consistent here.
	const policyID = "compile-all-policy-1"
	const sqlBody = `region = 'us-east-1'`
	seedPolicyOfKind(t, gc, compileAllEnginesTenant, policyID, sqlBody,
		"orders", "test", "Policy", "row_filter", "ROW_FILTER_GOVERNS")

	tenantUUID := uuid.MustParse(compileAllEnginesTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	// Wire the cross-engine compiler with the live dialect set.
	celEnv, err := policycel.NewEnv()
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	celComp, err := policycel.NewCompiler(celEnv, 16)
	if err != nil {
		t.Fatalf("cel.NewCompiler: %v", err)
	}

	comp, err := compiler.NewCompiler(compiler.CompilerConfig{
		Dialects: map[string]dialect.DialectCompiler{
			"trino":  dialect.NewTrinoCompiler(),
			"spark":  dialect.NewSparkCompiler(),
			"dremio": dialect.NewDremioCompiler(),
		},
		Probes:         compiler.NewProbeRunner(nil), // probes skipped (no executor)
		CompiledStore:  store.NewCompiledStore(gc),
		EngineRegistry: store.NewEngineRegistry(gc),
		CELEnv:         celEnv,
		CELCompiler:    celComp,
	})
	if err != nil {
		t.Fatalf("compiler.NewCompiler: %v", err)
	}

	src := compiler.PolicySource{
		PolicyID:      policyID,
		PolicyKind:    "row_filter",
		DefinitionSQL: sqlBody,
	}
	ref := iceberg.TableRef{Namespace: []string{"test"}, Name: "orders"}
	results, err := comp.CompileAll(ctx, src, ref)
	if err != nil {
		t.Fatalf("CompileAll: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results: got %d; want 3 (trino+spark+dremio)", len(results))
	}

	byKind := map[string]compiler.CompileResult{}
	for _, r := range results {
		byKind[r.EngineKind] = r
	}
	if byKind["trino"].Status != store.CompiledPolicyStatusActive {
		t.Errorf("trino status = %q; want active (err=%v)", byKind["trino"].Status, byKind["trino"].Err)
	}
	if byKind["spark"].Status != store.CompiledPolicyStatusActive {
		t.Errorf("spark status = %q; want active (err=%v)", byKind["spark"].Status, byKind["spark"].Err)
	}
	if byKind["dremio"].Status != store.CompiledPolicyStatusCompileFailed {
		t.Errorf("dremio status = %q; want compile_failed (ErrDialectStub) (err=%v)",
			byKind["dremio"].Status, byKind["dremio"].Err)
	}

	// Assert the three CompiledPolicy nodes landed in the graph.
	cstore := store.NewCompiledStore(gc)
	loaded, err := cstore.LoadCompiledForTable(ctx, ref)
	if err != nil {
		t.Fatalf("LoadCompiledForTable: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("loaded CompiledPolicy nodes: got %d; want 3 (have %+v)",
			len(loaded), loaded)
	}
	_ = fmt.Sprintf // anchor for fmt import on diagnostic paths
}
