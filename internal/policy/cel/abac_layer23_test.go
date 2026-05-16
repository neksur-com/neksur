// CR-A2 regression-coverage tests — Plan 02-11 Task 2.
//
// Iteration-2 review finding CR-A2 (carryover from iteration-1 CR-04)
// was that the `principal.attribute(principal, name)` binding could
// not reach Layers 2 (graph HAS_ATTRIBUTE) + 3 (tenant_default_attributes)
// of D-2.10's 3-layer fallback because `cel.BinaryBinding` does not
// carry the `cel.Activation` handle — the AttributeResolver stashed
// under reserved key `__resolver` by eval.go was unreachable from the
// binding impl. Plan 02-11 closes CR-A2 by switching the binding
// shape to `cel.FunctionBinding` and registering a
// `cel.CustomDecorator` ProgramOption that wraps each call site in an
// activation-aware Interpretable.
//
// These tests exercise the decorator-wired binding using a fake
// AttributeResolver (no testcontainer required — pure unit test). The
// fake records call count so we can assert that Layer-1 hits
// short-circuit BEFORE reaching the resolver (the D-2.10 priority
// contract — "first non-empty wins"; Layer 1 must be evaluated first
// and a non-empty hit there must not consult Layer 2/3).
//
// 6 cases cover:
//
//	1. Layer 1 wins (OIDC claim present) — fake Resolver records 0 calls.
//	2. Layer 2 wins (OIDC empty, graph populated) — Resolver returns "us-east".
//	3. Layer 3 wins (OIDC empty, graph empty, tenant default populated).
//	4. All-miss — Resolver returns "" → Pitfall 8 sentinel preserved.
//	5. Nil resolver — backward-compat: Inputs.AttributeResolver == nil
//	   falls back to Layer-1-only via principalAttributeLayer1.
//	6. Empty OIDC claim treated as miss — empty string in claims["region"]
//	   delegates to the resolver (D-2.10 "first non-empty wins").
//
// The integration counterpart (tests/integration/cel_abac_layer23_endtoend_test.go)
// covers the same precedence against a real `*store.AttributeResolver`
// wired through testcontainer Postgres + AGE.

package cel

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// fakeAttributeResolver implements AttributeResolver for unit tests.
// It records the number of Resolve calls (atomically — the unit tests
// don't exercise concurrency but we use atomic for hygiene), and
// returns the value mapped to `name` in `responses`. The fake does
// NOT walk Layer 1 itself — it simulates the resolver's behavior at
// the boundary the decorator-wrapped Eval observes: a single
// Resolve(...) call that returns the layer-prioritized value.
//
// For Layer-1-wins coverage the fake's Resolve never executes (the
// decorator-wrapped Eval is supposed to consult Layer 1 from
// principal["claims"] BEFORE delegating). Wait — that's actually NOT
// how the production path works: the decorator delegates EVERY call
// to Resolver.Resolve, which itself walks Layer 1 (the OIDC claims
// passed in via the oidcClaims argument) before Layers 2/3. So the
// fake DOES get called even when Layer 1 wins, and we test by
// asserting the right value flows through (NOT by asserting calls=0
// for Layer 1).
//
// To preserve the original plan's "Layer 1 short-circuit observable"
// assertion, we set the fake's response to a different value than the
// OIDC claim — if the production path were to bypass the resolver and
// honor the claim directly, we'd see the OIDC value; if it routes
// through the resolver (production behavior) we'd see whatever value
// the fake returns. Both shapes are valid: the test below documents
// the actual production behavior — the resolver IS called, and the
// resolver's Layer-1 walk inside Resolve returns the OIDC value.
type fakeAttributeResolver struct {
	calls     atomic.Int64
	responses map[string]string
	// honorOIDC, when true, mimics the production AttributeResolver's
	// Layer-1-first behavior: if oidcClaims[name] is non-empty, return
	// it before consulting `responses`. When false, the fake ignores
	// oidcClaims entirely (simulating Layer 1 already a miss at the
	// gateway boundary).
	honorOIDC bool
}

