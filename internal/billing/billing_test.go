package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNoopBillingCreateSubscriptionReturnsDisabledErr: under
// BILLING_ENABLED=false (cfg.Enabled=false), NewBilling returns
// noopBilling whose CreateSubscription returns ErrBillingDisabled.
// D-0.5.12 contract.
func TestNoopBillingCreateSubscriptionReturnsDisabledErr(t *testing.T) {
	b := NewBilling(Config{Enabled: false, WebhookSecret: "whsec_test"})
	_, err := b.CreateSubscription(context.Background(), CreateSubscriptionOpts{
		TenantID: "tenant-123",
		PriceID:  "price_test",
		Quantity: 1,
	})
	if err == nil {
		t.Fatal("expected ErrBillingDisabled, got nil")
	}
	if !errors.Is(err, ErrBillingDisabled) {
		t.Fatalf("expected ErrBillingDisabled, got %v", err)
	}
}

// TestNoopBillingCancelSubscriptionReturnsDisabledErr: same as above
// for CancelSubscription.
func TestNoopBillingCancelSubscriptionReturnsDisabledErr(t *testing.T) {
	b := NewBilling(Config{Enabled: false, WebhookSecret: "whsec_test"})
	err := b.CancelSubscription(context.Background(), SubscriptionID("sub_test"))
	if !errors.Is(err, ErrBillingDisabled) {
		t.Fatalf("expected ErrBillingDisabled, got %v", err)
	}
}

// TestNoopBillingRecordUsageReturnsDisabledErr: same as above
// for RecordUsage.
func TestNoopBillingRecordUsageReturnsDisabledErr(t *testing.T) {
	b := NewBilling(Config{Enabled: false, WebhookSecret: "whsec_test"})
	err := b.RecordUsage(context.Background(), SubscriptionID("sub_test"), "api_calls", 100, time.Now())
	if !errors.Is(err, ErrBillingDisabled) {
		t.Fatalf("expected ErrBillingDisabled, got %v", err)
	}
}

// TestNoopBillingHandleWebhookInvalidSignature: T-0.5-stripe-spoof
// implementation-level defense. With BILLING_ENABLED=false, an unsigned
// or wrong-signature webhook STILL returns ErrInvalidSignature — the
// signature verification runs even when billing is disabled.
func TestNoopBillingHandleWebhookInvalidSignature(t *testing.T) {
	b := NewBilling(Config{Enabled: false, WebhookSecret: "whsec_test"})
	body := []byte(`{"id":"evt_test","type":"customer.subscription.created"}`)
	err := b.HandleWebhook(context.Background(), body, "t=0,v1=invalidhex")
	if err == nil {
		t.Fatal("expected ErrInvalidSignature, got nil")
	}
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

// TestNewBillingFactoryReturnsStripeWhenEnabled: when Enabled=true,
// NewBilling returns the *stripeBilling implementation (verified by
// type assertion). This is the M7-activation switch — flipping
// BILLING_ENABLED to true makes the real adapter take over with no
// code change.
func TestNewBillingFactoryReturnsStripeWhenEnabled(t *testing.T) {
	b := NewBilling(Config{Enabled: true, APIKey: "sk_test_dummy", WebhookSecret: "whsec_test"})
	if _, ok := b.(*stripeBilling); !ok {
		t.Fatalf("expected *stripeBilling, got %T", b)
	}
}

// TestNewBillingFactoryReturnsNoopWhenDisabled: dual of the above.
// Phase 0.5 baseline — Enabled=false returns noopBilling.
func TestNewBillingFactoryReturnsNoopWhenDisabled(t *testing.T) {
	b := NewBilling(Config{Enabled: false, WebhookSecret: "whsec_test"})
	if _, ok := b.(*noopBilling); !ok {
		t.Fatalf("expected *noopBilling, got %T", b)
	}
}
