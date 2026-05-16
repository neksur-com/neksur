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
	"regexp"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
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
		// D-2.10 ABAC fetch. Layer 1 (OIDC claims via
		// principal["claims"]) is honoured by this binding in
		// isolation; Layers 2 (graph HAS_ATTRIBUTE) + 3
		// (tenant_default_attributes) become reachable when Plan
		// 02-04 wires the AttributeResolver via Inputs and enriches
		// the principal map. Returns "" (never errors) when the
		// attribute is unset across all reachable layers — Pitfall 8
		// null-safety.
		cel.Function("principal.attribute",
			cel.Overload("principal_attribute_map_string",
				[]*cel.Type{
					cel.MapType(cel.StringType, cel.DynType),
					cel.StringType,
				},
				cel.StringType,
				cel.BinaryBinding(principalAttributeImpl),
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

// principalAttributeImpl is the D-2.10 ABAC fetch entry-point. In
// THIS plan (02-03), the binding honours **Layer 1** only — OIDC
// claims that the gateway has already inlined into
// `principal["claims"]` (a map[string]any keyed by claim name). When
// Plan 02-04 lands the gateway-side wiring + the cel.Activation
// enrichment that surfaces the AttributeResolver under
// activation["__resolver"], that wiring will be threaded into this
// binding (via an activation-aware variant — cel-go ProgramOption
// `cel.CustomDecorator` or a closure-binding) to make Layers 2+3
// reachable. Until then, callers that need the full 3-layer fetch
// MUST use `(*store.AttributeResolver).Resolve` directly from the
// gateway projection logic.
//
// Returns "" (never errors) on any miss — D-2.10 Pitfall 8
// null-safety: policy authors compare
// `principal.attribute(principal, "region") == ""` to detect
// "attribute is unset across every layer".
func principalAttributeImpl(principalVal, nameVal ref.Val) ref.Val {
	pMap, ok := principalVal.(traits.Mapper)
	if !ok {
		return types.String("")
	}
	nameStr, ok := nameVal.Value().(string)
	if !ok {
		return types.String("")
	}

	// Layer 1: OIDC claims under principal["claims"]. The gateway
	// (Plan 02-04) is responsible for projecting the principal's
	// OIDC claims onto this sub-map BEFORE calling Evaluate.
	claimsVal, found := pMap.Find(types.String("claims"))
	if !found {
		return types.String("")
	}
	claims, ok := claimsVal.Value().(map[string]any)
	if !ok {
		return types.String("")
	}
	v, ok := claims[nameStr]
	if !ok {
		return types.String("")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return types.String("")
	}
	return types.String(s)
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