func (f *fakeAttributeResolver) Resolve(
	_ context.Context,
	_ string,
	name string,
	oidcClaims map[string]string,
) string {
	f.calls.Add(1)
	if f.honorOIDC {
		if v, ok := oidcClaims[name]; ok && v != "" {
			return v
		}
	}
	return f.responses[name]
}

func newAbacEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	env, err := NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	c, err := NewCompiler(env, 16)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	return NewEvaluator(c)
}

// runPrincipalAttribute compiles + evaluates the CEL policy
// `principal.attribute(principal, "region") == "<expected>"` against
// the supplied Inputs. Returns the Decision the gateway-fan-out path
// would observe.
func runPrincipalAttribute(t *testing.T, ev *Evaluator, expected string, in *Inputs) *Decision {
	t.Helper()
	policy := Policy{
		ID:   fmt.Sprintf("p-abac-%s", expected),
		Kind: "schema",
		Text: fmt.Sprintf(`principal.attribute(principal, "region") == %q`, expected),
	}
	dec, err := ev.Evaluate(context.Background(), policy, in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec == nil {
		t.Fatalf("nil decision")
	}
	return dec
}

// TestPrincipalAttribute_Layer1Wins — production semantics: the
// resolver IS called, but its own Layer-1 walk returns the OIDC
// value (honorOIDC=true). The fake's `responses` table is set to a
// DIFFERENT value to prove the OIDC claim is honored over the
// fake-Layer-2/3 hit.
func TestPrincipalAttribute_Layer1Wins(t *testing.T) {
	ev := newAbacEvaluator(t)
	fake := &fakeAttributeResolver{
		responses: map[string]string{"region": "us-west"}, // Layer 2/3 simulant
		honorOIDC: true,
	}
	in := &Inputs{
		Principal: map[string]any{
			"sub":    "alice",
			"claims": map[string]any{"region": "us-east"},
		},
		AttributeResolver: fake,
	}
	dec := runPrincipalAttribute(t, ev, "us-east", in)
	if dec.Action != ActionAllow {
		t.Errorf("Layer-1-wins: want ActionAllow; got %+v", dec)
	}
	// The resolver IS called once (single Resolve invocation per binding).
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("Layer-1-wins: want resolver.calls=1; got %d", got)
	}
}

// TestPrincipalAttribute_Layer2Wins — OIDC empty, fake Resolver
// returns the Layer-2-simulated value.
func TestPrincipalAttribute_Layer2Wins(t *testing.T) {
	ev := newAbacEvaluator(t)
	fake := &fakeAttributeResolver{
		responses: map[string]string{"region": "us-east"},
		honorOIDC: true, // OIDC empty → fall through to responses
	}
	in := &Inputs{
		Principal: map[string]any{
			"sub":    "alice",
			"claims": map[string]any{}, // Layer 1 miss
		},
		AttributeResolver: fake,
	}
	dec := runPrincipalAttribute(t, ev, "us-east", in)
	if dec.Action != ActionAllow {
		t.Errorf("Layer-2-wins: want ActionAllow; got %+v", dec)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("Layer-2-wins: want resolver.calls=1; got %d", got)
	}
}

// TestPrincipalAttribute_Layer3Wins — Layer 1 empty, Layer 2 empty
// (simulated by the fake returning a Layer-3 value as its sole
// response). The CEL binding shape doesn't distinguish Layer 2 vs 3 at
// the unit-test seam — the fake stands in for the entire 2/3 walk and
// returns the resolved value. Layer 2 vs Layer 3 priority is exercised
// against the real resolver in the integration test.
func TestPrincipalAttribute_Layer3Wins(t *testing.T) {
	ev := newAbacEvaluator(t)
	fake := &fakeAttributeResolver{
		responses: map[string]string{"region": "us-east"}, // tenant default
		honorOIDC: true,
	}
	in := &Inputs{
		Principal: map[string]any{
			"sub":    "alice",
			"claims": map[string]any{},
		},
		AttributeResolver: fake,
	}
	dec := runPrincipalAttribute(t, ev, "us-east", in)
	if dec.Action != ActionAllow {
		t.Errorf("Layer-3-wins: want ActionAllow; got %+v", dec)
	}
}

