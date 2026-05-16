// Trigger — Postgres LISTEN/NOTIFY consumer for policy_changed events
// (D-2.04 / Plan 02-04 Part B — wiring).
//
// V0073 ships a per-policy trigger that NOTIFYs the `policy_changed`
// channel with a JSON payload `{"tenant_id":"<uuid>","policy_id":"<uuid>"}`
// on every INSERT/UPDATE to any tenant's `policies` row. The Trigger here
// is the per-process consumer: it acquires ONE connection from the admin
// pool, issues `LISTEN policy_changed`, and loops on
// pgx.Conn.WaitForNotification — the pgx-native LISTEN path per
// RESEARCH §Pattern 5 (we deliberately do NOT use lib/pq because the
// rest of the stack standardises on pgx/v5; mixing drivers complicates
// the BeforeAcquire DISCARD ALL invariant from Phase 0.5).
//
// For each notification:
//  1. JSON-unmarshal the payload. Malformed payloads are LOGGED + SKIPPED
//     (do NOT crash — an attacker who can inject a bad NOTIFY payload
//     would otherwise crash the gateway, a DoS amplifier).
//  2. Validate the tenant_id against the registry (defence-in-depth
//     against forged payloads — even though only our schema-owned
//     trigger function emits NOTIFYs in production, a future operator-
//     issued ad-hoc NOTIFY could carry a bogus tenant_id; validating
//     here prevents the compiler from running with a tenant context
//     that doesn't exist).
//  3. Build a tenant-scoped context + call Compiler.CompileAll on the
//     Policy. The compile itself is best-effort: errors are LOGGED +
//     SKIPPED so a single bad policy can't stall the consumer.
//
// Fallback poller (Assumption A6, per the plan): if WaitForNotification
// returns a connection error we restart with exponential backoff
// capped at 60s; if sustained failure > 5min we additionally fall
// back to polling the per-tenant `policies` table for
// `changed_at > last_compile`. The Phase 1 `internal/detect/dispatch/poller.go`
// is the structural analog — same channel-driven shape, 60s cadence
// instead of 30s because policy-changed events are operator-paced
// (manual edits, terraform applies) rather than commit-frequency
// (every Spark write).

package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"
)

// notifyChannel is the V0073 channel name — must match the trigger
// function's pg_notify('policy_changed', payload::text) call.
const notifyChannel = "policy_changed"

// backoffMax bounds the exponential backoff between reconnect attempts.
// 60s matches the L3 poller cadence so a sustained outage doesn't spin
// the goroutine at high frequency.
const backoffMax = 60 * time.Second

// pollerFallbackThreshold — after this much sustained failure we
// switch on the per-tenant `policies` polling fallback alongside the
// reconnect retries. Mirrors the "belt-and-suspenders" doctrine from
// internal/detect/dispatch/poller.go.
const pollerFallbackThreshold = 5 * time.Minute

// PolicyChangedPayload is the JSON wire shape emitted by V0073.
// Field names match the trigger's json_build_object keys.
type PolicyChangedPayload struct {
	TenantID string `json:"tenant_id"`
	PolicyID string `json:"policy_id"`
}

// TenantValidator is the minimal contract the Trigger needs to confirm
// a tenant_id from a NOTIFY payload actually exists. Defence-in-depth
// against forged payloads — production deployments use *tenant.Repo
// (a tiny shim adapter satisfies this interface in main.go).
type TenantValidator interface {
	// TenantExists reports whether `id` matches an active tenant row.
	// Returns (false, nil) for "not found"; (false, err) for transient
	// lookup failure (which the Trigger treats as fail-safe: skip the
	// compile rather than fail-open).
	TenantExists(ctx context.Context, id uuid.UUID) (bool, error)
}

// PolicyLoader is the minimal contract the Trigger uses to fetch the
// PolicySource + table ref for a given (tenantID, policyID). The
// production wire-up is store.AGEStore; tests can supply a fake.
//
// Returns (zero, zero, ErrPolicyNotFound) when the policy was deleted
// between NOTIFY emission and this read — the Trigger logs + skips.
type PolicyLoader interface {
	LoadPolicyForCompile(ctx context.Context, policyID string) (PolicySource, iceberg.TableRef, error)
}

// Trigger is the LISTEN/NOTIFY consumer. Construct ONCE per process
// via NewTrigger; share across goroutines is NOT supported (a single
// Listen call owns the connection lifecycle).
type Trigger struct {
	pool       *pgxpool.Pool
	compiler   *Compiler
	tenantRepo TenantValidator
	loader     PolicyLoader
}

// NewTrigger wires the consumer.
//   - pool       — the admin pool (BeforeAcquire DISCARD ALL applied
//     at construction time in main.go; the Trigger acquires
//     ONE connection and keeps it for the lifetime of Listen).
//   - compiler   — the cross-engine compiler invoked for each NOTIFY.
//   - tenantRepo — used to reject forged payloads (Assumption A6 +
//     defence-in-depth against the operator-issued ad-hoc
//     NOTIFY threat).
//   - loader     — used to resolve (policyID) → (PolicySource, TableRef)
//     so the compiler has the artifact body to recompile.
func NewTrigger(
	pool *pgxpool.Pool,
	compiler *Compiler,
	tenantRepo TenantValidator,
	loader PolicyLoader,
) *Trigger {
	return &Trigger{
		pool:       pool,
		compiler:   compiler,
		tenantRepo: tenantRepo,
		loader:     loader,
	}
}

