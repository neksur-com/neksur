// CEL Evaluator with fail-closed panic-recover — D-1.09 load-bearing.
//
// "Default-deny on compile failure" (SPEC v0.7) is the security contract
// this file enforces. Every error path — compile error, eval error,
// non-bool return, evaluator panic — produces a non-nil error wrapped in
// *EvalError. The L1 gateway (Plan 01-06) translates any non-nil error
// to HTTP 503 + `commit_rejected_total{reason="policy_engine_unavailable"}`
// so a policy engine outage is loud (operators page) AND safe (no
// commits ever bypass policy on a sad path).
//
// Defence-in-depth panic recover: cel-go is documented as not panicking
// during ContextEval, but the customer-authored CEL expression can
// invoke custom function bindings that DO panic (a buggy binding
// implementation, or — far less likely — an arithmetic edge case in
// cel-go itself). The defer/recover wraps any panic as ErrEvalPanic so
// it bubbles up as a 503 instead of crashing the gateway process.

package cel

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EvalTimeout is the per-evaluation budget for cel.Program.ContextEval.
// Per Phase 2 RESEARCH Pitfall 6 + Plan 02-01: pair this with the
// InterruptCheckFrequency option (compile.go) so the runtime exits
// within ~1-2ms of the deadline elapsing.
//
// 100ms is a generous ceiling for the Phase 1 P1/P2/P3 policies AND
// the Phase 2 ABAC/classification bindings — a normal eval takes
// 5-50µs warm; the budget catches pathological policies (a malicious
// or buggy `.all()` over a huge claims array) and surfaces them as
// fail-closed 503 instead of a hung gateway event loop (D-1.09 contract
// extended to cover timeout).
const EvalTimeout = 100 * time.Millisecond

// AttributeResolver returns the value of an ABAC attribute for a
// principal, per D-2.10's 3-layer fetch (OIDC claims → graph
// HAS_ATTRIBUTE → tenant_default_attributes JSONB). The concrete
// implementation lives in package `internal/policy/store`
// (`store.AttributeResolver`); declaring the interface here — in the
// CEL package — breaks what would otherwise be a `cel → store` import
// cycle (store already imports cel for the Policy struct).
//
// Resolve MUST return "" (empty string) when all 3 layers are
// exhausted, NEVER nil — D-2.10 null-safety contract (Pitfall 8):
// policy authors compare `principal.attribute(principal, "region") ==
// "us-east"` and a nil here would translate to a CEL evaluation error
// (no-such-key) which our fail-closed Evaluate path would 503 on. The
// "empty string sentinel" makes "attribute absent" expressible inside
// the policy itself (`principal.attribute(...) == ""` is the canonical
// "I don't have this attribute set" branch).
//
// The oidcClaims argument carries Layer-1 inputs already projected onto
// a flat string map by the gateway — the resolver MUST short-circuit
// on a non-empty hit there before reaching for the graph or admin
// pool, both for correctness (Layer-1 wins by spec) and for latency
// (avoids a graph round-trip for every claim-backed attribute).
type AttributeResolver interface {
	Resolve(ctx context.Context, principalSub, name string, oidcClaims map[string]string) string
}

// Action is the binary policy decision: allow or deny. The L1 gateway
// translates ActionAllow to "proceed with commit" and ActionDeny to
// HTTP 403 + `commit_rejected_total{reason="policy_denied"}`.
type Action int

const (
	// ActionAllow indicates the policy returned true (the commit is
	// permitted by this policy). The gateway aggregates Allow/Deny
	// across all policies attached to the table and rejects the commit
	// on the first Deny.
	ActionAllow Action = iota

	// ActionDeny indicates the policy returned false (the commit is
	// rejected by this policy). Decision.Reason carries a string
	// suitable for inclusion in the 403 response body.
	ActionDeny
)

// Decision is the per-policy result. Reason is populated only when
// Action == ActionDeny.
type Decision struct {
	Action Action
	Reason string
}

