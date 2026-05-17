// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// listener.go — Package-internal aliases for pkg/pglistener types + helpers.
//
// Plan 03-07 extracted the Postgres LISTEN/NOTIFY per-connection loop into
// pkg/pglistener (public, importable by neksur-commercial). This file provides
// package-internal aliases so trigger.go and any future consumers inside the
// compiler package can use the standard types without a long import path.
//
// The canonical LISTEN supervisor (ListenOn) and single-connection helper
// (ListenOnce) live in pkg/pglistener. trigger.go calls listenOnce (via the
// alias below) in its own reconnect-backoff loop, preserving the pollOnceStub
// fallback that policy_changed needs.
//
// ANTI-PATTERN: Do NOT introduce a second pgxpool.Pool. Both the Trigger
// and the Broadcaster MUST share the SAME admin pool.

package compiler

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/pkg/pglistener"
)

// ChannelListener is the handler contract for any LISTEN/NOTIFY consumer.
// Alias of pglistener.ChannelListener — identical interface, re-declared here
// so trigger.go does not need an explicit pglistener import.
type ChannelListener = pglistener.ChannelListener

// listenerFunc is a convenience adapter for functions satisfying ChannelListener.
// Alias of pglistener.Func.
type listenerFunc = pglistener.Func

// listenOnce acquires ONE connection from pool, issues LISTEN channelName, and
// loops on WaitForNotification calling listener.Handle for each notification.
// Returns nil on ctx.Done(); non-nil error on connection drop.
//
// Package-internal thin wrapper around pkg/pglistener.ListenOnce so trigger.go
// can call it without importing pglistener directly.
func listenOnce(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	return pglistener.ListenOnce(ctx, pool, channelName, listener)
}

// ListenOn runs the full LISTEN/NOTIFY supervisor (reconnect-backoff loop).
// Used by any package-internal caller that wants the full supervisor without
// adding the pollOnceStub fallback (e.g., future plans that add new channels
// without a fallback poll requirement).
func ListenOn(ctx context.Context, pool *pgxpool.Pool, channelName string, listener ChannelListener) error {
	return pglistener.ListenOn(ctx, pool, channelName, listener)
}

// asChannelListener adapts a plain function to ChannelListener.
// Convenience wrapper for tests and one-off usages.
func asChannelListener(fn func(ctx context.Context, notif *pgconn.Notification)) ChannelListener {
	return pglistener.Func(fn)
}
