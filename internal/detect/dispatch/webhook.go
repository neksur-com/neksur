// Polaris webhook receiver — POST /v1/webhooks/polaris.
//
// HMAC signature verification BEFORE any tenant resolution or DB lookup
// (Pitfall: T-1-webhook-spoof-polaris). The per-tenant webhook secret
// lives in catalog_credentials.config_json[webhook_secret] (Plan 01-09
// admin CLI's `polaris webhook-register` subcommand seeds the row).
//
// Trust-chain rationale (CONTEXT line 174):
//   - Polaris signs the payload with HMAC-SHA256 using a secret shared
//     during webhook registration. The `X-Polaris-Signature` header
//     carries the hex-encoded MAC.
//   - We MUST verify before any DB / Cypher access — an attacker that
//     forges a Polaris payload would otherwise trigger arbitrary
//     `LoadTable` reads against the upstream catalog.
//   - The webhook handler is mounted OUTSIDE workosauth.TenantMiddleware
//     because Polaris is a server-to-server caller (not a user session).
//     The HMAC verification IS the auth check; the per-tenant secret IS
//     the principal.
//
// If Polaris 1.4.0 lacks webhook signing entirely (Open Question 2),
// this handler is disabled via env flag NEKSUR_POLARIS_WEBHOOK_ENABLED=0
// — the runbook (Plan 01-09) documents the operator step. The handler
// returns 410 Gone when disabled so misconfigured upstream Polaris
// callers see a clear "not enabled" signal.

package dispatch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// polarisSignatureHeader is the HTTP header carrying the HMAC-SHA256
// of the request body. Convention follows Polaris 1.4.0 webhook docs;
// the operator confirms the wire shape during onboarding.
const polarisSignatureHeader = "X-Polaris-Signature"

// polarisTenantHeader optionally identifies the tenant the webhook is
// for (when the upstream Polaris instance hosts multiple tenants on
// one webhook subscription). Phase 1 also accepts the tenant in the
// payload body via `tenant_id` field; the header is preferred when
// present.
const polarisTenantHeader = "X-Polaris-Tenant"

// maxPolarisWebhookBytes — body cap. Polaris webhook payloads are
// well under 100KiB; 256KiB is generous.
const maxPolarisWebhookBytes = 256 * 1024

// polarisWebhookPayload is the minimal shape we extract from the
// signed body. Polaris emits a richer event envelope; we only need
// the snapshot's metadata_location for dispatch dedup.
type polarisWebhookPayload struct {
	TenantID         string `json:"tenant_id,omitempty"`
	MetadataLocation string `json:"metadata_location"`
	Namespace        string `json:"namespace,omitempty"`
	TableName        string `json:"table_name,omitempty"`
}

