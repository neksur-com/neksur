// Sentinel errors for the cross-engine policy compiler (D-2.04 / Plan 02-04).
//
// Every wrapped error produced by the compiler / dialect emitters /
// probe runner / artifact store discriminates against one of the
// sentinels here via errors.Is + errors.Join so callers can branch on
// the failure mode without string-matching:
//
//   - ErrCompileFailed         — dialect compiler refused the input
//                                fragment (parse / lower failure).
//   - ErrProbeFailed           — synthetic probe query returned a
//                                non-zero rowcount, errored, or
//                                deadline-exceeded against the live
//                                engine.
//   - ErrDialectStub           — the dialect emitter is a Phase-2 stub
//                                (Dremio, Snowflake) — caller MUST mark
//                                the corresponding CompiledPolicy as
//                                status=compile_failed and skip the
//                                probe entirely.
//   - ErrCompiledPolicyNotFound — LoadCompiledForTable did not find a
//                                row for the (table, engine) pair —
//                                caller decides whether this is a hard
//                                fail-closed (gateway) or a soft "no
//                                policy attached" (admin tooling).
//
// Convention mirrors internal/policy/cel/errors.go: sentinels declared
// at package level, wrapped via fmt.Errorf("%w", err) or errors.Join.

package compiler

import "errors"

var (
	// ErrCompileFailed is the sentinel for any failure in the dialect
	// emitter, the SQL fragment parser, or the CEL artifact builder.
	// The compiler writes CompiledPolicy.status=compile_failed and
	// surfaces the cause via fmt.Errorf("%w", err) wrap chains so
	// errors.Is(err, ErrCompileFailed) holds.
	ErrCompileFailed = errors.New("compiler: compile failed")

	// ErrProbeFailed is the sentinel for any failure in the probe
	// roundtrip: deadline-exceeded (5s ctx.WithTimeout), engine-side
	// error response, or unexpected non-zero rowcount from a
	// `WHERE … AND 1=0` probe. The compiler writes
	// CompiledPolicy.status=probe_failed.
	ErrProbeFailed = errors.New("compiler: probe failed")

	// ErrDialectStub is returned by a dialect emitter whose live
	// implementation is deferred (Dremio, Snowflake). Callers MUST
	// treat this as a compile_failed status and skip the probe — the
	// stub cannot emit a syntactically valid SQL fragment so the
	// probe would either deadline-exceed or 4xx.
	ErrDialectStub = errors.New("compiler: dialect emitter is a stub")

	// ErrCompiledPolicyNotFound is returned by LoadCompiledForTable
	// when no CompiledPolicy node exists for the requested
	// (table, engine) pair. The gateway maps this to a fail-closed
	// 503 on the data plane; admin tooling treats it as an
	// informational "no policy attached" condition.
	ErrCompiledPolicyNotFound = errors.New("compiler: compiled policy not found")
)
