package workos

import (
	"encoding/json"
	"io"
	"net/http"
	"os"

	"github.com/workos/workos-go/v4/pkg/webhooks"
)

// webhookEvent is the minimum we decode from a verified webhook body.
// Phase 0.5 leaves the SCIM-enabled switch cases empty; Phase 1+ wires
// dsync.user.created / dsync.user.deleted handlers.
type webhookEvent struct {
	Type string `json:"event"` // WorkOS uses `event` as the discriminator field
}

// HandleWebhook returns an http.HandlerFunc that verifies the
// WorkOS-Signature header against `secret` BEFORE any feature-flag
// check or JSON decode. This ordering is non-negotiable per
// D-0.5.21 T-0.5-webhook-spoof-workos:
//
//  1. Read raw body bytes with a 256KiB cap (defense vs. memory DoS).
//  2. Verify WorkOS-Signature against `secret` using the SDK's
//     webhooks.Client.ValidatePayload (constant-time HMAC SHA-256
//     with 180s timestamp tolerance; supports the `t=<unix_ms>,v1=<hex>`
//     WorkOS header format and rejects wrong-format headers).
//  3. ONLY after verification, check SCIM_ENABLED — if not "true",
//     return 200 (ack + no-op).
//  4. SCIM-enabled path (Phase 1+): unmarshal + dispatch.
//
// Failure modes all return 400 with EMPTY response body. WorkOS treats
// 4xx as a retry signal; the empty body avoids leaking implementation
// detail to a spoofer probing the surface.
//
// Why we read body bytes BEFORE verification (RESEARCH Pitfall 4 line 1101):
// the signature is over the raw payload bytes. A `json.Decode(r.Body, ...)`
// would consume the reader and re-serialise, changing whitespace and
// invalidating the HMAC. We must buffer the raw bytes, verify, THEN
// unmarshal.
//
// The 256KiB cap is a defense against an attacker sending a huge body
// to consume memory/CPU; legitimate WorkOS webhooks are well under 100KiB.
func (c *Client) HandleWebhook(secret string) http.HandlerFunc {
	verifier := webhooks.NewClient(secret)

	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Read raw bytes (capped). io.ReadAll over MaxBytesReader.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
		if err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}

		// 2. Verify signature. The SDK's ValidatePayload parses the
		//    `t=...,v1=...` header, checks the 180s timestamp tolerance,
		//    and constant-time-compares the HMAC. On any failure path,
		//    we return 400 silently — no body, no headers, no hints.
		sig := r.Header.Get("WorkOS-Signature")
		if _, err := verifier.ValidatePayload(sig, string(body)); err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}

		// 3. Verified — now we can check the feature flag.
		if os.Getenv("SCIM_ENABLED") != "true" {
			// Acknowledge without dispatching. WorkOS treats 2xx as
			// delivery success; the next-poll won't redeliver this
			// event. That's the intended Phase 0.5 behavior: webhooks
			// land, we count them (TODO Phase 1+: metric), but we
			// don't dispatch.
			w.WriteHeader(http.StatusOK)
			return
		}

		// 4. SCIM-enabled path (Phase 1+ — Plan 03 leaves stubs).
		var ev webhookEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		switch ev.Type {
		case "dsync.user.created":
			// TODO Phase 1+: create OrganizationMembership in WorkOS,
			// upsert Neksur user, assign role.
		case "dsync.user.deleted":
			// TODO Phase 1+: soft-delete user; preserve audit log entries.
		case "dsync.group.created":
			// TODO Phase 1+: SCIM group → Neksur role mapping.
		}
		w.WriteHeader(http.StatusOK)
	}
}
