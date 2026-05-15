// Sentinel errors for the Phase 1 L1 Catalog Gateway.
//
// The gateway translates these to HTTP responses (Pitfall mapping per
// 01-RESEARCH.md §Pattern 5):
//
//   - ErrPolicyEngineUnavailable → 503 + commit_rejected_total{reason=
//                                  "policy_engine_unavailable"} (D-1.09
//                                  fail-closed; operator page).
//   - ErrPolicyDenied             → 403 + commit_rejected_total{reason=
//                                  "policy_denied"} + WriteEvent
//                                  REJECTED audit row.
//   - ErrUpstreamCatalogFailed    → 502 (transparent forward-failure;
//                                  the upstream catalog rejected the
//                                  commit on a non-conflict path).
//   - ErrPrincipalMissing         → 401 (no mTLS SAN, no Authorization
//                                  bearer, no WorkOS session — Pitfall 8).

package iceberg

import "errors"

// ErrPolicyEngineUnavailable is the fail-closed sentinel — the policy
// engine could not produce a verdict (compile error, eval error, panic,
// or AGE store fetch failure). Maps to HTTP 503.
var ErrPolicyEngineUnavailable = errors.New("gateway: policy engine unavailable")

// ErrPolicyDenied is returned when at least one P1/P2/P3 policy
// evaluated to ActionDeny. Maps to HTTP 403; the audit emission still
// fires (decision='REJECTED' + reason).
var ErrPolicyDenied = errors.New("gateway: policy denied")

// ErrUpstreamCatalogFailed wraps any non-conflict, non-credentials-
// expired error returned by the upstream catalog adapter on commit
// forward. Maps to HTTP 502 — the upstream could not accept the commit
// for reasons unrelated to policy.
var ErrUpstreamCatalogFailed = errors.New("gateway: upstream catalog failed")

// ErrPrincipalMissing is returned by ExtractPrincipal when none of the
// three Pitfall 8 chain steps produces a principal:
//   - no mTLS client certificate SAN
//   - no Authorization: Bearer header
//   - no tenant context attached by WorkOS middleware (defence-in-
//     depth: production paths cannot reach this state)
// Maps to HTTP 401.
var ErrPrincipalMissing = errors.New("gateway: principal context required")
