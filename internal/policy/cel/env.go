// CEL environment construction — D-1.07 process-singleton cel.Env.
//
// The cel.Env is constructed ONCE at process startup; every Compiler
// + Evaluator instance shares it. cel.Env is documented as thread-safe
// for concurrent Compile/Program calls so a single shared env is the
// correct shape.
//
// Variable declarations:
//
//   - table     map[string]any  — the full table metadata projection
//                                 (Schema, PartitionSpec, Snapshots,
//                                  Properties). Per RESEARCH §Pattern 6
//                                  lines 981-1006, declared as
//                                  MapType(StringType, DynType) so policy
//                                  authors index by string keys.
//   - commit    map[string]any  — the incoming CommitRequest (Requirements,
//                                 Updates). Same MapType shape.
//   - principal map[string]any  — the request principal (sub, roles,
//                                 token claims). Same MapType shape.
//
// Custom function bindings live in functions.go and are folded into the
// env via registerManifestFunctions().

package cel

import (
	"sync"

	"github.com/google/cel-go/cel"
)

// envOnce guards the process-singleton env so NewEnv may be called
// from multiple goroutines (e.g., the main server bootstrap + a test
// that runs in parallel) without constructing the env twice.
var (
	envOnce       sync.Once
	envSingleton  *cel.Env
	envSingletErr error
)

// NewEnv returns the process-singleton cel.Env. Constructed at most once
// per process; subsequent calls return the cached env (and any
// construction error).
//
// Per D-1.07 the env declares three input variables — `table`, `commit`,
// `principal` — all typed as MapType(StringType, DynType) so policy
// authors index by string key with no compile-time schema constraint.
// Custom function bindings (manifest.has_column / manifest.has_partition
// / principal.role) cover Pitfall 7 (CEL has no JSONPath) — see
// functions.go.
//
// Thread-safe; safe to call repeatedly. Returns the same env pointer
// every call (the env is concurrent-safe for Compile/Program by
// cel-go's documented contract).
func NewEnv() (*cel.Env, error) {
	envOnce.Do(func() {
		opts := []cel.EnvOption{
			cel.Variable("table", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("commit", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		}
		opts = append(opts, registerManifestFunctions()...)
		envSingleton, envSingletErr = cel.NewEnv(opts...)
	})
	return envSingleton, envSingletErr
}
