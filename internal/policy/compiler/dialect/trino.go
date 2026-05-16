// Trino dialect emitter — live Phase 2 implementation (D-2.04).
//
// Trino's SQL grammar accepts the portable subset directly: the only
// dialect-specific shaping is in how table qualification works (Trino
// uses catalog.schema.table) and how string literals quote. The
// compiler hands us a pre-validated AST so we type-switch and emit.
//
// Identifiers in the AST have already passed the lexer's
// `^[A-Za-z_][A-Za-z0-9_]*$` filter — no further escaping is needed
// because the allowlist excludes every quoting metacharacter. String
// literals are re-quoted by us with `''` escaping so any embedded
// single-quote round-trips safely through the Trino parser.

package dialect

import (
	"fmt"
	"strings"
)

// astNodeShim is a type-asserted view of the parent compiler
// package's AST nodes. We can't import the parent package (cycle), so
// the cross-engine compiler passes interface{} values that we
// reflect on via struct-shape type assertions through a small adapter
// interface.
//
// In practice the cross-engine compiler converts the AST to a
// dialect-package view (a tiny duplicate type tree) before invoking
// the emitter — see compiler.go's adaptForDialect helper. This keeps
// the dialect package free of upward imports.

// astBinaryOp / astUnaryOp / astColumnRef / astLiteral / astInList /
// astFuncCall / astMaskProject mirror the compiler package's AST
// shapes. Exported so the cross-engine compiler can construct them
// when adapting its native AST to the dialect-facing shape.

type AstBinaryOp struct {
	Op    string
	Left  any
	Right any
}
type AstUnaryOp struct {
	Op      string
	Operand any
}
type AstColumnRef struct {
	Table  string
	Column string
}
type AstLiteral struct {
	Kind  string // "string","number","null"
	Value string
}
type AstInList struct {
	Left   any
	Values []AstLiteral
}
type AstFuncCall struct {
	Name string
	Arg  any
}
type AstMaskProject struct {
	Column string
	Expr   any
}

// TrinoCompiler emits Trino SQL fragments. The zero value is usable;
// construct via NewTrinoCompiler for symmetry with the other emitters.
type TrinoCompiler struct{}

// NewTrinoCompiler returns the Trino dialect emitter.
func NewTrinoCompiler() *TrinoCompiler { return &TrinoCompiler{} }

// Kind returns "trino".
func (TrinoCompiler) Kind() string { return "trino" }

// CompileRowFilter lowers a predicate AST to a Trino WHERE-clause body.
func (c TrinoCompiler) CompileRowFilter(table string, predicate any) (string, error) {
	if predicate == nil {
		return "", fmt.Errorf("trino: nil row-filter predicate")
	}
	var sb strings.Builder
	if err := c.emitPred(&sb, predicate); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// CompileColumnMask lowers a column-mask AST to a Trino SELECT
// projection list.
func (c TrinoCompiler) CompileColumnMask(table string, mask any) (string, error) {
	projects, ok := mask.([]AstMaskProject)
	if !ok {
		return "", fmt.Errorf("trino: column mask is not []AstMaskProject (got %T)", mask)
	}
	if len(projects) == 0 {
		return "", fmt.Errorf("trino: column mask is empty")
	}
	parts := make([]string, 0, len(projects))
	for _, p := range projects {
		var sb strings.Builder
		if err := c.emitPred(&sb, p.Expr); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s AS %s", sb.String(), p.Column))
	}
	return strings.Join(parts, ", "), nil
}

func (c TrinoCompiler) emitPred(sb *strings.Builder, n any) error {
	switch v := n.(type) {
	case AstBinaryOp:
		sb.WriteByte('(')
		if err := c.emitPred(sb, v.Left); err != nil {
			return err
		}
		sb.WriteByte(' ')
		sb.WriteString(v.Op)
		sb.WriteByte(' ')
		if err := c.emitPred(sb, v.Right); err != nil {
			return err
		}
		sb.WriteByte(')')
	case AstUnaryOp:
		sb.WriteByte('(')
		sb.WriteString(v.Op)
		sb.WriteByte(' ')
		if err := c.emitPred(sb, v.Operand); err != nil {
			return err
		}
		sb.WriteByte(')')
	case AstColumnRef:
		if v.Table != "" {
			sb.WriteString(v.Table)
			sb.WriteByte('.')
		}
		sb.WriteString(v.Column)
	case AstLiteral:
		switch v.Kind {
		case "string":
			sb.WriteByte('\'')
			sb.WriteString(strings.ReplaceAll(v.Value, "'", "''"))
			sb.WriteByte('\'')
		case "number":
			sb.WriteString(v.Value)
		case "null":
			sb.WriteString("NULL")
		default:
			return fmt.Errorf("trino: unsupported literal kind %q", v.Kind)
		}
	case AstInList:
		if err := c.emitPred(sb, v.Left); err != nil {
			return err
		}
		sb.WriteString(" IN (")
		for i, lit := range v.Values {
			if i > 0 {
				sb.WriteString(", ")
			}
			if err := c.emitPred(sb, lit); err != nil {
				return err
			}
		}
		sb.WriteByte(')')
	case AstFuncCall:
		// Trino built-ins for the common mask functions; reject any
		// function name the policy author hasn't pre-registered.
		switch v.Name {
		case "hash":
			sb.WriteString("to_hex(sha256(to_utf8(CAST(")
			if err := c.emitPred(sb, v.Arg); err != nil {
				return err
			}
			sb.WriteString(" AS VARCHAR))))")
		case "lower", "upper", "length":
			sb.WriteString(v.Name)
			sb.WriteByte('(')
			if err := c.emitPred(sb, v.Arg); err != nil {
				return err
			}
			sb.WriteByte(')')
		default:
			return fmt.Errorf("trino: function %q not whitelisted", v.Name)
		}
	default:
		return fmt.Errorf("trino: unsupported AST node type %T", n)
	}
	return nil
}