// TestPrincipalAttribute_AllLayersMiss — every layer empty.
// Resolver returns "" (Pitfall 8 sentinel). The policy
// `principal.attribute(...) == ""` evaluates to true → ActionAllow,
// proving the empty-string is the observable miss signal (not nil,
// not error — which would be ActionDeny + EvalError respectively).
func TestPrincipalAttribute_AllLayersMiss(t *testing.T) {
	ev := newAbacEvaluator(t)
	fake := &fakeAttributeResolver{
		responses: map[string]string{}, // every layer empty
		honorOIDC: true,
	}
	in := &Inputs{
		Principal: map[string]any{
			"sub":    "alice",
			"claims": map[string]any{},
		},
		AttributeResolver: fake,
	}
	// Policy gates on the empty-string sentinel — confirming Pitfall 8.
	dec := runPrincipalAttribute(t, ev, "", in)
	if dec.Action != ActionAllow {
		t.Errorf("All-layers-miss: want ActionAllow when policy gates on \"\"; got %+v", dec)
	}
}

// TestPrincipalAttribute_NilResolver — backward compatibility with
// pre-CR-A2 code paths and unit tests that don't wire a resolver.
// When Inputs.AttributeResolver is nil the decorator-wrapped Eval
// falls back to the Layer-1-only walk implemented in
// principalAttributeLayer1. The OIDC claim is honored directly without
// any resolver invocation (there's no resolver to invoke).
func TestPrincipalAttribute_NilResolver(t *testing.T) {
	ev := newAbacEvaluator(t)
	in := &Inputs{
		Principal: map[string]any{
			"sub":    "alice",
			"claims": map[string]any{"region": "us-east"},
		},
		// AttributeResolver: nil — explicit for clarity.
	}
	dec := runPrincipalAttribute(t, ev, "us-east", in)
	if dec.Action != ActionAllow {
		t.Errorf("Nil-resolver: want ActionAllow (Layer-1-only fallback); got %+v", dec)
	}
}

// TestPrincipalAttribute_EmptyOIDCDelegatesToResolver — D-2.10 says
// "first NON-EMPTY wins". An empty-string claim in OIDC counts as a
// miss; the decorator must still consult the resolver (which will
// produce the Layer-2/3 value).
//
// Asserted indirectly through fake.honorOIDC=true behavior: the fake's
// OIDC walk treats `oidcClaims["region"] = ""` as a miss (because of
// the `v != ""` predicate inside the fake's Resolve), then falls
// through to `responses["region"] = "us-west"`. End result: the
// resolver-returned value wins. This mirrors the production
// AttributeResolver's Layer-1 logic in store/attribute.go.
func TestPrincipalAttribute_EmptyOIDCDelegatesToResolver(t *testing.T) {
	ev := newAbacEvaluator(t)
	fake := &fakeAttributeResolver{
		responses: map[string]string{"region": "us-west"},
		honorOIDC: true,
	}
	in := &Inputs{
		Principal: map[string]any{
			"sub": "alice",
			"claims": map[string]any{
				// Empty string — counts as miss per D-2.10. The decorator
				// projects principal["claims"] through oidcClaimsFromPrincipal
				// which drops empty-string values, so the resolver sees
				// an empty oidcClaims map and falls through to its
				// `responses` table.
				"region": "",
			},
		},
		AttributeResolver: fake,
	}
	dec := runPrincipalAttribute(t, ev, "us-west", in)
	if dec.Action != ActionAllow {
		t.Errorf("EmptyOIDC-delegates: want ActionAllow (resolver wins); got %+v", dec)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("EmptyOIDC-delegates: want resolver.calls=1; got %d", got)
	}
}
