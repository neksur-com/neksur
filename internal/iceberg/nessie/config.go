// Package nessie is the Project Nessie 0.100+ IcebergCatalogClient
// adapter (D-1.02 second live catalog) — Nessie's branch model is the
// most divergent from Polaris (which has no branching), so an adapter
// that drives Nessie cleanly through the same 6-method
// IcebergCatalogClient interface (Plan 01-02) empirically proves the
// interface is catalog-agnostic.
//
// Branching model:
//
//	Phase 1 selects the working branch declaratively at construction
//	time via Config.DefaultBranch (D-1.03). The adapter passes
//	`nessie.commit.ref=<branch>` through iceberg-go's REST catalog
//	props — iceberg-go forwards this to Nessie as the implicit ref
//	for every catalog operation. Branch is GLOBAL to the constructed
//	adapter; Phase 1 does NOT expose a runtime branch-switching API.
//	Phase 3 may add a `WithBranch(name string) IcebergCatalogClient`
//	method when multi-branch test workflows arrive (Plan 01-03 task 1
//	doc-comment).
//
// Per D-1.03 the entire catalog-side surface is declarative struct
// fields on Config; there is NO functional-options pattern and NO
// runtime capability flags. New (in adapter.go) reads from the
// struct, validates, and constructs an iceberg-go catalog client.
package nessie

import (
	"errors"
	"fmt"
)

// ErrInvalidConfig is the package sentinel returned when Validate
// finds a missing required field, an unsupported AuthMode, or a
// AuthMode that requires a value the caller didn't supply (e.g.,
// `bearer` mode without a BearerToken). Callers branch on
// errors.Is(err, nessie.ErrInvalidConfig) to distinguish a
// configuration problem from a runtime upstream-Nessie error.
var ErrInvalidConfig = errors.New("nessie: invalid config")

// Auth mode constants. Phase 1 supports `none` (test/dev — Nessie
// testcontainer runs unauthenticated) and `bearer` (production —
// static or rotating bearer token sourced from
// `tenant_<uuid>.catalog_credentials.config_json`). `aws-iam` is
// declared on the surface so Phase 3 (Nessie-on-AWS via SigV4) can
// flip it on without an interface change; for now `aws-iam` is
// rejected at Validate.
const (
	AuthModeNone   = "none"
	AuthModeBearer = "bearer"
	AuthModeAWSIAM = "aws-iam"
)

// DefaultBranch is the Nessie default branch name. When Config.DefaultBranch
// is empty the adapter falls back to this. Matches Nessie's own
// default branch name.
const DefaultBranch = "main"

// Config is the entire surface for a Nessie-backed catalog (D-1.03
// declarative). All fields are visible at construction time;
// missing-required-field bugs are caught by Validate before any
// network call.
//
// Field semantics:
//
//   - Endpoint: the base URL of the Nessie Iceberg REST API
//     (e.g., "https://nessie.customer.com" — iceberg-go appends
//     the `/v1/...` paths itself, matching Polaris adapter
//     convention; Nessie also exposes a `/api/v2/...` namespace
//     for the native Nessie REST API which the testfixture uses
//     to manage branches, but adapter operations go through
//     iceberg-go's REST catalog client which uses the standard
//     Iceberg REST shape).
//
//   - DefaultBranch: the Nessie branch every catalog operation
//     runs on. Defaults to "main" when empty. Tests use the
//     dedicated `neksur-test` branch (Pitfall 2 mitigation per
//     CONTEXT line 173); the iceberg-go REST catalog forwards
//     this as the `nessie.commit.ref` property.
//
//   - AuthMode: one of `none` | `bearer` | `aws-iam`. Phase 1
//     supports `none` and `bearer` only; `aws-iam` is declared on
//     the surface for Phase 3 forward compatibility but rejected
//     at Validate today.
//
//   - BearerToken: required when AuthMode == "bearer"; passed
//     through to iceberg-go as the `token` property (the documented
//     Iceberg REST OAuth shape for static-bearer-token auth — when
//     `token` is set, iceberg-go skips the OAuth refresh dance and
//     uses the token verbatim).
type Config struct {
	Endpoint      string
	DefaultBranch string
	AuthMode      string
	BearerToken   string
}

// Validate checks the required fields for the configured AuthMode
// and returns a wrapped ErrInvalidConfig on failure so callers can
// branch on errors.Is + still see the specific missing field in the
// message text.
//
// Validate does NOT mutate or return a normalized Config — defaults
// for DefaultBranch + AuthMode are applied by New (in adapter.go)
// before this validator runs against the populated struct. The
// Polaris pattern is identical (see polaris/config.go withDefaults +
// Validate split): Validate gates the eventual wire call; defaults
// keep the caller-visible Config struct value-pure.
func (c Config) Validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("nessie: config: Endpoint required: %w", ErrInvalidConfig)
	}
	switch c.AuthMode {
	case AuthModeNone:
		// no-op — Nessie testcontainer + many on-prem deployments
		// run unauthenticated; nothing else to check.
	case AuthModeBearer:
		if c.BearerToken == "" {
			return fmt.Errorf("nessie: config: BearerToken required for AuthMode=bearer: %w", ErrInvalidConfig)
		}
	case AuthModeAWSIAM:
		return fmt.Errorf("nessie: config: AuthMode=aws-iam deferred to Phase 3: %w", ErrInvalidConfig)
	default:
		return fmt.Errorf("nessie: config: AuthMode %q not supported (want one of none|bearer|aws-iam): %w", c.AuthMode, ErrInvalidConfig)
	}
	if c.DefaultBranch == "" {
		// Reachable only if a caller invokes Validate directly on
		// an un-defaulted Config (New applies the default before
		// calling Validate). Treat as a configuration bug.
		return fmt.Errorf("nessie: config: DefaultBranch required: %w", ErrInvalidConfig)
	}
	return nil
}

// withDefaults returns a copy of c with DefaultBranch and AuthMode
// set to their Phase 1 defaults when the caller left them empty.
// Internal helper used by New; not exported because callers should
// see Validate as the only normalization step at the surface.
func (c Config) withDefaults() Config {
	if c.DefaultBranch == "" {
		c.DefaultBranch = DefaultBranch
	}
	if c.AuthMode == "" {
		c.AuthMode = AuthModeNone
	}
	return c
}
