// Dremio dialect emitter — Phase 2 STUB (D-2.04 / Plan 02-04).
//
// Dremio's SQL flavor is close to Trino's but the live emitter is
// deferred to Phase 3 (Plan 03-XX) because:
//
//   1. Dremio's row-access-policy API is currently undocumented
//      outside of the enterprise UDF surface; the Phase 3 plan budgets
//      time for a reverse-engineering spike against a sandbox cluster.
//   2. Phase 2's reference engine is Trino; Spark is plumbed via the
//      Scala Extension. No Phase 2 customer has Dremio in their pilot
//      stack so a stub is sufficient.
//
// Behavior of the stub: every method returns ErrDialectStub (re-exported
// from this package — same sentinel as the compiler's
// ErrDialectStub). The cross-engine compiler must check for this and
// write the resulting CompiledPolicy with status=compile_failed +
// skip the probe. The CompiledPolicy node is STILL created (with an
// empty artifact_body) so that the planner can answer "is this engine
// registered for this policy?" with a deterministic graph query.
//
// To promote this stub to live in Phase 3:
//   1. Replace the body of Kind / CompileRowFilter / CompileColumnMask
//      with the live emitter, mirroring trino.go.
//   2. Remove the ErrDialectStub return from both Compile* methods.
//   3. Add a Dremio entry to the compiler's probe runner so probes
//      execute against a live Dremio sandbox.

package dialect

// DremioCompiler is the Phase 2 stub. Construct via NewDremioCompiler.
type DremioCompiler struct{}

// NewDremioCompiler returns the stub emitter.
func NewDremioCompiler() *DremioCompiler { return &DremioCompiler{} }

// Kind returns "dremio". Even the stub answers the dispatch
// question so the compiler's registry lookup succeeds (the
// ErrDialectStub return on Compile* is what signals deferred work).
func (DremioCompiler) Kind() string { return "dremio" }

// CompileRowFilter always returns ErrDialectStub.
func (DremioCompiler) CompileRowFilter(table string, predicate any) (string, error) {
	return "", ErrDialectStub
}

// CompileColumnMask always returns ErrDialectStub.
func (DremioCompiler) CompileColumnMask(table string, mask any) (string, error) {
	return "", ErrDialectStub
}
