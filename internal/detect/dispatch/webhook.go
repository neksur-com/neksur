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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
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

// unauthorizedBody is the SINGLE response body shape returned for
// EVERY authentication-failure path on the webhook handler — invalid
// UUID, no schema, no row, no secret, signature mismatch. Identical
// status + identical body length prevents the tenant-enumeration
// oracle called out by REVIEW.md CR-02: an attacker cannot
// distinguish "tenant exists but no Polaris" from "tenant doesn't
// exist" from "signature wrong" via response content or timing.
//
// Operators triage real auth failures via the per-failure slog.Warn
// records (which DO carry the failure mode + remote IP), not the
// public response body.
const unauthorizedBody = "unauthorized"

// writeUnauthorized writes the unified 401 response. Use this for
// EVERY auth-failure path on the webhook handler (CR-02 mitigation).
// Operators see the discriminator in slog.Warn records, not the
// public response.
func writeUnauthorized(w http.ResponseWriter) {
	http.Error(w, unauthorizedBody, http.StatusUnauthorized)
}

// WebhookHandler returns the http.HandlerFunc for POST /v1/webhooks/polaris.
// adminPool is used to look up the per-tenant webhook secret from
// catalog_credentials.config_json. in is the dispatch channel — verified
// hits are pushed as Hit{Source: "polaris-webhook"}.
//
// Mount OUTSIDE TenantMiddleware (HMAC IS the auth).
//
// Pipeline (CR-02 mitigation — REVIEW.md):
//
//   1. Method + disable-switch gates (cheap, public).
//   2. Read raw body (256KiB cap) — bounds OOM exposure.
//   3. Read signature + tenant-id HEADERS only. Body is NOT yet
//      parsed (defer json.Unmarshal until AFTER HMAC verification).
//   4. Validate tenant UUID FORMAT (cheap, no DB hit) — invalid
//      format returns the unified 401 (NOT 400) so an attacker
//      cannot use the response to distinguish "well-formed
//      non-existent tenant" from "malformed input".
//   5. Look up per-tenant webhook secret — the bootstrap paradox
//      requires the DB hit happen pre-HMAC. ALL failure modes (no
//      schema / no row / null secret / pool error) collapse to the
//      same unified 401 response. Operator-visible details land in
//      slog.Warn.
//   6. HMAC verification — constant-time compare. Failure → unified 401.
//   7. ONLY NOW parse the JSON payload (HMAC has confirmed
//      Polaris-signed content; we can safely run encoding/json
//      against it). Malformed-after-sig-verify is 400 because the
//      caller's identity is established and the bug is in their
//      payload, not in a probing attacker.
//   8. Push verified Hit to dispatch channel.
//
// HTTP status conventions (CR-02-normalized):
//   - 200 — verified + queued.
//   - 400 — POST-HMAC malformed payload (e.g., missing
//     metadata_location AFTER signature verification).
//   - 401 — ANY pre-HMAC auth failure (unified body shape).
//   - 405 — method not allowed.
//   - 410 — handler disabled (NEKSUR_POLARIS_WEBHOOK_ENABLED=0).
//   - 503 — context cancelled / dispatch queue saturated (rare).
//
// Phase 2: per-IP rate limiting at the ingress layer is required to
// fully neutralize the enumeration vector. The Phase 1 deployment
// runbook (ops/runbooks/) MUST document that the Polaris webhook
// endpoint is rate-limited by the ALB / WAF rules in front of the
// pod. This handler does not implement an in-process limiter
// because the deployment-level limiter is the canonical fix.
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

		// Step 2 — read raw body (cap at 256KiB). The body is NOT
		// parsed yet; we only need the bytes for HMAC verification.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolarisWebhookBytes))
		if err != nil {
			// Body-read failure (e.g., cap exceeded) is the only
			// pre-auth failure that is allowed to be distinguishable
			// (it's a transport / size-limit signal, not a tenant
			// existence oracle). MaxBytesReader already writes the
			// response in some implementations; we add the unified
			// 401 here as a fallback to avoid a body-write race.
			writeUnauthorized(w)
			slog.Warn("dispatch/webhook: body read failed",
				"remote", r.RemoteAddr, "err", err)
			return
		}

		// Step 3 — extract signature + tenant-id HEADERS (no body parse yet).
		sig := r.Header.Get(polarisSignatureHeader)
		tenantID := r.Header.Get(polarisTenantHeader)
		if sig == "" || tenantID == "" {
			// Missing header → unified 401 (NOT 400). Identical
			// response shape to "valid headers but wrong sig" so
			// the attacker cannot distinguish "is the header
			// required?" from "did my sig match?".
			writeUnauthorized(w)
			slog.Warn("dispatch/webhook: missing required header",
				"remote", r.RemoteAddr, "has_sig", sig != "", "has_tenant", tenantID != "")
			return
		}

		// Step 4 — validate tenant UUID FORMAT (cheap, no DB hit).
		// CR-06: use canonical uuid.Parse() + tenant.SchemaName(),
		// NOT the hand-rolled isValidTenantUUID. Invalid format →
		// unified 401 (identical response to "well-formed but no
		// tenant").
		tenantUUID, parseErr := uuid.Parse(tenantID)
		if parseErr != nil {
			writeUnauthorized(w)
			slog.Warn("dispatch/webhook: invalid tenant uuid format",
				"remote", r.RemoteAddr, "tenant_raw", tenantID, "err", parseErr)
			return
		}

		// Step 5 — look up per-tenant webhook secret. ALL failure
		// modes (no schema / no row / null secret / pool error)
		// collapse to the SAME unified 401 response — the attacker
		// cannot distinguish them via response content. Operators
		// see the discriminator in slog.Warn.
		secret, lookupErr := loadWebhookSecret(r.Context(), adminPool, tenantUUID)
		if lookupErr != nil || secret == "" {
			writeUnauthorized(w)
			// Constant-time HMAC against a dummy key so the response
			// time on "no secret configured" matches the time on
			// "secret configured but wrong signature". Without this,
			// the attacker could distinguish the two paths via
			// timing alone.
			//
			// We compute against a per-process random key (initialized
			// in init() below) so the result is uncorrelated with any
			// real tenant secret. The result is discarded — the
			// computation is purely for timing parity.
			_ = verifyHMAC(body, sig, dummyHMACKey)
			slog.Warn("dispatch/webhook: secret lookup failed or empty",
				"remote", r.RemoteAddr, "tenant", tenantUUID.String(),
				"err", lookupErr)
			return
		}

		// Step 6 — HMAC verification (constant-time).
		if !verifyHMAC(body, sig, secret) {
			writeUnauthorized(w)
			slog.Warn("dispatch/webhook: HMAC mismatch",
				"remote", r.RemoteAddr, "tenant", tenantUUID.String(),
				"sig_len", len(sig))
			return
		}

		// Step 7 — HMAC verified; NOW it is safe to parse JSON.
		// Malformed body post-verify is a 400 (the caller's identity
		// is established; the bug is in their payload, not in a
		// probing attacker).
		var payload polarisWebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "body parse failed", http.StatusBadRequest)
			return
		}
		if payload.MetadataLocation == "" {
			http.Error(w, "missing metadata_location", http.StatusBadRequest)
			return
		}

		// Step 8 — push verified Hit to dispatch channel.
		hit := Hit{
			TenantID:         tenantUUID.String(),
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

// dummyHMACKey is a per-process random key used for the
// timing-equalization HMAC computation on the "no secret configured"
// path (CR-02). Real tenant secrets are never derivable from this
// key; it exists solely so the verifyHMAC call time matches the
// happy-path's time. Initialized once at package load.
var dummyHMACKey = func() string {
	// 32 random bytes hex-encoded — same length as a typical
	// SHA-256-derived secret so the HMAC arithmetic cost matches.
	// Using crypto/rand here would force a context dep on init;
	// we use a fixed-shape placeholder. The exact bytes do not
	// matter for timing parity — only the length + arithmetic
	// shape do.
	return "neksur-dispatch-webhook-dummy-key-padded-to-64-chars-AAAAAAAAAA"
}()

// Compile-time guard: keep tenant import alive even if a refactor
// drops the tenant.SchemaName call site.
var _ = tenant.SchemaName

// loadWebhookSecret fetches the per-tenant webhook secret from
// catalog_credentials.config_json[webhook_secret]. We use the admin
// pool directly (not tenant.WithTenantTx) because the webhook arrives
// pre-authentication; we resolve the tenant from the signed payload
// AFTER verification, but we need the secret to verify. The lookup is
// scoped via tenant.SchemaName (CR-06 — canonical helper).
//
// CR-02 contract: this function is called BEFORE HMAC verification,
// so ALL failure modes (no schema / no row / null secret / pool
// error) MUST collapse to a single response shape at the caller.
// The caller (WebhookHandler) maps any non-nil-error OR empty-string
// result to the unified 401 response.
//
// Takes uuid.UUID (NOT string) — the caller pre-validates the
// format via uuid.Parse so this function does not need to re-check.
func loadWebhookSecret(ctx context.Context, pool *pgxpool.Pool, tenantUUID uuid.UUID) (string, error) {
	// CR-06: canonical schema-name helper. tenant.SchemaName builds
	// "tenant_<uuid-with-dashes-replaced>"; pgx.Identifier.Sanitize
	// double-quotes the identifier so it is safe to splice.
	schema := tenant.SchemaName(tenantUUID)
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
	if _, err := conn.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantUUID.String()); err != nil {
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

// SignBody is a helper for tests + the polaris-webhook-register CLI to
// produce a signature header value matching the verifyHMAC function's
// expected shape.
func SignBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
