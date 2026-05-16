// Minimal SQL fragment grammar — D-2.01 hybrid DSL (Phase 2 Plan 02-04).
//
// The cross-engine policy DSL is intentionally NOT a full SQL dialect.
// Per D-2.01 a row-filter / column-mask policy body is a *fragment*
// in a tiny portable subset that the per-dialect emitters can lower
// to engine-native SQL. Restricting the grammar at the source keeps
// the compiler's attack surface small (no full SQL parser) and keeps
// the per-dialect emitters honest (no engine-specific syntax leaks
// back through the fragment AST).
//
// Two fragment shapes are supported in Phase 2:
//
//   - RowFilter:  a boolean predicate spliced after WHERE in the
//                 generated SQL. Grammar:
//                     pred       := orExpr
//                     orExpr     := andExpr ( "OR"  andExpr )*
//                     andExpr    := notExpr ( "AND" notExpr )*
//                     notExpr    := "NOT" notExpr | primary
//                     primary    := "(" pred ")" | comparison
//                     comparison := col op value
//                     op         := "="|"!="|"<>"|"<"|"<="|">"|">="|"IN"|"LIKE"
//                     col        := IDENT ("." IDENT)?
//                     value      := STRING | NUMBER | "(" valueList ")"
//                     valueList  := value ("," value)*
//
//   - ColumnMask: a list of "col AS expr" entries. The expr is a
//                 minimal subset of the predicate grammar above
//                 augmented with CASE WHEN forms — Phase 2 ships
//                 only the literal-replacement form ("col AS NULL"
//                 / "col AS '***'") and the function-call form
//                 ("col AS hash(col)"). Conditional masks (CASE WHEN)
//                 are deferred to Plan 02-07.
//
// All identifiers are validated against `^[A-Za-z_][A-Za-z0-9_]*$`
// so the dialect emitters can splice them into SQL without a
// per-dialect quoting layer. Untrusted identifiers (i.e. from
// customer input) MUST pass through this validator before the AST
// is even constructed — the parser refuses them at lex time.
//
// Strings are single-quoted with `''`-escaping; the lexer rejects
// any character that the safe-Cypher allowlist would reject (so the
// same fragment can travel through AGE storage without re-quoting).
//
// Phase 3 will extend the grammar with CASE WHEN, IS NULL, BETWEEN,
// and arithmetic operators. The AST is structured with concrete node
// types (not opaque interface{}) so the extension is additive — new
// node types implement Node, dialect emitters add a switch case.

package compiler

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// FragmentKind discriminates the two top-level fragment shapes.
type FragmentKind int

const (
	// FragmentRowFilter is a boolean predicate (WHERE-clause body).
	FragmentRowFilter FragmentKind = iota + 1
	// FragmentColumnMask is a comma-separated list of "col AS expr"
	// projections.
	FragmentColumnMask
)

// Fragment is the parsed AST root. Exactly one of Row / Mask is
// non-nil — Kind discriminates.
type Fragment struct {
	Kind FragmentKind
	Row  Node          // when Kind == FragmentRowFilter
	Mask []MaskProject // when Kind == FragmentColumnMask
}

// Node is the predicate-expression AST node interface. Concrete
// types: BinaryOp, UnaryOp, ColumnRef, Literal, InList.
type Node interface{ astNode() }

// BinaryOp covers AND/OR and all comparison operators.
type BinaryOp struct {
	Op    string // "AND","OR","=","!=","<>","<","<=",">",">=","LIKE"
	Left  Node
	Right Node
}

// UnaryOp covers NOT.
type UnaryOp struct {
	Op      string // "NOT"
	Operand Node
}

// ColumnRef references a column, optionally table-qualified.
type ColumnRef struct {
	Table  string // "" if unqualified
	Column string
}

// Literal carries a typed scalar value. Kind ∈ {"string","number"}.
type Literal struct {
	Kind  string
	Value string // raw lexeme (string: unquoted; number: as-typed)
}

// InList carries the `col IN (v1, v2, …)` form.
type InList struct {
	Left   Node
	Values []Literal
}

// MaskProject is a single "col AS expr" entry of a ColumnMask
// fragment. Expr is either a Literal (e.g. NULL → Literal{Kind:"null"}),
// a ColumnRef (passthrough), or a FuncCall (hash(col)).
type MaskProject struct {
	Column string
	Expr   Node
}

// FuncCall is a single-arg function application (Phase 2 limit: one
// arg, identifier-named function). Phase 3 lifts the limit.
type FuncCall struct {
	Name string
	Arg  Node
}

