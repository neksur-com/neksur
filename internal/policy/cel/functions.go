// Custom CEL function bindings — Pitfall 7 mitigation.
//
// CEL has NO built-in JSONPath / structural-walk operator (per RESEARCH
// §Pitfalls 7 lines 1485-1493). Without these custom bindings, a P1
// policy like "the new schema must NOT introduce a column named ssn"
// cannot be expressed at all (the policy author would need to write
// imperative iteration, which CEL doesn't have either).
//
// Three bindings:
//
//   - manifest.has_column(table, name string) bool
//     True if `table.schema.fields[*].name` contains `name`.
//
//   - manifest.has_partition(table, spec string) bool
//     True if `table.partition_spec.fields[*].name` contains `spec`.
//
//   - principal.role(principal, role string) bool
//     True if `principal.roles` (a string slice) contains `role`.
//
// All three are BINARY (take the table/principal map as the FIRST arg,
// the lookup string as the SECOND arg). Policy authors invoke them as
// `manifest.has_column(table, "ssn")`, NOT as method calls — CEL has no
// method-call syntax for custom bindings of this shape.
//
// Implementation note: each binding is a pure function over CEL's ref.Val.
// The activation values come from the per-call Activation passed to
// prog.ContextEval (in eval.go); the bindings receive them as
// already-unwrapped Go maps, so we type-assert + walk + return.
// Errors during the walk return Bool(false) — the binding is fail-safe
// at the data layer (the wider fail-closed contract is enforced at the
// Evaluate boundary; a binding-level panic would also be recovered there).

package cel

import (
	"context"
	"regexp"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/interpreter"
)

// registerManifestFunctions returns the cel.EnvOption slice that wires
// the three custom bindings into the singleton env. Called from env.go's
// NewEnv ONCE per process.
func registerManifestFunctions() []cel.EnvOption {
	return []cel.EnvOption{
		// manifest.has_column(table map, name string) bool
		cel.Function("manifest.has_column",
			cel.Overload("manifest_has_column_map_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
					cel.StringType,
				},
				cel.BoolType,
				cel.BinaryBinding(hasColumnImpl),
			),
		),
		// manifest.has_partition(table map, spec string) bool
		cel.Function("manifest.has_partition",
			cel.Overload("manifest_has_partition_map_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
					cel.StringType,
				},
				cel.BoolType,
				cel.BinaryBinding(hasPartitionImpl),
			),
		),
		// principal.role(principal map, role string) bool
		cel.Function("principal.role",
			cel.Overload("principal_role_map_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
					cel.StringType,
				},
				cel.BoolType,
				cel.BinaryBinding(principalRoleImpl),
			),
		),
		// Phase 2 — Plan 02-03: 4 new bindings (P4/P5/P7/ABAC).

		// location.region(commit map) string
		// Reads commit-projection key "location_region"; Plan 02-04
		// wires the gateway to inline it from the X-Neksur-Region
		// header (or similar IP-geolocation projection).
		cel.Function("location.region",
			cel.Overload("location_region_map_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
				},
				cel.StringType,
				cel.UnaryBinding(locationRegionImpl),
			),
		),
		// manifest.classification_satisfied(table map, columnPattern string, requiredTag string) bool
		// True iff every column whose name matches `columnPattern`
		// (Go regexp) has a classification entry with tag
		// `requiredTag`. P4 (residency) and P5 (purpose limitation)
		// invoke this to assert e.g. "every column matching ^pii_.*
		// is tagged sensitive".
		cel.Function("manifest.classification_satisfied",
			cel.Overload("manifest_classification_satisfied_map_string_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
					cel.StringType,
					cel.StringType,
				},
				cel.BoolType,
				cel.FunctionBinding(classificationSatisfiedImpl),
			),
		),
		// manifest.partition_spec(table map) map<string,string>
		// Exposes the partition spec as a flat string→string map for
		// P7 (data-locality) policies that read e.g.
		// `manifest.partition_spec(table)["region"] == "us-east-1"`.
		cel.Function("manifest.partition_spec",
			cel.Overload("manifest_partition_spec_map",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
				},
				cel.MapType(cel.StringType, cel.StringType),
				cel.UnaryBinding(partitionSpecImpl),
			),
		),
		// principal.attribute(principal map, name string) string
		// D-2.10 ABAC fetch — 3-layer fallback (OIDC claims → graph
		// HAS_ATTRIBUTE → tenant_default_attributes). Layer 1 is
		// honoured here in isolation; Layers 2+3 are reached via the
		// principalAttributeDecorator() ProgramOption registered in
		// env.go, which wraps every call site with an activation-aware
		// Interpretable that pulls the AttributeResolver + context.Context
		// from activation["__resolver"] / activation["__ctx"]. When the
		// resolver is absent (e.g., unit tests with no Inputs.AttributeResolver),
		// the decorator-wrapped Eval falls back to the Layer-1-only path
		// implemented in principalAttributeFuncImpl — preserves backward
		// compatibility with the Plan 02-03 test suite.
		//
		// CR-A2 fix (Plan 02-11): the binding shape is `FunctionBinding`
		// (variadic-args) instead of `BinaryBinding` so the call site is
		// dispatched through the same code path as
		// `manifest.classification_satisfied` — required so the decorator's
		// `InterpretableCall.Function()` check picks it up consistently.
		cel.Function("principal.attribute",
			cel.Overload("principal_attribute_map_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
					cel.StringType,
				},
				cel.StringType,
				cel.FunctionBinding(principalAttributeFuncImpl),
			),
		),
	}
}

