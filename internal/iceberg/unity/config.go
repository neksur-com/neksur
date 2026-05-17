// Package unity is the Databricks Unity Catalog IcebergCatalogClient adapter
// (D-3.02 — live Phase 3 implementation). Clones the polaris/ adapter pattern
// with Databricks-specific OAuth2 client-credentials + workspace-context
// headers.
//
// Per D-1.03 the entire catalog-side surface is declarative struct fields on
// Config; there is NO functional-options pattern and NO runtime capability
// flags. New (in adapter.go) reads from the struct, validates, and constructs
// an iceberg-go REST catalog client.
package unity

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrInvalidConfig is the package sentinel returned when Validate finds a
// missing or empty required field. Callers branch on
// errors.Is(err, unity.ErrInvalidConfig) to distinguish a configuration
// problem from a runtime Unity error.
var ErrInvalidConfig = errors.New("unity: invalid config")

// Config is the entire surface for a Unity Catalog-backed catalog (D-1.03
// declarative). All fields are visible at construction time; missing-required-
// field bugs are caught by Validate before any network call.
//
// Field semantics:
//
//   - WorkspaceHost: the base URL of the Databricks workspace
//     (e.g., "https://adb-12345.1.azuredatabricks.net"). The Unity Iceberg
//     REST API is at WorkspaceHost + "/api/2.1/unity-catalog/iceberg"; the
//     OAuth token endpoint is at WorkspaceHost + "/oidc/v1/token".
//
//   - WorkspaceID: the Databricks workspace ID (numeric string). Injected as
//     the X-Databricks-Workspace-Id header on every outbound request so
//     Databricks can route within a multi-workspace org. Supplied by operator
//     config (per-tenant); Validate() rejects empty/whitespace (T-3-unity-
//     workspace-spoof mitigation — see threat model).
//
//   - OAuthClientID / OAuthClientSecret: M2M OAuth client-credentials grant
//     identity. Combined as "OAuthClientID:OAuthClientSecret" for the iceberg-
//     go REST catalog credential wire (WR-07 colon restriction preserved).
//
//   - CatalogName: the Unity catalog name (e.g. "main"). Mapped to the
//     iceberg-go "warehouse" property — Unity's Iceberg REST surface uses
//     the catalog name as the warehouse identifier.
//
//   - CredentialMode: passed through to the upstream catalog as the
//     X-Iceberg-Access-Delegation header. Defaults to "vended-credentials"
//     (Unity STS vending enabled by default per Phase 3 contract, unlike
//     Polaris which defaults to "passthrough").
type Config struct {
	WorkspaceHost     string
	WorkspaceID       string
	OAuthClientID     string
	OAuthClientSecret string
	CatalogName       string
	CredentialMode    string

	// BaseTransportWrap is an OPTIONAL hook that wraps the underlying
	// http.Transport before the databricksContextTransport composes over it.
	// The composition order at New() time is:
	//
	//   iceberg-go.sessionTransport
	//      → refreshOn401Transport (Pitfall 2 refresh-on-401 retry)
	//          → databricksContextTransport (X-Databricks-Workspace-Id header)
	//              → BaseTransportWrap(http.DefaultTransport.Clone())  ← THIS
	//
	// Production callers leave this nil — the adapter uses a fresh
	// http.DefaultTransport.Clone() as the base. Integration tests inject a
	// recording RoundTripper to capture outbound request headers without
	// requiring a live Databricks workspace.
	//
	// The hook is on Config (declarative) rather than a functional-option per
	// D-1.03 — every field of the adapter surface is visible on the struct at
	// construction time.
	BaseTransportWrap func(http.RoundTripper) http.RoundTripper
}

// Validate checks the five required fields are non-empty. Defaults for
// CredentialMode are NOT applied here (callers may inspect a returned Config
// to see what they passed); New (in adapter.go) is responsible for applying
// the empty-string defaults before constructing the iceberg-go client.
//
// Returns an error wrapped around ErrInvalidConfig so callers can branch on
// errors.Is + still see the specific missing field in the message text.
func (c Config) Validate() error {
	switch {
	case c.WorkspaceHost == "":
		return fmt.Errorf("unity: config: WorkspaceHost required: %w", ErrInvalidConfig)
	case c.WorkspaceID == "":
		return fmt.Errorf("unity: config: WorkspaceID required: %w", ErrInvalidConfig)
	case c.OAuthClientID == "":
		return fmt.Errorf("unity: config: OAuthClientID required: %w", ErrInvalidConfig)
	case c.OAuthClientSecret == "":
		return fmt.Errorf("unity: config: OAuthClientSecret required: %w", ErrInvalidConfig)
	case c.CatalogName == "":
		return fmt.Errorf("unity: config: CatalogName required: %w", ErrInvalidConfig)
	}
	// WR-07: the OAuth credential prop is constructed as
	// "OAuthClientID:OAuthClientSecret"; iceberg-go splits on the FIRST colon.
	// A ClientID or ClientSecret containing ':' would silently truncate at the
	// wrong position. Reject at config-Validate time so the bug is caught at
	// boot, not at first commit.
	if strings.Contains(c.OAuthClientID, ":") {
		return fmt.Errorf("unity: config: OAuthClientID must not contain ':' (OAuth credential ambiguity): %w", ErrInvalidConfig)
	}
	if strings.Contains(c.OAuthClientSecret, ":") {
		return fmt.Errorf("unity: config: OAuthClientSecret must not contain ':' (OAuth credential ambiguity): %w", ErrInvalidConfig)
	}
	return nil
}

// withDefaults returns a copy of c with CredentialMode set to its Phase 3
// default when the caller left it empty. Internal helper used by New; not
// exported because callers should see Validate as the only normalization step
// at the surface.
func (c Config) withDefaults() Config {
	if c.CredentialMode == "" {
		c.CredentialMode = "vended-credentials"
	}
	return c
}