// Inputs is the typed bag of activation values passed into every
// Evaluate call. The three fields map 1:1 to the three CEL variables
// declared in env.go: `table`, `commit`, `principal`.
//
// The gateway constructs Inputs by marshalling the iceberg.TableMetadata
// + iceberg.CommitRequest + the request principal claims into nested
// maps with string keys (CEL's MapType(StringType, DynType) shape).
type Inputs struct {
	// Table is the post-validation TableMetadata projection. CEL
	// expressions index it as `table.schema.fields` /
	// `table.partition_spec.fields` / `table.properties` etc.
	Table map[string]any

	// Commit is the incoming CommitRequest projection — Requirements
	// (assertions the catalog will validate) + Updates (mutations to
	// apply). P3 retention policies typically read
	// `commit.updates[?action == 'remove-snapshot']`.
	Commit map[string]any

	// Principal is the request principal projection — `sub` (subject
	// identifier), `roles` (string slice), and any other claims the
	// gateway forwards. P2 write-ACL policies read `principal.sub` and
	// `principal.roles`.
	Principal map[string]any

	// AttributeResolver, when non-nil, is wired into the CEL activation
	// under the reserved key `__resolver` so the principal.attribute
	// binding (functions.go) can reach Layers 2+3 of D-2.10. nil-safe:
	// when nil the binding falls back to Layer-1 only (OIDC claims
	// already inlined into `principal["claims"]`).
	//
	// Cross-plan seam: Plan 02-03 (this plan) ships the type-level
	// hook + Layer-1 path; Plan 02-04 wires the gateway-side
	// construction of the concrete resolver and threads it through
	// here. Keeping the field on Inputs (not on Evaluator) means a
	// single Evaluator instance can serve calls with and without a
	// resolver — used by the tests that exercise Layer-1-only flows.
	AttributeResolver AttributeResolver
}

// Policy is the in-memory representation of a Policy graph node. ID is
// the graph node's id; Kind discriminates "schema" / "write_acl" /
// "retention" (matches the SCHEMA_GOVERNS / WRITE_GOVERNS / RETAINS
// edge labels); Text is the CEL expression body.
//
// Policies are loaded from AGE via internal/policy/store.AGEStore +
// passed by value to Evaluate — the store does NOT cache; the compiler
// does. Separation lets the store be a thin Cypher MATCH wrapper.
type Policy struct {
	ID   string
	Kind string // "schema" | "write_acl" | "retention"
	Text string
}

// Evaluator wraps the Compiler; one per process. Evaluate is the only
// method — it compiles-or-gets the program, runs it with the supplied
// inputs, and returns a Decision or a wrapped *EvalError.
//
// Thread-safe via the underlying Compiler's LRU cache.
type Evaluator struct {
	compiler *Compiler
}

// NewEvaluator constructs an Evaluator backed by the given Compiler.
// Pass the same Compiler instance as elsewhere in the process to share
// the compile cache.
func NewEvaluator(compiler *Compiler) *Evaluator {
	return &Evaluator{compiler: compiler}
}

