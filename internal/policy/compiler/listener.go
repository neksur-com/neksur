// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// listener.go — Reusable Postgres LISTEN/NOTIFY supervisor loop.
//
// Plan 03-07 refactors the LISTEN consumer pattern originally implemented in
// trigger.go so that BOTH the policy_changed Trigger (Plan 02-04) AND the new
// schema_changed Broadcaster (Plan 03-07) can share the same reconnect-backoff
// supervisor without duplicating it.
//
// Chosen pattern: Option A — export a ChannelListener interface + a generic
// ListenOn supervisor function. The Trigger's listenOnce logic is extracted
// into a ListenOn helper that both implementations consume. The policy_changed
// Trigger delegates to ListenOn; the schemacache Broadcaster calls ListenOn
// with the "schema_changed" channel name and its own ChannelListener.
//
// CRITICAL anti-pattern (RESEARCH Anti-pattern, 03-CONTEXT §Code context):
// ListenOn accepts an *existing* *pgxpool.Pool — it does NOT construct a new
// pool. Both the Trigger and the Broadcaster MUST share the SAME admin pool.
// Introducing a second pool would break the Phase 0.5 BeforeAcquire DISCARD ALL
// invariant and the Pool A connection budget.

package compiler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChannelListener is the handler contract for any LISTEN/NOTIFY consumer.
// The schemacache Broadcaster implements ChannelListener; the policy_changed
// Trigger uses an inline closured adapter (see listenerFunc below).
//
// Implementations MUST NOT panic. All errors should be logged + swallowed
// inside Handle so the listen loop can continue. If Handle returns (it must
// not block indefinitely), the caller immediately blocks again on the next
// WaitForNotification.
type ChannelListener interface {
	// Handle is invoked exactly once per received notification. The ctx is
	// the context passed to ListenOn (cancelled on shutdown). notif is the
	// raw pgx notification — implementations unmarshal + dispatch here.
	Handle(ctx context.Context, notif *pgconn.Notification)
}

// listenerFunc is a convenience adapter so a plain function can satisfy
// ChannelListener without a named struct.
type listenerFunc func(ctx context.Context, notif *pgconn.Notification)

func (f listenerFunc) Handle(ctx context.Context, notif *pgconn.Notification) { f(ctx, notif) }

// ListenOn runs the LISTEN/NOTIFY supervisor loop for the given channel on the
// given pool, calling listener.Handle for every received notification.
//
// The supervisor loop provides:
//   - Connection acquisition from pool (one connection per ListenOn goroutine;
//     no second pool introduced — RESEARCH Anti-pattern).
//   - Exponential backoff capped at backoffMax (60s) between reconnect attempts.
//   - CR-03 healthy-window reset: if a connection ran for ≥ reconnectHealthy (30s)
//     before dropping, backoff + lastSuccess are reset to treat it as a transient
//     reconnect event rather than a reconnect storm.
//   - ctx cancellation propagation — returns ctx.Err() on context cancel/timeout.
//
// Callers should run ListenOn in a dedicated goroutine:
//
//	go func() {
//	    if err := compiler.ListenOn(ctx, pool, "schema_changed", myListener); err != nil {
//	        slog.Error("listener stopped", "err", err)
//	    }
//	}()
//
// channelName must be a Postgres IDENTIFIER controlled by the caller (not
// user-supplied input) — it is spliced verbatim into "LISTEN <channelName>".
// For the schema_changed and policy_changed channels this is always a constant.
func ListenOn(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	backoff := time.Second
	lastSuccess := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		listenStart := time.Now()
		err := listenOnce(ctx, pool, channelName, listener)
		if err == nil {
			// listenOnce returns nil only on ctx.Done().
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// CR-03 healthy-window reset: if the connection ran for ≥ 30s before
		// dropping, treat the failure as a transient reconnect (reset backoff).
		const reconnectHealthy = 30 * time.Second
		if time.Since(listenStart) >= reconnectHealthy {
			backoff = time.Second
			lastSuccess = time.Now()
		}

		slog.Error("policy/compiler/listener: listen error — reconnecting",
			"channel", channelName,
			"err", err,
			"backoff", backoff,
			"since_last_success", time.Since(lastSuccess))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// listenOnce acquires ONE connection from pool, issues LISTEN channelName, and
// loops on WaitForNotification calling listener.Handle for each notification.
// Returns nil on ctx.Done(); a non-nil error on connection drop or unrecoverable issue.
func listenOnce(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// pg LISTEN — channelName is a Postgres identifier controlled by the caller.
	if _, err := conn.Exec(ctx, "LISTEN "+channelName); err != nil {
		return fmt.Errorf("LISTEN %s: %w", channelName, err)
	}

	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("WaitForNotification: %w", err)
		}
		listener.Handle(ctx, notif)
	}
}