// locationRegionImpl reads the commit projection's "location_region"
// slot and returns it as a CEL string. Returns "" on any miss / type
// mismatch — P4/P7 authors compare to the empty string to detect
// "header not present" (consistent with D-2.10 Pitfall 8 sentinel).
//
// Activation shape (from eval.go's Inputs.Commit, populated by the
// gateway):
//
//	{
//	  "location_region": "us-east-1",
//	  "requirements": [...],
//	  "updates": [...],
//	  ...
//	}
func locationRegionImpl(commitVal ref.Val) ref.Val {
	m, ok := commitVal.(traits.Mapper)
	if !ok {
		return types.String("")
	}
	v, found := m.Find(types.String("location_region"))
	if !found || v == nil {
		return types.String("")
	}
	s, ok := v.Value().(string)
	if !ok {
		return types.String("")
	}
	return types.String(s)
}

// classificationSatisfiedImpl checks that every column in
// `table["columns"]` whose name matches the supplied regexp pattern
// carries a classification entry in `table["classifications"]` tagged
// with `requiredTag`. A column with NO classification entry counts as
// a miss (returns Bool(false)) — the policy contract is "the
// classification MUST be present", not "if present, must match".
//
// Activation shape (gateway projection, Plan 02-04 owns wiring):
//
//	table.columns = [
//	    {"name": "pii_ssn", "type": "string", ...},
//	    {"name": "purchase_id", "type": "long", ...},
//	    ...
//	]
//	table.classifications = [
//	    {"column_name": "pii_ssn", "tag": "sensitive"},
//	    {"column_name": "pii_ssn", "tag": "encrypted"},
//	    ...
//	]
//
// Returns Bool(false) on any type mismatch or unparseable regexp —
// fail-safe at the binding layer (the wider fail-closed contract
// applies at Evaluate).
func classificationSatisfiedImpl(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.Bool(false)
	}
	table, ok := args[0].(traits.Mapper)
	if !ok || table == nil {
		return types.Bool(false)
	}
	pattern, ok := args[1].Value().(string)
	if !ok {
		return types.Bool(false)
	}
	requiredTag, ok := args[2].Value().(string)
	if !ok {
		return types.Bool(false)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return types.Bool(false)
	}

	// Index classifications: column_name → set<tag>.
	clsVal, found := table.Find(types.String("classifications"))
	if !found {
		return types.Bool(false)
	}
	clsList, ok := clsVal.(traits.Lister)
	if !ok {
		return types.Bool(false)
	}
	tagsByCol := map[string]map[string]bool{}
	clsIter := clsList.Iterator()
	for clsIter.HasNext() == types.True {
		entry, ok := clsIter.Next().(traits.Mapper)
		if !ok || entry == nil {
			continue
		}
		colVal, ok := entry.Find(types.String("column_name"))
		if !ok {
			continue
		}
		tagVal, ok := entry.Find(types.String("tag"))
		if !ok {
			continue
		}
		col, ok := colVal.Value().(string)
		if !ok {
			continue
		}
		tag, ok := tagVal.Value().(string)
		if !ok {
			continue
		}
		if tagsByCol[col] == nil {
			tagsByCol[col] = map[string]bool{}
		}
		tagsByCol[col][tag] = true
	}

	// Walk columns; every regex-matching column MUST carry requiredTag.
	colsVal, found := table.Find(types.String("columns"))
	if !found {
		return types.Bool(false)
	}
	colsList, ok := colsVal.(traits.Lister)
	if !ok {
		return types.Bool(false)
	}
	colsIter := colsList.Iterator()
	for colsIter.HasNext() == types.True {
		entry, ok := colsIter.Next().(traits.Mapper)
		if !ok || entry == nil {
			continue
		}
		nameVal, ok := entry.Find(types.String("name"))
		if !ok {
			continue
		}
		name, ok := nameVal.Value().(string)
		if !ok {
			continue
		}
		if !re.MatchString(name) {
			continue
		}
		if !tagsByCol[name][requiredTag] {
			return types.Bool(false)
		}
	}
	return types.Bool(true)
}

