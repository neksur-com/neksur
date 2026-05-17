// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// Package pglistener provides a reusable Postgres LISTEN/NOTIFY supervisor loop.
//
// This package is PUBLIC so that external modules (neksur-commercial,
// neksur-enterprise) can use the same LISTEN/NOTIFY supervisor without
// duplicating the reconnect-backoff logic or introducing second pools.
//
// Key design invariant (RESEARCH Anti-pattern): callers MUST pass the SAME
// *pgxpool.Pool used by the policy_changed Trigger. ListenOn acquires ONE
// connection from the pool — no second pool is introduced.
//
// The internal/policy/compiler package re-exports the shared listenOnce helper
// from this package; the trigger.go supervisor loop remains in compiler so the
// pollOnceStub fallback can reference compiler-internal types without a circular
// import.

package pglistener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backoffMax bounds the exponential backoff between reconnect attempts.
const backoffMax = 60 * time.Second

// ChannelListener is the handler contract for any LISTEN/NOTIFY consumer.
// The schemacache Broadcaster implements ChannelListener; the policy_changed
// Trigger uses an inline adapter.
//
// Implementations MUST NOT panic. All errors should be logged + swallowed
// inside Handle so the listen loop can continue.
type ChannelListener interface {
	// Handle is invoked exactly once per received notification.
	Handle(ctx context.Context, notif *pgconn.Notification)
}

// Func is a convenience adapter so a plain function can satisfy ChannelListener.
type Func func(ctx context.Context, notif *pgconn.Notification)

func (f Func) Handle(ctx context.Context, notif *pgconn.Notification) { f(ctx, notif) }

// ListenOn runs the LISTEN/NOTIFY supervisor loop for the given channel on the
// given pool, calling listener.Handle for every received notification.
//
// The supervisor provides:
//   - Connection acquisition from pool (ONE connection per goroutine; no second pool).
//   - Exponential backoff capped at 60s between reconnect attempts.
//   - CR-03 healthy-window reset: if a connection ran ≥30s before dropping,
//     backoff is reset to treat it as a transient event.
//   - ctx cancellation propagation — returns ctx.Err() on cancel/timeout.
//
// channelName must be a Postgres IDENTIFIER controlled by the caller (not
// user-supplied) — it is spliced verbatim into "LISTEN <channelName>".
func ListenOn(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	backoff := time.Second

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		listenStart := time.Now()
		err := listenOnce(ctx, pool, channelName, listener)
		if err == nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// CR-03: if the connection ran ≥30s before dropping, treat as transient.
		const reconnectHealthy = 30 * time.Second
		if time.Since(listenStart) >= reconnectHealthy {
			backoff = time.Second
		}

		slog.Error("pglistener: listen error — reconnecting",
			"channel", channelName,
			"err", err,
			"backoff", backoff)

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

// ListenOnce acquires ONE connection from pool, issues LISTEN channelName, and
// loops on WaitForNotification calling listener.Handle for each notification.
// Returns nil on ctx.Done(); non-nil error on connection drop or unrecoverable issue.
//
// ListenOnce does NOT reconnect on failure — callers that need reconnect-backoff
// should use ListenOn (the full supervisor) instead. ListenOnce is intended for
// use by callers that manage their own reconnect loop (e.g., trigger.go's Listen
// which adds a pollOnceStub fallback on top of the reconnect loop).
func ListenOnce(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	return listenOnce(ctx, pool, channelName, listener)
}

// listenOnce is the unexported single-connection implementation.
func listenOnce(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

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
