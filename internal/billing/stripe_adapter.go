package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"
)

// stripeBilling is the real Stripe-backed Billing implementation. Inert
// until BILLING_ENABLED=true is flipped on at M7+; until then,
// NewBilling returns noopBilling and stripeBilling is never instantiated.
//
// For Phase 0.5 the Create / Cancel / RecordUsage methods are stubs
// returning explicit "awaiting M7" errors. The HandleWebhook method is
// production-shaped — signature verification runs through the Stripe SDK
// and the event dispatch switch is wired (no-op cases for now, to be
// expanded with real handlers in M7+). This lets us test webhook flow
// end-to-end before M7 by simply flipping BILLING_ENABLED=true.
type stripeBilling struct {
	cfg Config
}

func (s *stripeBilling) CreateSubscription(_ context.Context, _ CreateSubscriptionOpts) (SubscriptionID, error) {
	return "", fmt.Errorf("billing: CreateSubscription stub awaiting M7")
}

func (s *stripeBilling) CancelSubscription(_ context.Context, _ SubscriptionID) error {
	return fmt.Errorf("billing: CancelSubscription stub awaiting M7")
}

func (s *stripeBilling) RecordUsage(_ context.Context, _ SubscriptionID, _ string, _ int64, _ time.Time) error {
	return fmt.Errorf("billing: RecordUsage stub awaiting M7")
}

// HandleWebhook verifies the Stripe webhook signature via the SDK's
// ConstructEvent, then dispatches based on event.Type. For Phase 0.5
// every event type is a no-op (we only need signature verification to
// be wired); M7+ replaces the case stubs with real handlers.
//
// CRITICAL: signature verification runs BEFORE any other processing
// (D-0.5.21 T-0.5-stripe-spoof). The WebhookHandler at the HTTP layer
// also enforces verify-before-process by virtue of the interface
// contract — both noopBilling and stripeBilling do signature verify
// first thing.
func (s *stripeBilling) HandleWebhook(_ context.Context, body []byte, sigHeader string) error {
	event, err := webhook.ConstructEvent(body, sigHeader, s.cfg.WebhookSecret)
	if err != nil {
		return fmt.Errorf("billing: webhook verify: %w", ErrInvalidSignature)
	}

	switch event.Type {
	case "customer.subscription.created":
		// M7+: persist to public.tenant_billing; emit metric.
	case "customer.subscription.deleted":
		// M7+: transition tenant.lifecycle_state to 'suspended'.
	case "customer.subscription.updated":
		// M7+: update tier in public.tenant_billing.
	case "invoice.payment_failed":
		// M7+: enqueue dunning; PagerDuty info-severity event.
	}
	return nil
}