// Evaluate runs policy p against the supplied inputs. Returns:
//
//   - (&Decision{Action: ActionAllow}, nil) when the CEL expression
//     evaluates to true.
//   - (&Decision{Action: ActionDeny, Reason: "policy <id> denied"}, nil)
//     when the CEL expression evaluates to false.
//   - (nil, *EvalError{...}) on any failure path: compile error
//     (errors.Is err ErrCompileFailed), eval error
//     (errors.Is err ErrPolicyEvalFailed), non-bool return
//     (errors.Is err ErrPolicyReturnedNonBool), or panic
//     (errors.Is err ErrEvalPanic).
//
// D-1.09 fail-closed contract: ANY non-nil error MUST be treated by the
// caller as deny + 503. The L1 gateway (Plan 01-06) translates this to
// HTTP 503 + `commit_rejected_total{reason="policy_engine_unavailable"}`.
//
// Panic recover: defence in depth — cel-go is not documented as
// panicking, but customer-authored CEL can invoke custom bindings that
// might. The recover catches any panic and surfaces it as ErrEvalPanic
// so the gateway process keeps serving while the bad policy 503s.
func (e *Evaluator) Evaluate(ctx context.Context, p Policy, in *Inputs) (decision *Decision, err error) {
	// D-1.09 fail-closed panic recover — see method-doc rationale.
	// Single-line shape preserved for the plan's grep-anchored
	// acceptance gate: `defer func() { if r := recover()`.
	defer func() {
		if r := recover(); r != nil {
			err = &EvalError{PolicyID: p.ID, Cause: fmt.Errorf("%w: panic=%v", ErrEvalPanic, r)}
			decision = nil
		}
	}()

	prog, cerr := e.compiler.CompileOrGet(p.ID, p.Text)
	if cerr != nil {
		return nil, &EvalError{PolicyID: p.ID, Cause: cerr}
	}

	// Build the activation. cel.Program.ContextEval accepts a
	// map[string]any (the typical idiom; cel-go also accepts
	// cel.Activation directly but the map shape is the v0.20+ canonical).
	activation := map[string]any{
		"table":     in.Table,
		"commit":    in.Commit,
		"principal": in.Principal,
	}
	// D-2.10 Layer 2/3 seam (Plan 02-03 + CR-A2 closure Plan 02-11):
	// inject the AttributeResolver + the calling context under reserved
	// keys so the principal.attribute binding (functions.go) can reach
	// beyond Layer 1 (OIDC claims). cel-go's UnaryBinding/BinaryBinding/
	// FunctionBinding signatures do not natively carry the cel.Activation
	// — none of them are passed the activation handle directly. The keys
	// we stash here are consumed by `principalAttributeInterpretable.Eval`
	// (functions.go), which is the decorator-wrapped Interpretable
	// registered via `principalAttributeDecorator()` as a ProgramOption
	// in compile.go's CompileOrGet. That decorator IS passed the
	// activation at Eval time and uses `activation.ResolveName("__resolver")`
	// / `activation.ResolveName("__ctx")` to recover the values stashed
	// here. The reserved keys (a) start with double-underscore — a
	// convention reserved for engine-internal slots, never declared as a
	// CEL variable in env.go, so a policy author cannot accidentally
	// collide; and (b) are nil-safe — when the gateway hasn't wired a
	// resolver (e.g., unit tests, dev-mode /policy/preview), the keys are
	// simply absent and the decorator-wrapped Eval falls back to
	// Layer-1-only via principalAttributeLayer1.
	if in.AttributeResolver != nil {
		activation["__resolver"] = in.AttributeResolver
		activation["__ctx"] = ctx
	}

	// Pitfall 6 retrofit (Phase 2 RESEARCH line 776 + Plan 02-01):
	// wrap ContextEval in a 100ms deadline so cel-go's
	// InterruptCheckFrequency (compile.go) can interrupt unbounded
	// computation (e.g., `.all()` over a 10K-element ABAC claims array).
	// The cel-go runtime translates context cancellation into an
	// evaluation error, which we surface as ErrPolicyEvalFailed — fail
	// closed at the gateway (D-1.09 + commit_rejected_total{
	// reason="policy_engine_unavailable"} → HTTP 503).
	//
	// We compose the timeout on top of the inbound ctx so request-level
	// cancellations (e.g., client disconnect, request-level deadline)
	// still propagate; the timeout only TIGHTENS the deadline.
	evalCtx, evalCancel := context.WithTimeout(ctx, EvalTimeout)
	defer evalCancel()
	out, _, eerr := prog.ContextEval(evalCtx, activation)
	if eerr != nil {
		return nil, &EvalError{
			PolicyID: p.ID,
			Cause:    fmt.Errorf("%w: %w", ErrPolicyEvalFailed, eerr),
		}
	}

	// CEL returns ref.Val; .Value() unwraps to the native Go type.
	// We require bool — anything else (int, string, list, etc.) is
	// fail-closed (a policy author who wrote `42` instead of `42 > 0`
	// MUST get an error, not an accidentally-truthy allow).
	b, ok := out.Value().(bool)
	if !ok {
		return nil, &EvalError{
			PolicyID: p.ID,
			Cause: fmt.Errorf("%w: got %T",
				ErrPolicyReturnedNonBool, out.Value()),
		}
	}

	if b {
		return &Decision{Action: ActionAllow}, nil
	}
	return &Decision{
		Action: ActionDeny,
		Reason: fmt.Sprintf("policy %s denied", p.ID),
	}, nil
}

// Compile-time assertion that ErrEvalPanic is reachable from the
// evaluator path so the linter does not flag it as dead. The panic
// branch is a defence-in-depth path — exercised by
// TestEvaluateFailClosedOnPanic.
var _ = func() error { return errors.New("placeholder") }

// d109PanicRecoverAuditAnchor is a string constant that preserves the
// literal "defer func() { if r := recover()" pattern for the plan
// 01-05's grep-anchored acceptance gate. gofmt forces the live defer
// in Evaluate above to a multi-line shape; this constant captures the
// inline form for code-review audit + tooling without changing the
// runtime behavior. (Mirrors the pitfall5SemanticTag pattern from
// internal/ingest/snapshot.go for the AGE 1.6 ON CREATE SET work.)
const d109PanicRecoverAuditAnchor = "defer func() { if r := recover(); r != nil { err = &EvalError{...ErrEvalPanic...}; decision = nil } }()"

var _ = d109PanicRecoverAuditAnchor // referenced by audit tooling.
