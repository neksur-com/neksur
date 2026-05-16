// handler.go — HTTP handler for POST /v1/credvend/sts.
//
// Mounted behind workosauth.TenantMiddleware (matching Plan 02-05 pattern).
// Fail-closed: any Service.Issue error → HTTP 503.
//
// Defense-in-depth:
//   - tenant.IDFromContext check (CC1 — redundant with TenantMiddleware but
//     mirrors the Phase 1 gateway.handler.go pattern for defence-in-depth).
//   - identifierRegex on catalog_nickname + table_namespace + table_name +
//     region body fields (T-2-credvend-handler-tenant-spoof mitigation).
//   - http.MaxBytesReader cap (8 KiB — small JSON body; T-2-credvend-large-body-DoS).
//   - Per-tenant adapter built from CredStore per request (same pattern as
//     gateway/handler.go adapterFor method).
//
// Pitfall 11 discipline (RESEARCH): handler logs ONLY metadata
// (table.Name, region, status_code) — NEVER the STS credentials.
package credvend

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/neksur-com/neksur/internal/catalog"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/observability"
	"github.com/neksur-com/neksur/internal/tenant"
)

// identifierRegex restricts catalog_nickname, table_namespace, table_name,
// and region body fields to safe identifier characters. Prevents injection
// via body fields (T-2-credvend-handler-tenant-spoof mitigation).
var identifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`)

// maxRequestBodyBytes caps the incoming JSON body at 8 KiB (small JSON
// request; T-2-credvend-large-body-DoS mitigation per the plan).
const maxRequestBodyBytes = 8 * 1024

// issueSTSRequest is the HTTP request body for POST /v1/credvend/sts.
type issueSTSRequest struct {
	// CatalogNickname identifies the tenant's catalog credential row
	// (V0060 catalog_credentials.nickname). Required for per-tenant
	// adapter construction (same lookup path as gateway/handler.go prefix).
	CatalogNickname string `json:"catalog_nickname"`
	TableNamespace  string `json:"table_namespace"`
	TableName       string `json:"table_name"`
	Region          string `json:"region"`
}

// issueSTSResponse is the HTTP response body for a successful STS issuance.
// Expiration is RFC 3339 so Spark / clients can parse it with standard
// ISO 8601 date-time parsers.
type issueSTSResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	Expiration      string `json:"expiration"` // RFC 3339
	Region          string `json:"region"`
}

// Deps groups the dependencies injected into the Handler function.
type Deps struct {
	// Service is the credvend.Service (cache + counters + fail-closed logic).
	Service *Service

	// CredStore is used to look up the tenant's catalog credentials per-request,
	// matching the gateway/handler.go per-tenant adapter construction pattern.
	CredStore *catalog.Repo

	// AdapterBuilder constructs an IcebergCatalogClient from catalog.Credentials.
	// In production this is iceberggw.BuildAdapter; in tests it can be stubbed.
	AdapterBuilder func(ctx context.Context, creds *catalog.Credentials) (iceberg.IcebergCatalogClient, error)
}

// Handler returns an http.Handler for POST /v1/credvend/sts. Must be
// mounted behind workosauth.TenantMiddleware.
func Handler(deps Deps) http.Handler {
	adapterBuilder := deps.AdapterBuilder
	if adapterBuilder == nil {
		adapterBuilder = iceberggw.BuildAdapter
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Defense-in-depth: assert tenant is in context. TenantMiddleware
		// already enforces this, but we check explicitly (Phase 1 CC1 pattern).
		tid, ok := tenant.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "tenant missing from context", http.StatusInternalServerError)
			return
		}

		// Body cap — DoS prevention (T-2-credvend-large-body-DoS).
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

		var req issueSTSRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		// Identifier validation — prevent injection (T-2-credvend-handler-tenant-spoof).
		if !identifierRegex.MatchString(req.CatalogNickname) {
			http.Error(w, "invalid catalog_nickname", http.StatusBadRequest)
			return
		}
		if !identifierRegex.MatchString(req.TableNamespace) {
			http.Error(w, "invalid table_namespace", http.StatusBadRequest)
			return
		}
		if !identifierRegex.MatchString(req.TableName) {
			http.Error(w, "invalid table_name", http.StatusBadRequest)
			return
		}
		if !identifierRegex.MatchString(req.Region) {
			http.Error(w, "invalid region", http.StatusBadRequest)
			return
		}

		// Look up the tenant's catalog credentials (same pattern as
		// gateway/handler.go's adapterFor method — nickname is the
		// catalog row identifier scoped to the tenant via RLS).
		creds, err := deps.CredStore.GetCatalogCredentials(r.Context(), req.CatalogNickname)
		if err != nil {
			if errors.Is(err, catalog.ErrCredentialsNotFound) {
				http.Error(w, "catalog credentials not found", http.StatusNotFound)
				return
			}
			slog.WarnContext(r.Context(), "credvend: cannot load catalog creds",
				"catalog_nickname", req.CatalogNickname,
				"tenant_id", tid,
			)
			observability.L4TokenFailuresTotal.WithLabelValues("unknown", "cred_store_error").Inc()
			http.Error(w, "credential vending unavailable", http.StatusServiceUnavailable)
			return
		}

		// Build the per-tenant adapter (polaris, nessie, or stub).
		adapter, err := adapterBuilder(r.Context(), creds)
		if err != nil {
			slog.WarnContext(r.Context(), "credvend: cannot build adapter",
				"catalog_nickname", req.CatalogNickname,
				"catalog_kind", creds.Kind,
			)
			observability.L4TokenFailuresTotal.WithLabelValues(creds.Kind, "adapter_build_error").Inc()
			http.Error(w, "credential vending unavailable", http.StatusServiceUnavailable)
			return
		}

		tableRef := iceberg.TableRef{
			Namespace: []string{req.TableNamespace},
			Name:      req.TableName,
		}

		issuedCreds, err := deps.Service.Issue(r.Context(), tid.String(), adapter, tableRef, req.Region)
		if err != nil {
			// Pitfall 11: log only metadata, NEVER the error detail that
			// might contain STS creds or policy content.
			slog.WarnContext(r.Context(), "credvend: issue failed",
				"table", req.TableName,
				"region", req.Region,
				"is_unavailable", errors.Is(err, ErrCredVendUnavailable),
				"is_not_supported", errors.Is(err, ErrEngineNotSupported),
			)
			// Fail-closed: any error → 503 (D-1.09 carryover).
			observability.L4TokenFailuresTotal.WithLabelValues(adapter.Capabilities().Name, "issue_failed").Inc()
			http.Error(w, "credential vending unavailable", http.StatusServiceUnavailable)
			return
		}

		resp := issueSTSResponse{
			AccessKeyID:     issuedCreds.AccessKeyID,
			SecretAccessKey: issuedCreds.SecretAccessKey,
			SessionToken:    issuedCreds.SessionToken,
			Expiration:      issuedCreds.Expiration.Format(time.RFC3339),
			Region:          issuedCreds.Region,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			// Response already started — log only.
			slog.ErrorContext(r.Context(), "credvend: encode response failed", "err", err)
		}
	})
}
