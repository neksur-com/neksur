// DremioInjector — sqlproxy.Injector stub for the Dremio dialect.
// Wave 2 Plan 02-05 dispatch B. Dremio support lights up in Phase 3
// (alongside Snowflake); Phase 2 ships a fail-closed stub.
//
// CR-09: previously this stub returned iceberg.ErrAdapterStub, which
// the sqlproxy server.go error switch maps to HTTP 501 'engine not
// supported'. 501 does NOT increment the policy-engine-unavailable
// counter and does not page SREs — so a tenant configured for Dremio
// would silently run queries with NO policy enforcement and zero
// alerting. A fail-closed system must translate 'engine with no
// emitter registered' to 503 + commit_rejected_total{
// reason='policy_engine_unavailable'} so the dashboard surfaces the
// rejection.
//
// The injector now returns sqlproxy.ErrPolicyEngineUnavailable so the
// server maps the request to 503 and the counter increments.

package dialect

import (
	"context"
	"fmt"

	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// DremioInjector is the Phase 2 fail-closed stub for the "dremio"
// engine kind. Zero-field by design — every call short-circuits with
// sqlproxy.ErrPolicyEngineUnavailable before touching the store or
// cache. The struct exists (rather than a bare function) for parity
// with the other dialects and so dispatch C's wiring layer can
// register it through the same BuildInjector factory.
type DremioInjector struct{}

// NewDremioInjector constructs the stub. Takes no dependencies — see
// the DremioInjector struct doc for the rationale.
func NewDremioInjector() *DremioInjector {
	return &DremioInjector{}
}

// InjectPolicy is a Phase 2 fail-closed stub: every call returns
// sqlproxy.ErrPolicyEngineUnavailable wrapped with a per-dialect
// prefix. The sqlproxy server's error switch maps this to HTTP 503
// AND increments commit_rejected_total{reason='policy_engine_unavailable'},
// so a tenant accidentally routed to a Dremio engine is fail-closed
// AND visible on the SRE dashboard (CR-09).
//
// Per Pitfall 11: no query body is read or logged on this path.
func (i *DremioInjector) InjectPolicy(_ context.Context, _ string, _ sqlproxy.TableRef, _ sqlproxy.Claims) (string, string, error) {
	return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: Phase 2 stub — failing closed: %w", sqlproxy.ErrPolicyEngineUnavailable)
}
