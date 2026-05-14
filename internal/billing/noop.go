package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"
)

// noopBilling is the Phase 0.5 default Billing implementation. It returns
// ErrBillingDisabled for every state-mutating method, but its HandleWebhook
// STILL verifies the Stripe webhook signature. This is the D-0.5.21
// T-0.5-stripe-spoof defense: even with BILLING_ENABLED=false, an
// attacker cannot trick Neksur into accepting an unsigned/forged Stripe
// webhook event by exploiting feature-flag misconfiguration.
//
// On valid-signature path: the event is acknowledged with nil error
// (the WebhookHandler returns 200) and otherwise ignored — no Stripe
// state to update under BILLING_ENABLED=false.
//
// On invalid-signature path: HandleWebhook returns ErrInvalidSignature,
// the WebhookHandler returns 400 with empty body (silent 400, no
// information leak; D-0.5.21).
type noopBilling struct {
	webhookSecret string
}

func (n *noopBilling) CreateSubscription(_ context.Context, _ CreateSubscriptionOpts) (SubscriptionID, error) {
	return "", ErrBillingDisabled
}

func (n *noopBilling) CancelSubscription(_ context.Context, _ SubscriptionID) error {
	return ErrBillingDisabled
}

func (n *noopBilling) RecordUsage(_ context.Context, _ SubscriptionID, _ string, _ int64, _ time.Time) error {
	return ErrBillingDisabled
}

// HandleWebhook verifies the Stripe webhook signature even though billing
// is disabled. This is the T-0.5-stripe-spoof defense at the implementation
// level (independent of the HTTP-layer signature checks): operator
// misconfig (BILLING_ENABLED accidentally set to "false" in production
// while a real Stripe webhook endpoint is exposed) does NOT downgrade the
// security posture.
func (n *noopBilling) HandleWebhook(_ context.Context, body []byte, sigHeader string) error {
	if _, err := webhook.ConstructEvent(body, sigHeader, n.webhookSecret); err != nil {
		return fmt.Errorf("billing: noop webhook: %w", ErrInvalidSignature)
	}
	return nil // signature valid; event ignored under BILLING_ENABLED=false
}