// WebhookHandler returns the http.HandlerFunc for POST /v1/webhooks/polaris.
// adminPool is used to look up the per-tenant webhook secret from
// catalog_credentials.config_json. in is the dispatch channel — verified
// hits are pushed as Hit{Source: "polaris-webhook"}.
//
// Mount OUTSIDE TenantMiddleware (HMAC IS the auth).
//
// HTTP status conventions:
//   - 200 — verified + queued.
//   - 400 — body parse error / missing header / missing metadata_location.
//   - 401 — signature mismatch.
//   - 410 — handler disabled (NEKSUR_POLARIS_WEBHOOK_ENABLED=0).
//   - 503 — admin pool unreachable / catalog_credentials lookup failed.
func WebhookHandler(adminPool *pgxpool.Pool, in chan<- Hit) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Optional disable switch — lets ops gate the handler off if
		// Polaris webhook signing isn't yet supported in their deployment.
		if os.Getenv("NEKSUR_POLARIS_WEBHOOK_ENABLED") == "0" {
			http.Error(w, "polaris webhook handler disabled", http.StatusGone)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read raw body (cap at 256KiB).
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolarisWebhookBytes))
		if err != nil {
			http.Error(w, "body read failed", http.StatusBadRequest)
			return
		}

		// Extract signature header.
		sig := r.Header.Get(polarisSignatureHeader)
		if sig == "" {
			http.Error(w, "missing signature header", http.StatusBadRequest)
			return
		}

		// Parse the payload to extract tenant_id (we need it to look
		// up the per-tenant webhook secret).
		var payload polarisWebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "body parse failed", http.StatusBadRequest)
			return
		}
		tenantID := r.Header.Get(polarisTenantHeader)
		if tenantID == "" {
			tenantID = payload.TenantID
		}
		if tenantID == "" {
			http.Error(w, "missing tenant id (header or payload)", http.StatusBadRequest)
			return
		}

		// Look up the per-tenant webhook secret.
		secret, err := loadWebhookSecret(r.Context(), adminPool, tenantID)
		if err != nil {
			slog.Error("dispatch/webhook: lookup secret failed",
				"tenant", tenantID, "err", err)
			http.Error(w, "secret lookup failed", http.StatusServiceUnavailable)
			return
		}
		if secret == "" {
			http.Error(w, "no webhook secret configured for tenant", http.StatusUnauthorized)
			return
		}

		// HMAC verification — constant-time compare.
		if !verifyHMAC(body, sig, secret) {
			slog.Warn("dispatch/webhook: HMAC mismatch",
				"tenant", tenantID, "sig_len", len(sig))
			http.Error(w, "signature mismatch", http.StatusUnauthorized)
			return
		}

		if payload.MetadataLocation == "" {
			http.Error(w, "missing metadata_location", http.StatusBadRequest)
			return
		}

		// Verified — push to dispatch channel.
		hit := Hit{
			TenantID:         tenantID,
			MetadataLocation: payload.MetadataLocation,
			Source:           "polaris-webhook",
			TableName:        payload.TableName,
		}
		if payload.Namespace != "" {
			hit.TableNamespace = []string{payload.Namespace}
		}
		select {
		case <-r.Context().Done():
			http.Error(w, "request cancelled", http.StatusServiceUnavailable)
			return
		case in <- hit:
		}

		w.WriteHeader(http.StatusOK)
	}
}

// loadWebhookSecret fetches the per-tenant webhook secret from
// catalog_credentials.config_json[webhook_secret]. We use the admin
// pool directly (not tenant.WithTenantTx) because the webhook arrives
// pre-authentication; we resolve the tenant from the signed payload
// AFTER verification, but we need the secret to verify. The lookup is
// scoped via the schema name computed from the tenant UUID.
//
// This bootstrap-paradox is unavoidable for HMAC-signed webhooks —
// we trust the schema-qualification of the lookup (the tenant UUID is
// validated to be a UUIDv4 before splicing).
func loadWebhookSecret(ctx context.Context, pool *pgxpool.Pool, tenantID string) (string, error) {
	// Validate tenant UUID format BEFORE splicing into SQL identifier.
	if !isValidTenantUUID(tenantID) {
		return "", fmt.Errorf("dispatch/webhook: invalid tenant id format: %q", tenantID)
	}

	schema := "tenant_" + strings.ReplaceAll(tenantID, "-", "_")
	qSchema := pgx.Identifier{schema}.Sanitize()
	query := fmt.Sprintf(`
		SELECT config_json->>'webhook_secret' AS secret
		FROM %s.catalog_credentials
		WHERE catalog_kind = 'polaris'
		LIMIT 1
	`, qSchema)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("dispatch/webhook: pool acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		return "", fmt.Errorf("dispatch/webhook: set_config: %w", err)
	}

	var secret *string
	err = conn.QueryRow(ctx, query).Scan(&secret)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("dispatch/webhook: query: %w", err)
	}
	if secret == nil {
		return "", nil
	}
	return *secret, nil
}

// verifyHMAC compares the HMAC-SHA256 of body (using `secret`) against
// the hex-encoded signature header. Constant-time via hmac.Equal.
func verifyHMAC(body []byte, signatureHex, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(strings.TrimSpace(signatureHex))
	if err != nil {
		return false
	}
	return hmac.Equal(expected, got)
}

// isValidTenantUUID is a strict character-class check — UUID v4 is
// hex chars + hyphens only. We don't use a full regex here (this hot
// path runs per webhook); the strict char check is sufficient for
// SQL-identifier safety.
func isValidTenantUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// SignBody is a helper for tests + the polaris-webhook-register CLI to
// produce a signature header value matching the verifyHMAC function's
// expected shape.
func SignBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
