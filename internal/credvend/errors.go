// Package credvend implements the L4 credential vending service per
// D-2.09 + REQ-write-l4-credential-vending. The service issues short-lived
// AWS STS tokens scoped to s3:PutObject on the table prefix in the allowed
// region, cached in-process and refreshed at TTL/2.
//
// Error contract mirrors Phase 1 D-1.09 fail-closed semantics:
//   - ErrCredVendUnavailable: returned when the upstream catalog (Polaris)
//     is unreachable or returns an unexpected error. Callers map this to
//     HTTP 503. Mirrors ErrPolicyEngineUnavailable from D-1.09.
//   - ErrEngineNotSupported: returned when the target catalog kind does not
//     support L4 credential vending (e.g., Glue, Unity in Phase 2 — both
//     return iceberg.ErrAdapterStub). Callers map this to HTTP 501.
//   - ErrSessionPolicyMalformed: returned when BuildSessionPolicy produces
//     JSON that fails validation (defensive; should not happen in practice
//     because the struct-typed policy ensures correct shape).
package credvend

import (
	"errors"

	"github.com/neksur-com/neksur/internal/credvend/sessionpolicy"
)

// ErrCredVendUnavailable mirrors ErrPolicyEngineUnavailable from Phase 1
// D-1.09: the credential vending service is fail-closed — any upstream
// error (Polaris unreachable, STS error, cache error) causes the handler
// to return HTTP 503 to Spark, which then cannot write to S3. The 503
// mapping is asserted by the handler's switch in handler.go (WR-A2 fix).
//
// The L4 gate is the strongest write-ACL protection per ADR-003 D-003.01:
// without scoped STS tokens Spark physically cannot write to managed S3
// buckets. Fail-closed behaviour is therefore load-bearing.
var ErrCredVendUnavailable = errors.New("credvend: credential vending unavailable")

// ErrEngineNotSupported is returned when the target catalog kind does not
// support L4 STS vending (Unity + Glue in Phase 2 — both adapters return
// iceberg.ErrAdapterStub; Phase 3 lights Unity live). Callers map this
// sentinel to HTTP 501 Not Implemented (matches handler.go switch — see
// WR-A2 fix). The 501 vs 503 split distinguishes a configuration-drift
// signal (an operator pointed a tenant at a stub-only catalog kind) from
// an incident signal (vending broken for a kind that should work).
var ErrEngineNotSupported = errors.New("credvend: engine does not support STS credential vending")

// ErrSessionPolicyMalformed is returned if the session policy JSON cannot
// be constructed (e.g., empty bucket derived from warehouse URI). In
// practice this should not be reachable with valid configuration; it
// exists so callers can errors.Is on the specific policy construction
// failure without catching the broader ErrCredVendUnavailable.
//
// Plan 02-13: this is an alias of sessionpolicy.ErrMalformed (the actual
// sentinel lives in the leaf subpackage to break the would-be import
// cycle through polaris). errors.Is works through either name.
var ErrSessionPolicyMalformed = sessionpolicy.ErrMalformed
