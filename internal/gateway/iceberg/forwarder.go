// BuildAdapter — per-request adapter dispatch from V0060 catalog_credentials.
//
// Given a Credentials row (Plan 01-06 Task 1), construct the matching
// IcebergCatalogClient adapter (Plan 01-02 + 01-03). Each catalog kind
// has its own typed Config struct (D-1.03) that we unmarshal from
// `config_json`, then `cfg.Endpoint` is overridden with the row's
// stored Endpoint (the JSON may carry stale endpoint metadata; the row
// is the source of truth).
//
// Error translation:
//   - polaris/nessie/glue/unity recognised → adapter or wrapped err.
//   - any other kind → wrapped catalog.ErrCatalogKindUnsupported (the
//     V0060 CHECK constraint should make this unreachable in production;
//     defence-in-depth + future-proofing for new kinds).
//   - JSON unmarshal failure → wrapped catalog.ErrConfigUnmarshal (the
//     gateway maps to 500; SecOps alerts on the audit log).
//
// The function is stateless — each call re-builds an adapter. The
// adapters themselves cache their iceberg-go REST catalog clients
// internally (the adapter struct holds a *rest.Catalog) so the
// per-request cost is the JSON unmarshal + adapter constructor only.
// Phase 2 may add an LRU-cached `(tenantID, nickname) → adapter` map at
// the gateway boundary if profiling shows the construction cost matters.
//
// B-3 resolution (Plans 03-03 + 03-04): both the "unity" arm (live, Plan 03-03)
// and the "glue" arm (live, Plan 03-04 — flipped from glue_stub to glue.New)
// are in this single dispatch table. Plan 03-04 created internal/iceberg/glue/
// and replaced the glue_stub import with the live glue import. This eliminates
// the Wave 1 intra-wave overlap on forwarder.go between Plans 03-03 and 03-04.
//
// Phase 02 CR-03 boot guard (assertNoUnsupportedCatalogs) removed in Plan
// 03-03 main.go edit — tenants with kind=unity now route to the live adapter;
// kind=glue routes to glue_stub until Plan 03-04 ships.

package iceberg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/neksur-com/neksur/internal/catalog"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg/glue"
	"github.com/neksur-com/neksur/internal/iceberg/nessie"
	"github.com/neksur-com/neksur/internal/iceberg/polaris"
	"github.com/neksur-com/neksur/internal/iceberg/unity"
)

// polarisCfg is the JSON-tagged shape stored in V0060.config_json for
// kind=polaris. Phase 1 uses lowerCamelCase keys for forward-compat with
// the admin CLI's catalog-onboarding flow (Plan 01-09). Field semantics
// match polaris.Config 1:1; we have a separate struct here only so the
// JSON tags don't pollute polaris.Config (whose fields are referenced
// from production code that does not need JSON marshaling).
type polarisCfg struct {
	Warehouse      string `json:"warehouse"`
	ClientID       string `json:"clientId"`
	ClientSecret   string `json:"clientSecret"`
	Scope          string `json:"scope,omitempty"`
	CredentialMode string `json:"credentialMode,omitempty"`
}

// nessieCfg is the JSON-tagged shape for kind=nessie. Same rationale as
// polarisCfg — JSON tags isolated from nessie.Config.
type nessieCfg struct {
	DefaultBranch string `json:"defaultBranch,omitempty"`
	AuthMode      string `json:"authMode,omitempty"`
	BearerToken   string `json:"bearerToken,omitempty"`
}

// glueCfg matches glue.Config (Plan 03-04 live adapter). Region and
// CatalogID are required by glue.Config.Validate(). IAMRoleARN is
// optional metadata (credentials come from the AWS default chain).
type glueCfg struct {
	Region     string `json:"region,omitempty"`
	CatalogID  string `json:"catalogId,omitempty"`
	IAMRoleARN string `json:"iamRoleArn,omitempty"`
}

