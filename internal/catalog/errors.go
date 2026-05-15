// Sentinel errors for the per-tenant catalog credentials store.
//
// Plan 01-06 wires the L1 Catalog Gateway against this package; the
// gateway translates the sentinels to HTTP responses:
//
//   - ErrCredentialsNotFound      → 404 (no row matches `nickname` for
//                                  the calling tenant — RLS-isolated lookup).
//   - ErrCatalogKindUnsupported   → 500 (a row exists but `catalog_kind`
//                                  is not one of polaris/nessie/glue/unity;
//                                  the V0060 CHECK constraint should make
//                                  this unreachable in production).
//   - ErrConfigUnmarshal          → 500 (`config_json` is malformed —
//                                  catalog onboarding wrote a bad row;
//                                  log + alert; never expose to client).
//
// All sentinels follow the Phase 0 PATTERNS CC5 convention — wrapped via
// fmt.Errorf("%w") so callers branch on errors.Is without depending on
// the message text.

package catalog

import "errors"

// ErrCredentialsNotFound is returned by Repo.GetCatalogCredentials when
// no row matches `nickname` for the calling tenant. RLS scopes the
// SELECT to the tenant's schema, so this also fires when a different
// tenant owns a row with the same nickname (the lookup never crosses
// tenant boundaries).
var ErrCredentialsNotFound = errors.New("catalog: credentials not found")

// ErrCatalogKindUnsupported is returned by gateway.BuildAdapter when a
// row's `catalog_kind` is not one of the documented values
// (polaris/nessie/glue/unity). Defence-in-depth — V0060's CHECK
// constraint should make this unreachable, but a future migration that
// relaxes the constraint would otherwise silently fall through to a nil
// adapter at the gateway.
var ErrCatalogKindUnsupported = errors.New("catalog: unsupported kind")

// ErrConfigUnmarshal is returned when the per-row `config_json` cannot
// be unmarshalled into the kind-specific Config struct (polaris.Config,
// nessie.Config, glue_stub.Config, unity_stub.Config). Catalog
// onboarding (Plan 01-09 admin CLI) writes the JSON; a malformed row
// indicates a provisioning bug, not a runtime issue — log + alert.
var ErrConfigUnmarshal = errors.New("catalog: config_json unmarshal failed")