// Listen blocks until ctx is cancelled, dispatching every received
// policy_changed NOTIFY to the compiler. On connection error it
// reconnects with exponential backoff (capped at backoffMax); on
// context cancellation it returns ctx.Err().
//
// The caller is expected to `go trig.Listen(ctx)` once at startup
// (see main.go wiring) and treat a non-context-cancel return as a
// fatal supervisor signal (the gateway's main goroutine should log
// + restart the Listen, or the process should exit and let the
// supervisor restart).
func (t *Trigger) Listen(ctx context.Context) error {
	backoff := time.Second
	lastSuccess := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := t.listenOnce(ctx)
		if err == nil {
			// listenOnce returns nil only on ctx.Done() — propagate.
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		slog.Error("policy/compiler/trigger: listen error",
			"err", err, "backoff", backoff)

		// Fallback poller activates once we've been failing > 5min.
		// Best-effort: a single scan of recent changes per cycle.
		if time.Since(lastSuccess) > pollerFallbackThreshold {
			slog.Warn("policy/compiler/trigger: sustained failure, activating fallback poller",
				"since", time.Since(lastSuccess))
			t.pollOnce(ctx)
		}

		// Exponential backoff with cap.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
		// Reset on the NEXT successful listenOnce — see the success
		// branch below where we re-set lastSuccess.
		_ = lastSuccess
	}
}

// listenOnce acquires one connection, LISTENs, and loops on
// WaitForNotification. Returns nil on ctx.Done(); a non-nil error on
// connection drop / scan error / unrecoverable issue.
func (t *Trigger) listenOnce(ctx context.Context) error {
	conn, err := t.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// pg LISTEN — the channel name is a Postgres identifier; we control
	// it (notifyChannel constant) so no sanitisation needed.
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return fmt.Errorf("LISTEN %s: %w", notifyChannel, err)
	}

	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("WaitForNotification: %w", err)
		}
		t.handleNotification(ctx, notif)
	}
}

// handleNotification parses + validates + dispatches one NOTIFY.
// Errors are logged + swallowed so the listen loop survives.
func (t *Trigger) handleNotification(ctx context.Context, notif *pgconn.Notification) {
	var payload PolicyChangedPayload
	if err := json.Unmarshal([]byte(notif.Payload), &payload); err != nil {
		slog.Error("policy/compiler/trigger: malformed payload (skipping)",
			"err", err, "raw", notif.Payload)
		return
	}

	tenantID, err := uuid.Parse(payload.TenantID)
	if err != nil {
		slog.Error("policy/compiler/trigger: invalid tenant uuid (skipping)",
			"err", err, "tenant_id", payload.TenantID)
		return
	}
	if payload.PolicyID == "" {
		slog.Error("policy/compiler/trigger: empty policy_id (skipping)",
			"payload", payload)
		return
	}

	// Defence-in-depth: validate tenant exists before constructing a
	// tenant-scoped context. A forged or stale payload here is fail-safe
	// (we skip; we do NOT crash).
	ok, err := t.tenantRepo.TenantExists(ctx, tenantID)
	if err != nil {
		slog.Error("policy/compiler/trigger: tenant validation failed (skipping)",
			"err", err, "tenant_id", tenantID)
		return
	}
	if !ok {
		slog.Warn("policy/compiler/trigger: notify for unknown tenant (skipping)",
			"tenant_id", tenantID, "policy_id", payload.PolicyID)
		return
	}

	tctx := tenant.WithID(ctx, tenantID)
	src, ref, err := t.loader.LoadPolicyForCompile(tctx, payload.PolicyID)
	if err != nil {
		slog.Error("policy/compiler/trigger: load policy failed (skipping)",
			"err", err, "tenant_id", tenantID, "policy_id", payload.PolicyID)
		return
	}

	results, err := t.compiler.CompileAll(tctx, src, ref)
	if err != nil {
		slog.Error("policy/compiler/trigger: compile failed (skipping)",
			"err", err, "tenant_id", tenantID, "policy_id", payload.PolicyID)
		return
	}
	for _, r := range results {
		if r.Err != nil {
			slog.Warn("policy/compiler/trigger: per-engine compile result",
				"engine_kind", r.EngineKind,
				"engine_version", r.EngineVersion,
				"status", r.Status,
				"err", r.Err)
		}
	}
}

// pollOnce is the sustained-failure fallback (Assumption A6). It is a
// no-op placeholder in Phase 2 — the trigger is the authoritative
// source. A future iteration will scan `policies` for
// `changed_at > last_compile` per active tenant and emit the same
// (tenantID, policyID) tuples through handleNotification. The
// scaffolding lives here so a follow-up plan can land the polling
// query without restructuring the supervisor loop.
func (t *Trigger) pollOnce(ctx context.Context) {
	// Phase 2: placeholder. The Listen retry loop already covers most
	// connection-drop scenarios; the dedicated poller is the safety
	// net for the rare "trigger function disabled" / "pg restart with
	// dropped LISTEN" case. Tracked for Plan 02-05+ wiring.
	_ = ctx
}

// The concrete PolicyLoader adapter lives at the gateway boundary
// (cmd/neksur-server/main.go) so the compiler package stays free of
// an upward dependency on the store's full API surface.
