// BuildInjector — the per-engine sqlproxy.Injector factory. Wave 2
// Plan 02-05 dispatch B.
//
// This function used to live in `sqlproxy/injector.go` (as a stub
// returning ErrEngineNotSupported for every engine kind). Dispatch B
// moves it here so it can construct concrete injectors from this
// package without forming an import cycle (`sqlproxy` → `dialect` →
// `sqlproxy`). The parent `sqlproxy` package's injector.go now
// declares only the Injector interface + supporting types; callers
// (the neksur-server wiring layer in dispatch C) import this package
// for the factory.

package dialect

import (
	"fmt"

	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// BuildInjector returns a concrete sqlproxy.Injector for the given
// engine kind. Wave 2 Plan 02-05 introduced three kinds; Phase 3
// D-3.02 (Plan 03-05) makes the dremio arm live:
//
//   - "trino"  → NewTrinoInjector(deps.Store, deps.Cache)
//   - "spark"  → NewSparkInjector(deps.Store, deps.Cache)
//   - "dremio" → NewDremioInjector(deps.Store, deps.Cache)
//     dremio LIVE (Phase 3 D-3.02 + 03-05-PLAN): replaces Phase 2
//     fail-closed stub with real splicer; honors divergent_suspended
//     as fail-closed 503 per D-3.05. Metric: sql_proxy_inject_failures_total
//     (NOT commit_rejected_total — WR-A3).
//
// Any other engine kind returns a wrapped sqlproxy.ErrEngineNotSupported
// so callers can branch via errors.Is and the wiring layer can either
// skip the registration or surface a startup error. BigQuery /
// Databricks / Snowflake land in later dispatches; until then they
// fall through this switch.
func BuildInjector(engineKind string, deps sqlproxy.InjectorDeps) (sqlproxy.Injector, error) {
	switch engineKind {
	case "trino":
		return NewTrinoInjector(deps.Store, deps.Cache), nil
	case "spark":
		return NewSparkInjector(deps.Store, deps.Cache), nil
	case "dremio":
		return NewDremioInjector(deps.Store, deps.Cache), nil
	default:
		return nil, fmt.Errorf("sqlproxy: BuildInjector(%q): %w", engineKind, sqlproxy.ErrEngineNotSupported)
	}
}
