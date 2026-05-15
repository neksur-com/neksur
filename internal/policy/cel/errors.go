// Package cel implements the Phase 1 policy expression engine (D-1.07).
//
// The engine is fail-closed (D-1.09): any failure path — compile error,
// evaluation error, evaluator panic, non-bool return — is wrapped in an
// EvalError that the L1 gateway (Plan 01-06) translates to HTTP 503 +
// `commit_rejected_total{reason="policy_engine_unavailable"}` increment.
// "Default-deny on compile failure" (SPEC v0.7) is preserved at every
// layer of this package.
//
// This file owns the sentinel error catalog. Every wrapped error
// produced by env.go / compile.go / eval.go / functions.go discriminates
// against one of these sentinels via errors.Is / errors.Join so callers
// can branch on the failure mode without string-matching:
//
//   - ErrCompileFailed         — env.Compile returned non-nil issues.
//   - ErrPolicyEvalFailed      — prog.ContextEval returned non-nil err.
//   - ErrPolicyReturnedNonBool — prog returned a non-bool ref.Val.
//   - ErrEvalPanic             — recover() inside Evaluate caught a panic.
//
// EvalError is the wrapper type — it carries the offending PolicyID for
// log/triage and the underlying cause for errors.Is unwrapping.

package cel

import (
	"errors"
	"fmt"
)

// Sentinel errors. Per the project's PATTERNS CC5 + Phase 0
// internal/graph/client.go (ErrUnboundedTraversal) convention, the
// sentinels live in the same file as the wrapper type so callers
// branch on errors.Is(err, cel.Err…) rather than substring-matching.
var (
	// ErrCompileFailed is the sentinel for any failure inside
	// env.Compile / env.Program — i.e., the policy text is not valid
	// CEL or references undeclared variables/functions. The L1 gateway
	// MUST treat this as a hard 503 (D-1.09 fail-closed): the policy
	// is unevaluable so we cannot prove the commit is allowed.
	ErrCompileFailed = errors.New("cel: compile failed")

	// ErrPolicyEvalFailed is the sentinel for any non-nil error
	// returned by cel.Program.ContextEval. Includes runtime errors
	// (e.g., divide-by-zero, type mismatch in operator), undefined
	// variable references, and binding errors. Always fail-closed.
	ErrPolicyEvalFailed = errors.New("cel: policy eval failed")

	// ErrPolicyReturnedNonBool is the sentinel for the case where the
	// CEL expression evaluated successfully but returned a non-bool
	// value (e.g., the policy author wrote `42` or `"yes"` instead of
	// a predicate). Treated as a fail-closed error so an accidentally
	// truthy non-bool value cannot bypass policy.
	ErrPolicyReturnedNonBool = errors.New("cel: policy returned non-bool")

	// ErrEvalPanic is the sentinel for the case where the cel-go
	// runtime panicked during evaluation (defence in depth — cel-go
	// should not panic, but we don't trust the customer-authored CEL
	// expression). Recovered inside Evaluate via defer. Always
	// fail-closed.
	ErrEvalPanic = errors.New("cel: eval panic")
)

// EvalError wraps an underlying cause with the offending policy ID for
// triage. Implements errors.Unwrap so callers branch on the cause via
// errors.Is(err, ErrCompileFailed) etc., regardless of the wrapping.
//
// Example caller pattern:
//
//	dec, err := evaluator.Evaluate(ctx, policy, inputs)
//	if err != nil {
//	    var evErr *cel.EvalError
//	    if errors.As(err, &evErr) {
//	        slog.Error("policy eval failed", "policy", evErr.PolicyID, "cause", evErr.Cause)
//	    }
//	    if errors.Is(err, cel.ErrEvalPanic) {
//	        metrics.CommitRejectedTotal.WithLabelValues("policy_engine_unavailable").Inc()
//	    }
//	    http.Error(w, "policy engine unavailable", http.StatusServiceUnavailable)
//	    return
//	}
type EvalError struct {
	PolicyID string
	Cause    error
}

// Error implements the error interface. Format: `cel: policy <id>: <cause>`.
// PolicyID is included in the message body so log scrapers can filter on
// the literal "policy <id>" substring.
func (e *EvalError) Error() string {
	return fmt.Sprintf("cel: policy %s: %v", e.PolicyID, e.Cause)
}

// Unwrap returns the underlying cause so callers branching via
// errors.Is(err, sentinel) traverse the wrap chain correctly.
func (e *EvalError) Unwrap() error { return e.Cause }
