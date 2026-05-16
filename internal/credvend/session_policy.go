// session_policy.go — backwards-compatible re-export of the canonical
// session policy constructor from internal/credvend/sessionpolicy.
//
// Plan 02-13: the actual implementation lives in the sessionpolicy leaf
// subpackage so the polaris adapter can import it WITHOUT creating an
// import cycle (credvend imports gateway/iceberg, which imports polaris;
// putting BuildSessionPolicy directly in credvend therefore forced a
// cycle when polaris started consuming it). The leaf is purely a Go
// import-graph fix — semantically there is no change.
//
// CR-02 follow-up: this re-export is now the SINGLE home for session
// policy construction across the L4 path. The duplicated
// polaris.buildSessionPolicy + helpers in adapter.go (lines 372-466
// pre-deletion) have been deleted per WR-A5/WR-A6.
//
// See the leaf package doc comment in sessionpolicy/sessionpolicy.go
// for the threat-model and Pitfall 1 rationale.
package credvend

import (
	"github.com/neksur-com/neksur/internal/credvend/sessionpolicy"
	"github.com/neksur-com/neksur/internal/iceberg"
)

// SessionPolicy is the exported alias for the JSON-serialisable inline
// session policy document. Exported so integration tests can decode and
// inspect the structure without re-implementing the JSON shape.
type SessionPolicy = sessionpolicy.Doc

// BuildSessionPolicy re-exports sessionpolicy.Build to preserve the
// pre-02-13 public surface — callers (integration tests, future
// packages) import credvend.BuildSessionPolicy and keep working
// unchanged. The polaris adapter calls sessionpolicy.Build directly to
// avoid importing credvend (which would cycle through gateway/iceberg).
func BuildSessionPolicy(table iceberg.TableRef, region, warehouse string) ([]byte, error) {
	return sessionpolicy.Build(table, region, warehouse)
}
