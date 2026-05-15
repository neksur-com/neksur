// neksur-cli polaris webhook-register — operator subcommand for Plan 01-07.
//
// Registers Neksur as a webhook subscriber against an upstream Polaris
// catalog instance. The flow:
//
//   1. Generate a fresh HMAC secret (32 bytes via crypto/rand → hex).
//   2. POST to the upstream Polaris admin API to register a webhook
//      subscription pointing at our gateway's /v1/webhooks/polaris URL.
//      Phase 1 OPEN QUESTION 2 — Polaris 1.4.0's webhook admin API is
//      under finalization; this subcommand encodes the most-likely
//      shape AND prints the secret to operator stdout (one-time
//      visible) so a human can manually configure if the admin API
//      isn't supported.
//   3. UPDATE catalog_credentials.config_json[webhook_secret] in the
//      tenant's schema so the gateway's /v1/webhooks/polaris handler
//      can verify signatures.
//
// Usage:
//
//   neksur-cli polaris webhook-register \
//     --tenant=<uuid> \
//     --polaris-endpoint=https://polaris.acme.com/api/catalog \
//     --neksur-webhook=https://neksur.acme.com/v1/webhooks/polaris
//
// Required env:
//
//   DATABASE_URL — admin pool DSN (for the catalog_credentials UPDATE)

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/tenant"
)

// runPolarisWebhookRegister implements `neksur-cli polaris webhook-register`.
// Returns an int exit code (0 = success).
func runPolarisWebhookRegister(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("polaris webhook-register", flag.ContinueOnError)
	tenantFlag := fs.String("tenant", "", "tenant UUID (required)")
	polarisEndpointFlag := fs.String("polaris-endpoint", "",
		"upstream Polaris catalog ROOT URL (e.g., https://polaris.acme.com/api/catalog)")
	neksurWebhookFlag := fs.String("neksur-webhook", "",
		"public URL of our gateway's /v1/webhooks/polaris endpoint")
	skipUpstreamFlag := fs.Bool("skip-upstream", false,
		"skip the upstream Polaris API call; only generate secret + UPDATE catalog_credentials (operator-manual path)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenantFlag == "" || *neksurWebhookFlag == "" {
		fmt.Fprintln(os.Stderr, "polaris webhook-register: --tenant and --neksur-webhook required")
		fs.Usage()
		return 2
	}
	if !*skipUpstreamFlag && *polarisEndpointFlag == "" {
		fmt.Fprintln(os.Stderr, "polaris webhook-register: --polaris-endpoint required (or pass --skip-upstream)")
		return 2
	}

	dsn, code := requireEnv("DATABASE_URL")
	if code != 0 {
		return code
	}

	// Generate a fresh HMAC secret — 32 bytes hex-encoded.
	secret, err := generateWebhookSecret()
	if err != nil {
		fmt.Fprintf(os.Stderr, "polaris webhook-register: secret gen failed: %v\n", err)
		return 1
	}

	// Step 1 — store the secret in catalog_credentials.config_json.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "polaris webhook-register: pgxpool.New: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := storeWebhookSecret(ctx, pool, *tenantFlag, secret); err != nil {
		fmt.Fprintf(os.Stderr, "polaris webhook-register: storeWebhookSecret: %v\n", err)
		return 1
	}

	// Step 2 — POST to the upstream Polaris admin API (unless --skip-upstream).
	if !*skipUpstreamFlag {
		if err := registerWithPolaris(ctx, *polarisEndpointFlag, *neksurWebhookFlag, secret); err != nil {
			fmt.Fprintf(os.Stderr,
				"polaris webhook-register: upstream registration failed (secret stored locally; configure upstream manually with the secret printed below): %v\n",
				err)
			// Don't return non-zero here — the local-side secret is in
			// place; the operator can configure the upstream Polaris
			// instance manually using the printed secret.
		}
	}

	// Step 3 — print the secret (one-time visible) so the operator can
	// configure the upstream side manually if needed.
	fmt.Printf("Webhook registration complete.\n")
	fmt.Printf("Tenant:           %s\n", *tenantFlag)
	fmt.Printf("Webhook URL:      %s\n", *neksurWebhookFlag)
	fmt.Printf("HMAC secret:      %s\n", secret)
	fmt.Printf("\nThe secret has been stored in catalog_credentials.config_json[webhook_secret] for this tenant.\n")
	fmt.Printf("If the upstream Polaris API was not auto-registered (--skip-upstream or admin API unsupported), configure it manually:\n")
	fmt.Printf("  - URL:    %s\n", *neksurWebhookFlag)
	fmt.Printf("  - Secret: %s\n", secret)
	return 0
}

// generateWebhookSecret returns a 64-character hex-encoded 32-byte
// random secret suitable for HMAC-SHA256.
func generateWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// storeWebhookSecret UPDATEs the polaris row in tenant_<uuid>.catalog_credentials,
// merging `webhook_secret` into the existing config_json.
//
// CR-06 (REVIEW.md): use canonical uuid.Parse() + tenant.SchemaName
// rather than the hand-rolled isValidTenantUUIDForCLI.
func storeWebhookSecret(ctx context.Context, pool *pgxpool.Pool, tenantID, secret string) error {
	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("invalid tenant UUID format %q: %w", tenantID, err)
	}
	schema := tenant.SchemaName(tenantUUID)
	qSchema := pgx.Identifier{schema}.Sanitize()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pool acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config: %w", err)
	}
	updateSQL := fmt.Sprintf(`
		UPDATE %s.catalog_credentials
		SET config_json = config_json || jsonb_build_object('webhook_secret', $1::text)
		WHERE catalog_kind = 'polaris'
	`, qSchema)
	res, err := conn.Exec(ctx, updateSQL, secret)
	if err != nil {
		return fmt.Errorf("update catalog_credentials: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("no polaris catalog_credentials row found for tenant %s; seed a row first via `neksur-cli catalog onboard`", tenantID)
	}
	return nil
}

// registerWithPolaris POSTs to the Polaris admin API to register a
// webhook subscription. Phase 1 best-effort: Polaris 1.4.0's exact
// admin API surface is under finalization (Open Question 2). This
// function uses the most-likely shape; if Polaris returns 4xx the
// caller falls back to the manual-configuration path printed to stdout.
func registerWithPolaris(ctx context.Context, polarisEndpoint, neksurWebhookURL, secret string) error {
	// POST to <polaris_endpoint>/v1/webhook-subscriptions (the most-likely
	// Phase 1 admin API path; operators may need to adjust per their
	// Polaris build).
	registerURL := strings.TrimRight(polarisEndpoint, "/") + "/v1/webhook-subscriptions"
	payload := map[string]any{
		"url":    neksurWebhookURL,
		"secret": secret,
		"events": []string{"snapshot.committed"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registerURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("polaris register status=%d body=%s", resp.StatusCode, string(respBody))
	}
	return nil
}

