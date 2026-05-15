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
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
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
	}
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
