// ProbeRunner — post-compile synthetic SQL probe (D-2.04 / Plan 02-04).
//
// A successful per-engine compile is necessary but not sufficient: the
// emitted SQL fragment must also be acceptable to the live engine.
// The probe runner submits a synthetic query that exercises the
// fragment against the engine and verifies the engine returns a
// well-formed *zero-row* response (the `WHERE … AND 1=0` trick).
//
// Why zero rows: the probe must validate parse + plan but not return
// real customer data. `AND 1=0` is constant-folded by every supported
// engine into a NEVER predicate, so the planner accepts the fragment
// and the executor returns immediately with no rows. If the engine
// rejects the SQL we know the dialect emission is wrong; if the engine
// returns rows we know our 1=0 splice was lost (a bug).
//
// Timeout: every probe is wrapped in `context.WithTimeout(ctx, 5s)`.
// A probe that runs longer than that is treated as failed — the live
// query path is on the gateway's hot path and any slow engine round-
// trip would translate to a user-visible commit latency spike.
//
// The probe runner is engine-agnostic at this layer — it takes a
// ProbeExecutor interface that the per-engine client implementations
// satisfy. Phase 2 ships an in-process Trino executor (using
// trinodb/trino-go-client, already a Phase 1 dep) and a no-op stub
// for Spark/Dremio that just returns nil (their probes happen out-
// of-process via the Scala Extension / deferred Phase 3 wiring).

package compiler

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// probeTimeout is the per-probe wall-clock budget. 5 s matches the
// SQL proxy's own request budget — a probe that exceeds it would be
// indistinguishable from a slow live query.
const probeTimeout = 5 * time.Second

// ProbeExecutor abstracts the engine-side SQL submission. Each
// engine client (Trino, Spark, Dremio) wires in its own
// implementation. The cross-engine compiler holds a map[engineKind]
// ProbeExecutor populated at startup; absent entries skip the probe
// (with a `probe_skipped` log line — not a compile_failed status,
// because the artifact is still serializable and useful for offline
// analysis).
type ProbeExecutor interface {
	// Submit runs `query` against the engine and returns (rowsScanned,
	// err). Implementations MUST honor ctx cancellation — the probe
	// runner wraps every call in a 5s timeout.
	Submit(ctx context.Context, query string) (rowsScanned int, err error)
}

// ProbeRunner orchestrates per-engine probes. Construct ONCE at
// compiler startup via NewProbeRunner. Thread-safe.
type ProbeRunner struct {
	executors map[string]ProbeExecutor
}

// NewProbeRunner wraps the per-engine executor map. Pass an empty
// map (or nil) to disable probing entirely — useful in tests that
// validate the compile path but don't have live engine fixtures.
func NewProbeRunner(executors map[string]ProbeExecutor) *ProbeRunner {
	if executors == nil {
		executors = map[string]ProbeExecutor{}
	}
	return &ProbeRunner{executors: executors}
}

// Run executes a synthetic probe for the (engineKind, table,
// rowFilter) triple. Returns (nil) on success, or ErrProbeFailed
// (wrapped) on any failure. Returns nil + a soft "skipped" log
// signal (via err == nil and probeSkipped sentinel below) when no
// executor is registered.
func (r *ProbeRunner) Run(ctx context.Context, engineKind, tableFQN, rowFilter string) error {
	exec, ok := r.executors[engineKind]
	if !ok {
		// No executor → probe-skipped. Not an error: the artifact is
		// still useful and the caller logs "probe skipped" so operators
		// know to onboard the engine before relying on probe coverage.
		return nil
	}
	// `SELECT 1 FROM <table> WHERE <row_filter> AND 1=0` exercises the
	// fragment through the planner without returning rows.
	query := buildProbeQuery(tableFQN, rowFilter)

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	rows, err := exec.Submit(probeCtx, query)
	if err != nil {
		// Distinguish context-timeout from engine error for clearer logs.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: engine=%s timeout after %s", ErrProbeFailed, engineKind, probeTimeout)
		}
		return fmt.Errorf("%w: engine=%s submit: %v", ErrProbeFailed, engineKind, err)
	}
	if rows != 0 {
		return fmt.Errorf("%w: engine=%s returned %d rows (expected 0 — AND 1=0 was bypassed?)", ErrProbeFailed, engineKind, rows)
	}
	return nil
}

// buildProbeQuery composes the standard 1-row-never SQL probe. The
// `tableFQN` is the engine-side fully-qualified name (e.g.
// `iceberg.sales.orders` for Trino, `default.orders` for Spark) and
// is already trusted at this layer (constructed from registry rows
// the cross-engine compiler validated upstream).
func buildProbeQuery(tableFQN, rowFilter string) string {
	if rowFilter == "" {
		return fmt.Sprintf("SELECT 1 FROM %s WHERE 1=0", tableFQN)
	}
	return fmt.Sprintf("SELECT 1 FROM %s WHERE (%s) AND 1=0", tableFQN, rowFilter)
}