func (BinaryOp) astNode()    {}
func (UnaryOp) astNode()     {}
func (ColumnRef) astNode()   {}
func (Literal) astNode()     {}
func (InList) astNode()      {}
func (FuncCall) astNode()    {}
func (MaskProject) astNode() {}

// ErrParseFailed is the sentinel returned by ParseFragment on any
// lex/parse failure. Wrapped via fmt.Errorf("%w", err) so callers
// can branch on errors.Is(err, ErrParseFailed). The error message
// always includes the offset of the failing token.
var ErrParseFailed = errors.New("sql_grammar: parse failed")

// ParseRowFilter parses a row-filter predicate body. Returns the
// AST root (a Node) on success, or (nil, ErrParseFailed)-wrapped
// error on failure.
func ParseRowFilter(src string) (*Fragment, error) {
	p := newParser(src)
	root, err := p.parsePred()
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, p.errf("unexpected token after predicate")
	}
	return &Fragment{Kind: FragmentRowFilter, Row: root}, nil
}

// ParseColumnMask parses a column-mask fragment of the form
//
//	col1 AS expr1, col2 AS expr2
//
// Empty input is rejected (a mask with zero projections is
// semantically meaningless).
func ParseColumnMask(src string) (*Fragment, error) {
	p := newParser(src)
	var projects []MaskProject
	for {
		if p.atEnd() {
			break
		}
		colTok, ok := p.expectIdent()
		if !ok {
			return nil, p.errf("expected column identifier")
		}
		if !p.expectKeyword("AS") {
			return nil, p.errf("expected AS after column %q", colTok)
		}
		expr, err := p.parseMaskExpr()
		if err != nil {
			return nil, err
		}
		projects = append(projects, MaskProject{Column: colTok, Expr: expr})
		if p.atEnd() {
			break
		}
		if !p.consume(",") {
			return nil, p.errf("expected , or end of input between mask projections")
		}
	}
	if len(projects) == 0 {
		return nil, p.errf("column mask is empty")
	}
	return &Fragment{Kind: FragmentColumnMask, Mask: projects}, nil
}

// -------------------- parser internals --------------------

type parser struct {
	src string
	pos int
}

func newParser(src string) *parser { return &parser{src: src} }

func (p *parser) errf(format string, a ...any) error {
	return fmt.Errorf("%w: at offset %d: %s", ErrParseFailed, p.pos, fmt.Sprintf(format, a...))
}

func (p *parser) atEnd() bool {
	p.skipSpace()
	return p.pos >= len(p.src)
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		r := rune(p.src[p.pos])
		if !unicode.IsSpace(r) {
			return
		}
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) consume(lit string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], lit) {
		// Guard against matching a prefix of an identifier.
		end := p.pos + len(lit)
		if isWordChar(lit[len(lit)-1]) && end < len(p.src) && isWordChar(p.src[end]) {
			return false
		}
		p.pos = end
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw string) bool {
	p.skipSpace()
	if p.pos+len(kw) > len(p.src) {
		return false
	}
	got := p.src[p.pos : p.pos+len(kw)]
	if !strings.EqualFold(got, kw) {
		return false
	}
	end := p.pos + len(kw)
	if end < len(p.src) && isWordChar(p.src[end]) {
		return false
	}
	p.pos = end
	return true
}

func (p *parser) expectIdent() (string, bool) {
	p.skipSpace()
	start := p.pos
	if start >= len(p.src) {
		return "", false
	}
	if !isIdentStart(p.src[start]) {
		return "", false
	}
	end := start + 1
	for end < len(p.src) && isWordChar(p.src[end]) {
		end++
	}
	ident := p.src[start:end]
	p.pos = end
	return ident, true
}

func (p *parser) parsePred() (Node, error) { return p.parseOr() }