// partitionSpecImpl projects `table["partition_spec"]` (a
// map[string]any) into a flat map<string,string> suitable for direct
// indexing inside CEL: `manifest.partition_spec(table)["region"]`.
// Non-string values are silently dropped (the policy author should
// only address string-typed partition fields by name; numeric
// transforms are out of scope here).
//
// Returns an empty map on any type mismatch — index lookup against
// an empty map in CEL yields the type-default empty string, which
// composes safely with the "" sentinel pattern used elsewhere.
func partitionSpecImpl(tableVal ref.Val) ref.Val {
	empty := types.NewStringStringMap(types.DefaultTypeAdapter, map[string]string{})
	m, ok := tableVal.(traits.Mapper)
	if !ok {
		return empty
	}
	spec, found := m.Find(types.String("partition_spec"))
	if !found {
		return empty
	}
	specMap, ok := spec.Value().(map[string]any)
	if !ok {
		return empty
	}
	out := make(map[string]string, len(specMap))
	for k, v := range specMap {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return types.NewStringStringMap(types.DefaultTypeAdapter, out)
}

// principalAttributeFuncImpl is the Layer-1-only fallback path used by
// the decorator (principalAttributeDecorator) when the activation has
// no AttributeResolver stashed under "__resolver" — e.g., unit tests
// that construct an Evaluator without setting Inputs.AttributeResolver,
// or pre-tenant request paths (the dev-mode /policy/preview endpoint).
// It walks `principal["claims"][name]` and returns the value as a CEL
// string, or "" on any miss / type mismatch.
//
// In the production path (Inputs.AttributeResolver != nil), this
// function is NEVER called directly by the CEL runtime — the decorator
// intercepts the call site and routes to AttributeResolver.Resolve,
// which itself walks Layer 1 → 2 → 3 (D-2.10 contract). This function
// remains registered as the binding's FunctionBinding because cel-go
// requires every Overload to declare an executable binding for the
// type-checker; the decorator wraps but does not replace the binding
// at the EnvOption layer.
//
// Returns "" (never errors) on any miss — D-2.10 Pitfall 8 null-safety:
// policy authors compare `principal.attribute(principal, "region") == ""`
// to detect "attribute is unset across every layer". The wider
// fail-closed contract at the Evaluate boundary (panic-recover) catches
// any binding-level panic.
//
// CR-A2 fix (Plan 02-11): the signature changed from
// `func(principalVal, nameVal ref.Val) ref.Val` (BinaryBinding) to
// `func(args ...ref.Val) ref.Val` (FunctionBinding) so the call site
// dispatches through the same Interpretable shape as
// `manifest.classification_satisfied`. The Interpretable shape is the
// hook the CustomDecorator wraps.
func principalAttributeFuncImpl(args ...ref.Val) ref.Val {
	if len(args) != 2 {
		return types.String("")
	}
	pMap, ok := args[0].(traits.Mapper)
	if !ok {
		return types.String("")
	}
	nameStr, ok := args[1].Value().(string)
	if !ok {
		return types.String("")
	}
	return principalAttributeLayer1(pMap, nameStr)
}

// principalAttributeLayer1 implements the pure Layer-1 OIDC-claim walk
// extracted as a function so both principalAttributeFuncImpl AND the
// decorator's Resolver-absent fallback path share the same code (no
// drift between the two miss-paths).
func principalAttributeLayer1(pMap traits.Mapper, name string) ref.Val {
	// Layer 1: OIDC claims under principal["claims"]. The gateway
	// projects the principal's OIDC claims onto this sub-map BEFORE
	// calling Evaluate.
	claimsVal, found := pMap.Find(types.String("claims"))
	if !found {
		return types.String("")
	}
	claims, ok := claimsVal.Value().(map[string]any)
	if !ok {
		return types.String("")
	}
	v, ok := claims[name]
	if !ok {
		return types.String("")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return types.String("")
	}
	return types.String(s)
}

// principalAttributeFunctionName is the textual function identifier
// the CEL parser produces for `principal.attribute(principal, name)`
// call sites. The decorator matches against this string when filtering
// InterpretableCall nodes — a small constant kept colocated with the
// binding registration so the matcher cannot drift independently of
// the cel.Function() name.
const principalAttributeFunctionName = "principal.attribute"

// principalAttributeDecorator returns a cel.ProgramOption that wraps
// every InterpretableCall to `principal.attribute` with an
// activation-aware Eval. At program-construction time the decorator
// receives the Interpretable instructions; for `principal.attribute`
// it returns a wrapper whose Eval(activation) (a) reads the
// AttributeResolver + context.Context from activation under reserved
// keys `__resolver` / `__ctx`, (b) evaluates the call's two argument
// Interpretables against the activation to obtain the live principal
// map + name, (c) extracts the principal `sub` + OIDC claims map for
// the resolver call, (d) invokes Resolver.Resolve which itself walks
// Layer 1 → 2 → 3 and returns the first non-empty value (or "" on
// all-miss — Pitfall 8). When the resolver is absent the wrapper
// falls back to Layer-1-only via principalAttributeLayer1.
//
// CR-A2 closure (Plan 02-11 / iteration-1 CR-04): before this
// decorator the binding registered via cel.BinaryBinding could not
// reach the activation — `cel.BinaryBinding(impl)` calls `impl(lhs, rhs ref.Val)`
// with the two arg values but no Activation handle. cel-go does not
// surface the Activation through FunctionBinding either (only the
// args). The activation handle is reachable only by intercepting at
// the Interpretable layer — which is precisely what the
// CustomDecorator hook is designed for. See cel-go v0.28.1
// interpreter.InterpretableDecorator.
func principalAttributeDecorator() cel.ProgramOption {
	return cel.CustomDecorator(func(i interpreter.Interpretable) (interpreter.Interpretable, error) {
		call, ok := i.(interpreter.InterpretableCall)
		if !ok {
			return i, nil
		}
		if call.Function() != principalAttributeFunctionName {
			return i, nil
		}
		// Wrap the call. The wrapper preserves ID() + delegates to the
		// original call's args (Interpretables) for argument evaluation;
		// it ONLY overrides Eval to route through the AttributeResolver.
		return &principalAttributeInterpretable{inner: call}, nil
	})
}

// principalAttributeInterpretable wraps the original
// `principal.attribute` InterpretableCall to thread the activation
// through to the AttributeResolver. Its Eval(activation) is the only
// method that diverges from the wrapped node — ID() proxies through
// for trace fidelity.
type principalAttributeInterpretable struct {
	inner interpreter.InterpretableCall
}

// ID returns the original call's expression id so any tracing /
// cost-tracking overlay sees the wrapped node as the same node.
func (w *principalAttributeInterpretable) ID() int64 { return w.inner.ID() }

// Eval is the activation-aware path. Steps:
//
//  1. Resolve the two argument Interpretables against the activation
//     to obtain the live principal map + the attribute name string.
//  2. Look up the AttributeResolver + context.Context from the
//     activation under reserved keys `__resolver` / `__ctx`. If
//     either is absent the wrapper short-circuits to the Layer-1-only
//     fallback (principalAttributeLayer1) so unit tests + dev-mode
//     callers without a resolver still work.
//  3. Project Layer-1 inputs: extract `principal["sub"]` as the
//     principalSub argument and `principal["claims"]` (string→string
//     coerced) as the oidcClaims argument. The resolver itself walks
//     Layer 1 first (so Layer 1 wins over Layer 2+3 per D-2.10), then
//     Layer 2 (graph), then Layer 3 (tenant default).
//  4. Return the resolved value as types.String. All-miss returns ""
//     — Pitfall 8 null-safety sentinel.
//
// Defence in depth: every error path on type assertion / lookup
// returns types.String(""), never panic / never error — the broader
// fail-closed contract at the Evaluate boundary catches anything that
// does panic via defer/recover (D-1.09).
func (w *principalAttributeInterpretable) Eval(activation interpreter.Activation) ref.Val {
	args := w.inner.Args()
	if len(args) != 2 {
		return types.String("")
	}
	principalVal := args[0].Eval(activation)
	nameVal := args[1].Eval(activation)

	pMap, ok := principalVal.(traits.Mapper)
	if !ok {
		return types.String("")
	}
	nameStr, ok := nameVal.Value().(string)
	if !ok {
		return types.String("")
	}

	// Look up resolver — absent → Layer-1-only fallback.
	resolverVal, resolverFound := activation.ResolveName("__resolver")
	if !resolverFound || resolverVal == nil {
		return principalAttributeLayer1(pMap, nameStr)
	}
	resolver, ok := resolverVal.(AttributeResolver)
	if !ok {
		return principalAttributeLayer1(pMap, nameStr)
	}
	// Look up ctx — required for the resolver's Layer 2 / Layer 3 calls
	// (they pull tenant.IDFromContext + run pgx queries). Absent →
	// degrade to Layer-1-only rather than risk a nil-context panic.
	ctxVal, ctxFound := activation.ResolveName("__ctx")
	if !ctxFound || ctxVal == nil {
		return principalAttributeLayer1(pMap, nameStr)
	}
	ctx, ok := ctxVal.(context.Context)
	if !ok {
		return principalAttributeLayer1(pMap, nameStr)
	}

	// Project the principal sub + OIDC claims.
	principalSub := principalSubFromMap(pMap)
	oidcClaims := oidcClaimsFromPrincipal(pMap)

	// Resolver.Resolve walks Layer 1 → 2 → 3 per D-2.10. All-miss
	// returns "" (Pitfall 8). Errors at Layer 2/3 are swallowed by
	// the resolver (fail-soft on transient backend faults — see
	// store/attribute.go doc).
	v := resolver.Resolve(ctx, principalSub, nameStr, oidcClaims)
	return types.String(v)
}

// principalSubFromMap extracts principal["sub"] as a string for the
// resolver's principalSub argument. Returns "" if the key is absent
// or the value is not a string — the resolver tolerates this (Layer 2
// match with `sub: ''` finds nothing, and Layer 3 doesn't need the
// sub at all).
func principalSubFromMap(pMap traits.Mapper) string {
	v, ok := pMap.Find(types.String("sub"))
	if !ok || v == nil {
		return ""
	}
	s, ok := v.Value().(string)
	if !ok {
		return ""
	}
	return s
}

// oidcClaimsFromPrincipal extracts principal["claims"] and coerces it
// to map[string]string for the resolver's oidcClaims argument. The
// resolver only treats non-empty string values as Layer-1 hits, so
// non-string values are silently dropped — they couldn't have produced
// a Layer-1 winner anyway.
//
// Returns an empty (but non-nil) map on any miss / type mismatch so
// the resolver's Layer-1 check (`if v, ok := oidcClaims[name]; ok && v != ""`)
// always operates on a valid map (no nil-map panic risk).
func oidcClaimsFromPrincipal(pMap traits.Mapper) map[string]string {
	out := map[string]string{}
	claimsVal, ok := pMap.Find(types.String("claims"))
	if !ok || claimsVal == nil {
		return out
	}
	claims, ok := claimsVal.Value().(map[string]any)
	if !ok {
		return out
	}
	for k, v := range claims {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	return out
}

// hasColumnImpl walks `table.schema.fields[*].name` and returns Bool(true)
// if any field's name matches the given column name. Pitfall 7 mitigation:
// gives policy authors a typed alternative to the JSONPath they don't have.
//
// Activation shape (from eval.go's Inputs.Table):
//
//	{
//	  "schema": {
//	    "fields": [
//	      {"name": "id", "type": "long", ...},
//	      {"name": "email", "type": "string", ...},
//	      ...
//	    ]
//	  },
//	  ...
//	}
//
// Returns Bool(false) on any type mismatch in the walk (the binding is
// fail-safe; the wider fail-closed contract is enforced at Evaluate).
func hasColumnImpl(tableVal, nameVal ref.Val) ref.Val {
	tbl, ok := tableVal.Value().(map[string]any)
	if !ok {
		return types.Bool(false)
	}
	name, ok := nameVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	schema, ok := tbl["schema"].(map[string]any)
	if !ok {
		return types.Bool(false)
	}
	fields, ok := schema["fields"].([]any)
	if !ok {
		return types.Bool(false)
	}
	for _, f := range fields {
		field, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if fieldName, _ := field["name"].(string); fieldName == name {
			return types.Bool(true)
		}
	}
	return types.Bool(false)
}

// hasPartitionImpl walks `table.partition_spec.fields[*].name` and
// returns Bool(true) if any partition field's name matches the given
// spec name. Same shape + semantics as hasColumnImpl.
//
// Activation shape (from eval.go's Inputs.Table):
//
//	{
//	  "partition_spec": {
//	    "fields": [
//	      {"name": "year", "transform": "years", ...},
//	      ...
//	    ]
//	  },
//	  ...
//	}
func hasPartitionImpl(tableVal, specVal ref.Val) ref.Val {
	tbl, ok := tableVal.Value().(map[string]any)
	if !ok {
		return types.Bool(false)
	}
	spec, ok := specVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	pspec, ok := tbl["partition_spec"].(map[string]any)
	if !ok {
		return types.Bool(false)
	}
	fields, ok := pspec["fields"].([]any)
	if !ok {
		return types.Bool(false)
	}
	for _, f := range fields {
		field, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if fieldName, _ := field["name"].(string); fieldName == spec {
			return types.Bool(true)
		}
	}
	return types.Bool(false)
}

// principalRoleImpl checks whether `principal.roles` (a []any of strings)
// contains the named role. P2 write-ACL policies are typically written as
// `principal.role(principal, "writer") || principal.sub in ['alice','bob']`.
//
// Activation shape (from eval.go's Inputs.Principal):
//
//	{
//	  "sub":   "alice@example.com",
//	  "roles": ["writer", "reader"],
//	  ...
//	}
func principalRoleImpl(principalVal, roleVal ref.Val) ref.Val {
	principal, ok := principalVal.Value().(map[string]any)
	if !ok {
		return types.Bool(false)
	}
	role, ok := roleVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	roles, ok := principal["roles"].([]any)
	if !ok {
		return types.Bool(false)
	}
	for _, r := range roles {
		if rs, _ := r.(string); rs == role {
			return types.Bool(true)
		}
	}
	return types.Bool(false)
}
