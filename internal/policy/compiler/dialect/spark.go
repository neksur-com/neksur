// SparkSQL dialect emitter — live Phase 2 implementation (D-2.04).
//
// SparkSQL accepts the same portable subset as Trino with two
// dialect deltas in Phase 2:
//
//   - Hash function:  Spark exposes `sha2(col, 256)` for SHA-256,
//                     where Trino uses `sha256(to_utf8(...))`. The
//                     emitter rewrites the `hash(col)` mask form
//                     accordingly.
//   - IN-list NULL:   SparkSQL's IN-with-NULL semantics match Trino
//                     (3VL) so no rewrite is needed in Phase 2.
//                     Phase 3 may need to insert COALESCE wrappers
//                     when Spark Catalyst's `spark.sql.legacyNullInIN`
//                     is unset on the customer cluster.
//
// The emitter operates on the same AstBinaryOp/AstUnaryOp/...
// types as the Trino emitter — both share the dialect-package AST
// shapes (defined in trino.go for historical reasons; conceptually
// they're a shared sub-package surface).

package dialect

import (
	"fmt"
	"strings"
)

// SparkCompiler emits SparkSQL fragments.
type SparkCompiler struct{}

// NewSparkCompiler returns the Spark dialect emitter.
func NewSparkCompiler() *SparkCompiler { return &SparkCompiler{} }

// Kind returns "spark".
func (SparkCompiler) Kind() string { return "spark" }

// CompileRowFilter lowers a predicate AST to a SparkSQL WHERE body.
func (c SparkCompiler) CompileRowFilter(table string, predicate any) (string, error) {
	if predicate == nil {
		return "", fmt.Errorf("spark: nil row-filter predicate")
	}
	var sb strings.Builder
	if err := c.emitPred(&sb, predicate); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// CompileColumnMask lowers a column-mask AST to a SparkSQL projection.
func (c SparkCompiler) CompileColumnMask(table string, mask any) (string, error) {
	projects, ok := mask.([]AstMaskProject)
	if !ok {
		return "", fmt.Errorf("spark: column mask is not []AstMaskProject (got %T)", mask)
	}
	if len(projects) == 0 {
		return "", fmt.Errorf("spark: column mask is empty")
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

func (c SparkCompiler) emitPred(sb *strings.Builder, n any) error {
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
			return fmt.Errorf("spark: unsupported literal kind %q", v.Kind)
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
		switch v.Name {
		case "hash":
			// SparkSQL idiom: sha2(CAST(col AS STRING), 256).
			sb.WriteString("sha2(CAST(")
			if err := c.emitPred(sb, v.Arg); err != nil {
				return err
			}
			sb.WriteString(" AS STRING), 256)")
		case "lower", "upper", "length":
			sb.WriteString(v.Name)
			sb.WriteByte('(')
			if err := c.emitPred(sb, v.Arg); err != nil {
				return err
			}
			sb.WriteByte(')')
		default:
			return fmt.Errorf("spark: function %q not whitelisted", v.Name)
		}
	default:
		return fmt.Errorf("spark: unsupported AST node type %T", n)
	}
	return nil
}