// unityCfg is the JSON-tagged shape stored in V0060.config_json for
// kind=unity. Field names match the Phase 3 live unity.Config (D-1.03). The
// AccessToken field is retained for backward compat with any Phase 1/2 tenant
// rows that used workspaceUrl+accessToken; the live unity.New now requires
// OAuth M2M credentials (oauthClientId + oauthClientSecret) and workspace
// context (workspaceHost + workspaceId).
//
// Plan 03-03 D-3.02: live adapter wired; stub deleted.
type unityCfg struct {
	WorkspaceHost     string `json:"workspaceHost,omitempty"`
	WorkspaceID       string `json:"workspaceId,omitempty"`
	OAuthClientID     string `json:"oauthClientId,omitempty"`
	OAuthClientSecret string `json:"oauthClientSecret,omitempty"`
	CatalogName       string `json:"catalogName,omitempty"`
	CredentialMode    string `json:"credentialMode,omitempty"`
}

// BuildAdapter dispatches on creds.Kind to construct the matching
// IcebergCatalogClient. The returned interface lets the gateway call
// LoadTable / CommitTable without per-kind branching downstream.
//
// Returns:
//   - (adapter, nil) on success.
//   - (nil, wrapped catalog.ErrConfigUnmarshal) — config_json bytes
//     don't fit the expected per-kind shape.
//   - (nil, wrapped catalog.ErrCatalogKindUnsupported) — kind is not
//     one of polaris/nessie/glue/unity (V0060 CHECK should prevent).
//   - (nil, wrapped per-adapter err) — Validate failed, network
//     failure during adapter construction (rare; the adapters' New
//     functions probe the upstream `/v1/config` at construction time).
func BuildAdapter(ctx context.Context, creds *catalog.Credentials) (iceberg.IcebergCatalogClient, error) {
	if creds == nil {
		return nil, fmt.Errorf("gateway: BuildAdapter: nil credentials")
	}
	switch creds.Kind {
	case "polaris":
		var pc polarisCfg
		if err := json.Unmarshal(creds.ConfigJSON, &pc); err != nil {
			return nil, fmt.Errorf("gateway: %w: polaris: %v", catalog.ErrConfigUnmarshal, err)
		}
		cfg := polaris.Config{
			Endpoint:       creds.Endpoint, // row endpoint is source of truth
			Warehouse:      pc.Warehouse,
			ClientID:       pc.ClientID,
			ClientSecret:   pc.ClientSecret,
			Scope:          pc.Scope,
			CredentialMode: pc.CredentialMode,
		}
		return polaris.New(ctx, cfg)

	case "nessie":
		var nc nessieCfg
		if err := json.Unmarshal(creds.ConfigJSON, &nc); err != nil {
			return nil, fmt.Errorf("gateway: %w: nessie: %v", catalog.ErrConfigUnmarshal, err)
		}
		cfg := nessie.Config{
			Endpoint:      creds.Endpoint,
			DefaultBranch: nc.DefaultBranch,
			AuthMode:      nc.AuthMode,
			BearerToken:   nc.BearerToken,
		}
		return nessie.New(ctx, cfg)

	case "glue":
		// Plan 03-04: live Glue Iceberg REST adapter. glue_stub/ deleted.
		// B-3 contract satisfied: this arm was pre-allocated by Plan 03-03;
		// Plan 03-04 creates internal/iceberg/glue/ and switches the import.
		var gc glueCfg
		if err := json.Unmarshal(creds.ConfigJSON, &gc); err != nil {
			return nil, fmt.Errorf("gateway: %w: glue: %v", catalog.ErrConfigUnmarshal, err)
		}
		cfg := glue.Config{
			Region:     gc.Region,
			CatalogID:  gc.CatalogID,
			IAMRoleARN: gc.IAMRoleARN,
		}
		return glue.New(ctx, cfg)

	case "unity":
		// Plan 03-03 D-3.02: live Unity adapter wired; unity_stub deleted.
		// B-3 absorption: this arm is in the same edit as the glue arm below.
		var uc unityCfg
		if err := json.Unmarshal(creds.ConfigJSON, &uc); err != nil {
			return nil, fmt.Errorf("gateway: %w: unity: %v", catalog.ErrConfigUnmarshal, err)
		}
		cfg := unity.Config{
			WorkspaceHost:     uc.WorkspaceHost,
			WorkspaceID:       uc.WorkspaceID,
			OAuthClientID:     uc.OAuthClientID,
			OAuthClientSecret: uc.OAuthClientSecret,
			CatalogName:       uc.CatalogName,
			CredentialMode:    uc.CredentialMode,
		}
		return unity.New(ctx, cfg)

	default:
		return nil, fmt.Errorf("gateway: %w: %s", catalog.ErrCatalogKindUnsupported, creds.Kind)
	}
}
