// Package chaos hosts the Patroni chaos-engineering driver — STUB.
//
// Function signatures are locked in this Wave 0/1 correction so
// downstream plans (specifically Plan 00-03 — Wave 2 Patroni HA)
// can import them without circular failure. Real implementation
// lands in Plan 03 against a 3-node Patroni + etcd cluster spun
// up via testcontainers / docker-compose.
//
// Originally Python's tests/chaos/lib/patroni_chaos.py under the
// Wave 0 plan; now Go per the 2026-05-13 D-PHASE0-stack correction.
package chaos

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented is the sentinel returned by every function in this
// stub. Plan 03 (Wave 2 — Patroni HA) will replace each function body
// with the real implementation; downstream callers can either expect
// nil error post-Plan-03 OR check errors.Is(err, ErrNotImplemented) to
// branch on stub vs real.
var ErrNotImplemented = errors.New("filled by Plan 03 — Wave 2 Patroni")

// KillPrimary SIGKILLs the current Patroni primary in clusterName to
// provoke a failover. Plan 03 will resolve the leader via the Patroni
// REST endpoint `GET /cluster`, then issue `docker kill --signal=SIGKILL`
// to that container.
func KillPrimary(ctx context.Context, clusterName string) error {
	_ = ctx
	_ = clusterName
	return ErrNotImplemented
}

// WaitForNewLeader polls the Patroni REST API every 500ms until a new
// node holds the leader lock. Returns the new leader's name. Plan 03
// uses `GET /cluster` and watches for `role == "primary"` on a member
// other than the previously-killed one. Returns context.DeadlineExceeded
// or context.Canceled if ctx expires before a new leader is observed.
func WaitForNewLeader(ctx context.Context, clusterName string, timeout time.Duration) (string, error) {
	_ = ctx
	_ = clusterName
	_ = timeout
	return "", ErrNotImplemented
}

// TimeFailover composes KillPrimary + WaitForNewLeader with a monotonic
// wall-clock measurement. Plan 03 + D-001.15 require P95 of repeated
// runs to be <30s; this function returns the single-run wall-clock
// duration for the caller's aggregation.
func TimeFailover(ctx context.Context, clusterName string) (time.Duration, error) {
	_ = ctx
	_ = clusterName
	return 0, ErrNotImplemented
}
