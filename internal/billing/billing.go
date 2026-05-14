// Package billing is the Phase 0.5 Stripe stub at depth (c) per D-0.5.12
// and R7 mitigation per ADR-004 §5.
//
// Design contract:
//   - The package exports a single `Billing` interface that every code path
//     touching billing operations consumes. There are TWO implementations:
//       * `stripeBilling` — real Stripe API calls (inert until M7+ activates
//         BILLING_ENABLED=true).
//       * `noopBilling`   — returns ErrBillingDisabled for every method
//         except HandleWebhook, which STILL verifies signatures even when
//         BILLING_ENABLED=false (D-0.5.21 T-0.5-stripe-spoof mitigation).
//   - `NewBilling(cfg Config)` picks the implementation based on cfg.Enabled.
//   - M7 activation is a single flag flip: set BILLING_ENABLED=true, populate
//     the Stripe API key in Secrets Manager, and `stripeBilling` takes over.
//
// Stripe SDK version: `github.com/stripe/stripe-go/v82` (RESEARCH §Standard
// Stack line 140). API version is pinned via `stripe.SetAPIKey` + `stripe-go`
// defaults — the SDK version corresponds to API version 2025-03-31.basil.
package billing

import (
	"context"
	"errors"
	"time"
)

// SubscriptionID is an opaque identifier for a Stripe subscription
// (or a Noop placeholder that always errors with ErrBillingDisabled).
type SubscriptionID string

// CreateSubscriptionOpts is the input to Billing.CreateSubscription.
//
// PriceID maps to a Stripe Price object (one-to-one with a plan / tier);
// Quantity is the per-tenant seat or usage-unit count.
type CreateSubscriptionOpts struct {
	TenantID string
	PriceID  string
	Quantity int64
}

// Billing is the interface every billing operation in Neksur SaaS goes
// through. Both stripeBilling and noopBilling implement it.
//
// Threat model contract: HandleWebhook MUST verify the signature before
// any state-mutating side effect. The noop implementation MUST also
// verify signatures (defense-in-depth against BILLING_ENABLED misconfig
// per D-0.5.21 T-0.5-stripe-spoof).
type Billing interface {
	CreateSubscription(ctx context.Context, opts CreateSubscriptionOpts) (SubscriptionID, error)
	CancelSubscription(ctx context.Context, id SubscriptionID) error
	RecordUsage(ctx context.Context, sub SubscriptionID, meter string, qty int64, ts time.Time) error
	HandleWebhook(ctx context.Context, body []byte, sigHeader string) error
}

// Sentinel errors.
//
// ErrBillingDisabled is returned by noopBilling for every non-webhook
// operation. Callers can check `errors.Is(err, billing.ErrBillingDisabled)`
// to distinguish a feature-flag short-circuit from a Stripe API error.
//
// ErrInvalidSignature is returned by both implementations' HandleWebhook
// when stripe.Webhook.ConstructEvent rejects the payload. The
// WebhookHandler maps this to HTTP 400 with empty body (D-0.5.21).
var (
	ErrBillingDisabled  = errors.New("billing: disabled (BILLING_ENABLED=false)")
	ErrInvalidSignature = errors.New("billing: webhook signature invalid")
)

// Config is the input to NewBilling. Sourced from environment + Secrets
// Manager at startup (cmd/neksur-server/main.go).
//
// Enabled gates whether the real Stripe adapter is constructed:
//   - false (Phase 0.5 baseline): NewBilling returns &noopBilling{...}.
//   - true  (M7+):                NewBilling returns &stripeBilling{...}.
//
// WebhookSecret MUST be populated regardless of Enabled — noopBilling
// still uses it to verify signatures. APIKey is only meaningful when
// Enabled=true; otherwise it's allowed to be empty.
type Config struct {
	Enabled       bool
	APIKey        string
	WebhookSecret string
}

// NewBilling returns the appropriate Billing implementation based on
// cfg.Enabled. Phase 0.5 baseline: returns &noopBilling{...}. M7+
// activation is a single-flag flip (set BILLING_ENABLED=true, populate
// the Stripe API key in Secrets Manager); no code change.
func NewBilling(cfg Config) Billing {
	if !cfg.Enabled {
		return &noopBilling{webhookSecret: cfg.WebhookSecret}
	}
	return &stripeBilling{cfg: cfg}
}