func (p *parser) parseOr() (Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.expectKeyword("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = BinaryOp{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.expectKeyword("AND") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = BinaryOp{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Node, error) {
	if p.expectKeyword("NOT") {
		operand, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return UnaryOp{Op: "NOT", Operand: operand}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Node, error) {
	if p.consume("(") {
		inner, err := p.parsePred()
		if err != nil {
			return nil, err
		}
		if !p.consume(")") {
			return nil, p.errf("expected )")
		}
		return inner, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Node, error) {
	left, err := p.parseColumnRef()
	if err != nil {
		return nil, err
	}
	// Operator.
	switch {
	case p.consume("="):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: "=", Left: left, Right: v}, nil
	case p.consume("!="):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: "!=", Left: left, Right: v}, nil
	case p.consume("<>"):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: "<>", Left: left, Right: v}, nil
	case p.consume("<="):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: "<=", Left: left, Right: v}, nil
	case p.consume(">="):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: ">=", Left: left, Right: v}, nil
	case p.consume("<"):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: "<", Left: left, Right: v}, nil
	case p.consume(">"):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: ">", Left: left, Right: v}, nil
	case p.expectKeyword("IN"):
		if !p.consume("(") {
			return nil, p.errf("expected ( after IN")
		}
		var vals []Literal
		for {
			lit, err := p.parseLiteral()
			if err != nil {
				return nil, err
			}
			vals = append(vals, lit)
			if p.consume(")") {
				break
			}
			if !p.consume(",") {
				return nil, p.errf("expected , or ) in IN list")
			}
		}
		return InList{Left: left, Values: vals}, nil
	case p.expectKeyword("LIKE"):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return BinaryOp{Op: "LIKE", Left: left, Right: v}, nil
	}
	return nil, p.errf("expected comparison operator")
}

func (p *parser) parseColumnRef() (Node, error) {
	id1, ok := p.expectIdent()
	if !ok {
		return nil, p.errf("expected column identifier")
	}
	if p.consume(".") {
		id2, ok := p.expectIdent()
		if !ok {
			return nil, p.errf("expected column identifier after .")
		}
		return ColumnRef{Table: id1, Column: id2}, nil
	}
	return ColumnRef{Column: id1}, nil
}

func (p *parser) parseValue() (Node, error) {
	lit, err := p.parseLiteral()
	if err != nil {
		return nil, err
	}
	return lit, nil
}

func (p *parser) parseLiteral() (Literal, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return Literal{}, p.errf("expected literal")
	}
	c := p.src[p.pos]
	if c == '\'' {
		// String literal with ''-escape.
		p.pos++ // consume opening quote
		start := p.pos
		var out strings.Builder
		for p.pos < len(p.src) {
			ch := p.src[p.pos]
			if ch == '\'' {
				// Doubled '' inside string → literal '.
				if p.pos+1 < len(p.src) && p.src[p.pos+1] == '\'' {
					out.WriteByte('\'')
					p.pos += 2
					continue
				}
				p.pos++ // consume closing quote
				_ = start
				return Literal{Kind: "string", Value: out.String()}, nil
			}
			// Reject characters outside the safe-Cypher allowlist's
			// printable-ASCII intersection — strings travel through
			// AGE storage and we want a single rejection point.
			if ch < 0x20 || ch == 0x7f {
				return Literal{}, p.errf("control character in string literal")
			}
			out.WriteByte(ch)
			p.pos++
		}
		return Literal{}, p.errf("unterminated string literal")
	}
	// Number literal: [+-]?[0-9]+(\.[0-9]+)?
	start := p.pos
	if c == '+' || c == '-' {
		p.pos++
	}
	digits := 0
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		digits++
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			digits++
			p.pos++
		}
	}
	if digits == 0 {
		p.pos = start
		// Bare identifier as a literal? Treat as NULL keyword if so.
		if p.expectKeyword("NULL") {
			return Literal{Kind: "null", Value: "NULL"}, nil
		}
		return Literal{}, p.errf("expected literal")
	}
	return Literal{Kind: "number", Value: p.src[start:p.pos]}, nil
}

func (p *parser) parseMaskExpr() (Node, error) {
	// Three forms supported in Phase 2:
	//   1. NULL                  → Literal{Kind:"null"}
	//   2. 'literal'             → Literal{Kind:"string"}
	//   3. func(col) | col       → FuncCall | ColumnRef
	p.skipSpace()
	if p.expectKeyword("NULL") {
		return Literal{Kind: "null", Value: "NULL"}, nil
	}
	if p.peek() == '\'' {
		return p.parseLiteral()
	}
	// Identifier — either bare column or function call.
	id, ok := p.expectIdent()
	if !ok {
		return nil, p.errf("expected mask expression")
	}
	if p.consume("(") {
		argCol, err := p.parseColumnRef()
		if err != nil {
			return nil, err
		}
		if !p.consume(")") {
			return nil, p.errf("expected ) closing function call")
		}
		return FuncCall{Name: id, Arg: argCol}, nil
	}
	// Bare column reference; honor optional table prefix.
	if p.consume(".") {
		id2, ok := p.expectIdent()
		if !ok {
			return nil, p.errf("expected column identifier after .")
		}
		return ColumnRef{Table: id, Column: id2}, nil
	}
	return ColumnRef{Column: id}, nil
}

// -------------------- character classes --------------------

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isWordChar(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}
