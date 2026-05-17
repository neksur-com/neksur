package testfixture

// unity.go — Unity Catalog live-creds skip-when-absent fixture.
//
// Unity Catalog does not offer a public testcontainer image (no
// "localstack for Unity" exists as of Phase 3). The fixture follows the
// D-2.09 PENDING_FIRST_RUN pattern from Phase 02: read live credentials
// from environment variables and call t.Skipf if any are absent.
//
// Required environment variables:
//   - NEKSUR_UNITY_WORKSPACE_HOST  — Databricks workspace URL (e.g.
//     "https://adb-12345.1.azuredatabricks.net")
//   - NEKSUR_UNITY_OAUTH_CLIENT_ID  — OAuth M2M client ID
//   - NEKSUR_UNITY_OAUTH_CLIENT_SECRET — OAuth M2M client secret
//   - NEKSUR_UNITY_CATALOG_NAME    — Unity catalog name (e.g. "main")
//
// When all four are present, StartUnity returns a *UnityClient that
// downstream tests use to configure Unity's Iceberg external catalog
// integration (Phase 3 D-3.02 adapter path). When any is absent, the
// test is skipped (t.Skipf) so CI without Unity credentials is not
// blocked.
//
// Nightly CI (D-3.01) — Unity live-account tests run in the nightly
// CI workflow only (`.github/workflows/nightly-unity.yml`); they are
// not part of the normal integration test suite run on every PR.
//
// Threat T-3-04-credential-leak (PLAN threat model — mitigate):
// Fixtures call t.Skipf on absence; tests never log env-var values;
// CI secret-scanning gates per Phase 0.5 invariant. Do NOT add any
// log.Printf or t.Logf that includes the credential values.

import (
	"os"
	"testing"
)

// UnityClient carries the live Unity Catalog coordinates for use in
// integration tests that configure the Unity Iceberg REST adapter.
// This is a thin data struct — no live connections are established at
// construction time.
type UnityClient struct {
	WorkspaceHost string
	CatalogName   string
	// ClientID and ClientSecret are intentionally not exported as public
	// fields — callers obtain them via OAuthCredentials() which returns
	// a [2]string pair. This prevents accidental logging of the struct.
	clientID     string
	clientSecret string
}

// StartUnity reads Unity Catalog credentials from environment variables
// and returns a *UnityClient for use in integration tests. If any
// required variable is absent, the test is skipped via t.Skipf.
//
// This function must be called from a *testing.T context (integration
// tests only). It is safe to call concurrently from multiple goroutines.
func StartUnity(t *testing.T) *UnityClient {
	t.Helper()

	host := os.Getenv("NEKSUR_UNITY_WORKSPACE_HOST")
	clientID := os.Getenv("NEKSUR_UNITY_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("NEKSUR_UNITY_OAUTH_CLIENT_SECRET")
	catalogName := os.Getenv("NEKSUR_UNITY_CATALOG_NAME")

	if host == "" || clientID == "" || clientSecret == "" || catalogName == "" {
		t.Skipf("unity credentials not set — skipping per D-2.09 PENDING_FIRST_RUN "+
			"(set NEKSUR_UNITY_WORKSPACE_HOST, NEKSUR_UNITY_OAUTH_CLIENT_ID, "+
			"NEKSUR_UNITY_OAUTH_CLIENT_SECRET, NEKSUR_UNITY_CATALOG_NAME to enable)")
		return nil // unreachable; t.Skipf panics, but satisfies the compiler
	}

	return &UnityClient{
		WorkspaceHost: host,
		CatalogName:   catalogName,
		clientID:      clientID,
		clientSecret:  clientSecret,
	}
}

// OAuthCredentials returns the M2M OAuth client ID and secret for use in
// HTTP Authorization headers when talking to Unity Catalog's REST API.
// Returns ["", ""] when the client is nil (test was skipped).
func (u *UnityClient) OAuthCredentials() (clientID, clientSecret string) {
	if u == nil {
		return "", ""
	}
	return u.clientID, u.clientSecret
}

// IcebergRESTEndpoint returns the Unity Catalog Iceberg REST API base URL
// for the configured workspace and catalog. Unity exposes Iceberg REST at
// `{workspace_host}/api/2.1/unity-catalog/iceberg` per Databricks docs
// (April 2026 Iceberg REST API conformance — Mode 1 only; Horizon Mode 2
// deferred to Phase 5 per D-3.01).
func (u *UnityClient) IcebergRESTEndpoint() string {
	if u == nil {
		return ""
	}
	return u.WorkspaceHost + "/api/2.1/unity-catalog/iceberg"
}
