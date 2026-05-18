// Package polaris is the reference IcebergCatalogClient adapter
// (D-1.02) — Apache Polaris 1.4.0 via iceberg-go's REST catalog
// client with OAuth client-credentials grant + automatic token
// refresh (Pitfall 1 mitigation).
//
// Per D-1.03 the entire catalog-side surface is declarative struct
// fields on Config; there is NO functional-options pattern and NO
// runtime capability flags. New (in adapter.go) reads from the
// struct, validates, and constructs an iceberg-go catalog client.
package polaris

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// ErrInvalidConfig is the package sentinel returned when Validate
// finds a missing or empty required field. Callers branch on
// errors.Is(err, polaris.ErrInvalidConfig) to distinguish a
// configuration problem from a runtime upstream-Polaris error.
var ErrInvalidConfig = errors.New("polaris: invalid config")

// Config is the entire surface for a Polaris-backed catalog (D-1.03
// declarative). All fields are visible at construction time;
// missing-required-field bugs are caught by Validate before any
// network call.
//
// Field semantics:
//
//   - Endpoint: the base URL of the Polaris Iceberg REST API
//     (e.g., "https://polaris.customer.com/api/catalog"). The OAuth
//     token endpoint is derived as Endpoint + "/v1/oauth/tokens"
//     per Iceberg REST OpenAPI; iceberg-go's REST catalog issues
//     token requests there automatically when given the OAuth
//     props (Pitfall 1).
//
//   - Warehouse: the Polaris warehouse name. Polaris is multi-tenant
//     internally — each warehouse maps to a customer-managed S3
//     bucket prefix.
//
//   - ClientID / ClientSecret: OAuth client-credentials grant
//     identity. Polaris bootstraps these via its admin-level
//     Principal-creation API (Phase 0.5 testfixture pre-seeds them
//     via POLARIS_BOOTSTRAP_CREDENTIALS at container boot for
//     hermetic tests).
//
//   - Scope: the OAuth scope string. Polaris uses the
//     "PRINCIPAL_ROLE:ALL" idiom by default (granting every
//     principal-role assigned to the client). Empty defaults to
//     this.
//
//   - CredentialMode: passed through to the upstream catalog as
//     the X-Iceberg-Access-Delegation header. Phase 1 = "passthrough"
//     (Spark drives writes with its own AWS credentials); Phase 2
//     L4 will switch to "vended-credentials" (Polaris's STS issues
//     short-lived per-table credentials, gated by the gateway).
//     Empty defaults to "passthrough" (D-1.02 Phase 1 contract).
type Config struct {
	Endpoint       string
	Warehouse      string
	ClientID       string
	ClientSecret   string
	Scope          string
	CredentialMode string

	// BaseTransportWrap is an OPTIONAL hook that wraps the underlying
	// http.Transport before the sessionPolicyTransport composes over it.
	// The composition order at New() time is:
	//
	//   iceberg-go.sessionTransport
	//      → sessionPolicyTransport (injects X-Iceberg-Session-Policy)
	//          → BaseTransportWrap(http.DefaultTransport.Clone())  ← THIS
	//
	// Production callers leave this nil — the adapter then uses a fresh
	// http.DefaultTransport.Clone() as the base. Integration tests use
	// this hook to inject a recording RoundTripper that captures the
	// outbound request headers reaching Polaris, so the test can assert
	// the X-Iceberg-Session-Policy + X-Iceberg-Access-Delegation values
	// without depending on Polaris debug log format.
	//
	// The hook is on Config (declarative) rather than a functional-option
	// per D-1.03 — every field of the adapter surface is visible on the
	// struct at construction time. Production-runtime nil is the default;
	// no behavior change for non-test callers.
	BaseTransportWrap func(http.RoundTripper) http.RoundTripper

	// CompactionCoordinator is an OPTIONAL L3 compaction coordinator that is
	// consulted before executing ExpireSnapshots. When non-nil, the adapter
	// calls GuardExpireSnapshots to partition candidate snapshot IDs into
	// allowed (safe to expire) and blocked (retained by an active SnapshotPin).
	// Only the allowed IDs are passed to iceberg-go's actual expiration call.
	//
	// When nil (L1+L2 binaries): ExpireSnapshots runs unmodified — full candidate
	// set is expired without consulting any pin store (no false protection).
	//
	// This field is on Config (declarative per D-1.03) rather than a
	// functional-option so all adapter configuration is visible at construction
	// time. Production wiring (Plan 03-13) sets this when the neksur-enterprise
	// module is present and the license permits "compaction_coordination".
	CompactionCoordinator iceberg.CompactionCoordinator
}

// Validate checks the four required fields are non-empty. Defaults
// for Scope + CredentialMode are NOT applied here (callers may
// inspect a returned Config to see what they passed); New (in
// adapter.go) is responsible for applying the empty-string defaults
// before constructing the iceberg-go client.
//
// Returns an error wrapped around ErrInvalidConfig so callers can
// branch on errors.Is + still see the specific missing field in
// the message text.
func (c Config) Validate() error {
	switch {
	case c.Endpoint == "":
		return fmt.Errorf("polaris: config: Endpoint required: %w", ErrInvalidConfig)
	case c.ClientID == "":
		return fmt.Errorf("polaris: config: ClientID required: %w", ErrInvalidConfig)
	case c.ClientSecret == "":
		return fmt.Errorf("polaris: config: ClientSecret required: %w", ErrInvalidConfig)
	case c.Warehouse == "":
		return fmt.Errorf("polaris: config: Warehouse required: %w", ErrInvalidConfig)
	}
	// WR-07: the OAuth `credential` prop is constructed as
	// `ClientID + ":" + ClientSecret`; iceberg-go splits on the FIRST
	// colon. A ClientSecret containing `:` would silently truncate at
	// the wrong position. Reject at config-Validate time so the bug
	// is caught at boot, not at first commit. ClientID can ALSO not
	// contain `:` for the same reason.
	if strings.Contains(c.ClientID, ":") {
		return fmt.Errorf("polaris: config: ClientID must not contain ':' (OAuth credential ambiguity): %w", ErrInvalidConfig)
	}
	if strings.Contains(c.ClientSecret, ":") {
		return fmt.Errorf("polaris: config: ClientSecret must not contain ':' (OAuth credential ambiguity): %w", ErrInvalidConfig)
	}
	return nil
}

// withDefaults returns a copy of c with Scope and CredentialMode
// set to their Phase 1 defaults when the caller left them empty.
// Internal helper used by New; not exported because callers should
// see Validate as the only normalization step at the surface.
func (c Config) withDefaults() Config {
	if c.Scope == "" {
		c.Scope = "PRINCIPAL_ROLE:ALL"
	}
	if c.CredentialMode == "" {
		c.CredentialMode = "passthrough"
	}
	return c
}
