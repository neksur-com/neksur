package billing

import (
	"io"
	"net/http"
)

// WebhookHandler returns an http.HandlerFunc that:
//   1. Reads the RAW request body (max 256 KiB) — Pitfall 4 from RESEARCH:
//      Stripe signs the raw bytes, JSON-decoding first invalidates the
//      signature.
//   2. Reads the Stripe-Signature header.
//   3. Delegates to Billing.HandleWebhook (interface dispatch), which
//      enforces signature verification BEFORE any state mutation:
//        - stripeBilling: ConstructEvent → switch on event.Type
//        - noopBilling:   ConstructEvent → return; ignore event
//      Both error out with ErrInvalidSignature on bad signatures.
//   4. On any non-nil error from HandleWebhook, returns HTTP 400 with
//      empty body (silent 400 per D-0.5.21 — no body, no information
//      leak about which error path triggered).
//   5. On success, returns HTTP 200.
//
// The `secret` parameter is unused at this layer — it lives inside the
// Billing implementation (noopBilling.webhookSecret / stripeBilling.cfg.
// WebhookSecret). We accept it as an argument anyway for symmetry with
// the WorkOS webhook handler signature (cmd/neksur-server/main.go wires
// both side-by-side).
func WebhookHandler(b Billing, _ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// RAW body — DO NOT JSON-decode before this point (Pitfall 4).
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
		if err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}

		sigHeader := r.Header.Get("Stripe-Signature")

		// Signature verification runs BEFORE the BILLING_ENABLED short-
		// circuit (the short-circuit is INSIDE noopBilling, which still
		// verifies signatures first). D-0.5.21 T-0.5-stripe-spoof.
		if err := b.HandleWebhook(r.Context(), body, sigHeader); err != nil {
			// 400 silent — no body — per D-0.5.21
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
