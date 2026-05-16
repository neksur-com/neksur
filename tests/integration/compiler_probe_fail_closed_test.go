//go:build integration

// Plan 02-04 Task BLOCKING — TestCompiledPolicy_ProbeFailClosed.
//
// Asserts that a probe failure (engine rejects the synthetic
// `SELECT 1 FROM <missing_table> WHERE 1=0` query) is persisted as
// `status: probe_failed` (NOT silently dropped) AND — per Pitfall 11
// in the plan — the test-scoped slog capture must NOT contain any
// probe-result-body strings (only error metadata).
//
// We use a hand-rolled ProbeExecutor that synthesises a missing-table
// error rather than starting Trino + creating a missing table, because
// the contract we're testing is the COMPILER's persist-and-log
// behaviour, not Trino's parser. The trino-go-client error string for
// "table not found" is shape-stable across versions; we mimic it
// literally so the assertion is realistic.
//
// Pitfall 11 enforcement: the slog handler writes to a bytes.Buffer
// scoped to this test goroutine. We assert that the captured log
// stream does NOT contain the probe table name as a substring — i.e.
// the compiler MUST NOT log probe-result bodies (only error
// classification metadata: engine kind, error class).

package integration

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
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

const probeFailClosedTenant = "9b09b09b-0204-4333-8a33-333333333333"

// failingProbeExecutor synthesises a "table not found" engine error
// on every Submit call. Used to drive the probe-failure code path in
// the compiler without depending on a live Trino mismatched-table
// scenario.
type failingProbeExecutor struct {
	wantTableName string
}

func (e *failingProbeExecutor) Submit(_ context.Context, query string) (int, error) {
	// Sanity: probe runner should be submitting an `AND 1=0` query.
	if !strings.Contains(query, "1=0") {
		return 0, fmt.Errorf("probe query missing AND 1=0 splice: %s", query)
	}
	// Mimic the trino-go-client "table does not exist" error shape.
	return 0, fmt.Errorf("io.trino.spi.TrinoException: line 1:15: Table 'iceberg.%s.%s' does not exist",
		"test", e.wantTableName)
}

// TestCompiledPolicy_ProbeFailClosed — see file header.
func TestCompiledPolicy_ProbeFailClosed(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, probeFailClosedTenant)
	_ = fx.ProvisionEngineRegistry(t, probeFailClosedTenant, []string{"trino"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const missingTable = "nonexistent_orders"
	const policyID = "probe-fail-row-filter"
	const sqlBody = `region = 'us-east-1'`
	seedPolicyOfKind(t, gc, probeFailClosedTenant, policyID, sqlBody,
		missingTable, "test", "Policy", "row_filter", "ROW_FILTER_GOVERNS")

	tenantUUID := uuid.MustParse(probeFailClosedTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	// Pitfall 11 capture: a test-scoped slog handler that writes to a
	// buffer instead of stderr. Restore the global default at end.
	logBuf := &bytes.Buffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	defer slog.SetDefault(prevLogger)

	celEnv, _ := policycel.NewEnv()
	celComp, _ := policycel.NewCompiler(celEnv, 16)

	probeExec := &failingProbeExecutor{wantTableName: missingTable}
	probes := compiler.NewProbeRunner(map[string]compiler.ProbeExecutor{
		"trino": probeExec,
	})

	comp, err := compiler.NewCompiler(compiler.CompilerConfig{
		Dialects: map[string]dialect.DialectCompiler{
			"trino": dialect.NewTrinoCompiler(),
		},
		Probes:         probes,
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
	ref := iceberg.TableRef{Namespace: []string{"test"}, Name: missingTable}
	results, err := comp.CompileAll(ctx, src, ref)
	if err != nil {
		t.Fatalf("CompileAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d; want 1 (trino only)", len(results))
	}
	r := results[0]
	if r.Status != store.CompiledPolicyStatusProbeFailed {
		t.Errorf("status = %q; want probe_failed (err=%v)", r.Status, r.Err)
	}

	// Persisted node still landed with probe_failed status (not silently
	// dropped).
	cstore := store.NewCompiledStore(gc)
	loaded, err := cstore.LoadCompiledForTable(ctx, ref)
	if err != nil {
		t.Fatalf("LoadCompiledForTable: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Status != store.CompiledPolicyStatusProbeFailed {
		t.Errorf("loaded = %+v; want one probe_failed node", loaded)
	}

	// Pitfall 11: probe-result body strings MUST NOT appear in logs.
	// The probe table name is our proxy — the compiler should log
	// engine kind + error class but never echo the probe SQL or table
	// reference in plaintext.
	logs := logBuf.String()
	if strings.Contains(logs, missingTable) {
		t.Errorf("Pitfall 11 violation: log capture contains probe table name %q.\nLogs:\n%s",
			missingTable, logs)
	}
	// Sanity: the probe SQL itself (SELECT 1 FROM ...) must not leak.
	if strings.Contains(logs, "SELECT 1 FROM") {
		t.Errorf("Pitfall 11 violation: log capture contains raw probe SQL.\nLogs:\n%s", logs)
	}
}
