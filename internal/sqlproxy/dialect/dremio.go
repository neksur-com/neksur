// DremioInjector — sqlproxy.Injector stub for the Dremio dialect.
// Wave 2 Plan 02-05 dispatch B. Dremio support lights up in Phase 3
// (alongside Snowflake); Phase 2 ships a stub that returns
// iceberg.ErrAdapterStub so the sqlproxy HTTP server's error switch
// maps every Dremio request to 501 Not Implemented (per the package-
// doc error-mapping table — wired in dispatch A's server.go).

package dialect

import (
	"context"
	"fmt"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// DremioInjector is the Phase 2 stub for the "dremio" engine kind.
// Zero-field by design — it carries no state because every call short-
// circuits with iceberg.ErrAdapterStub before touching the store or
// cache. The struct exists (rather than a bare function) for parity
// with the other dialects and so dispatch C's wiring layer can
// register it through the same BuildInjector factory.
type DremioInjector struct{}

// NewDremioInjector constructs the stub. Takes no dependencies — see
// the DremioInjector struct doc for the rationale.
func NewDremioInjector() *DremioInjector {
	return &DremioInjector{}
}

// InjectPolicy is a Phase 2 stub: every call returns
// iceberg.ErrAdapterStub wrapped with a per-dialect prefix. The
// sqlproxy server's error switch maps iceberg.ErrAdapterStub to
// HTTP 501, giving clients a deterministic "Dremio not implemented
// yet" signal that's distinct from transient store failures.
//
// Per Pitfall 11: no query body is read or logged on this path.
func (i *DremioInjector) InjectPolicy(_ context.Context, _ string, _ sqlproxy.TableRef, _ sqlproxy.Claims) (string, string, error) {
	return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/dremio: %w", iceberg.ErrAdapterStub)
}
