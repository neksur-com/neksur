// Plan 02-12 Task 1 — sqlproxy splicer (CR-A3 closure).
//
// # Phase 2 splicer shape
//
// SpliceArtifact parses the inbound user query against a MINIMAL
// single-table SELECT grammar and weaves the active
// CompiledPolicy.ArtifactBody into the query at the AST-correct
// position:
//
//   - row-filter artifact   → appends or AND-conjoins the predicate as
//                             a real WHERE-clause body (preserving
//                             trailing GROUP BY / HAVING / ORDER BY /
//                             LIMIT clauses verbatim).
//   - column-mask artifact  → rewrites the projection list to substitute
//                             each masked column with the artifact's
//                             `col AS expr` projection, keeping the
//                             original column name as an alias so
//                             downstream consumers see the same column.
//
// The supported SELECT grammar is intentionally narrow — Phase 2 is the
// load-bearing read-path wedge, not the full SQL surface:
//
//	SELECT <projection-list>
//	  FROM <table>                          -- single bare table, no AS alias
//	  [WHERE <pred>]                        -- optional, AND-conjoined on rewrite
//	  [GROUP BY <cols>]                     -- preserved verbatim
//	  [HAVING <pred>]                       -- preserved verbatim
//	  [ORDER BY <cols>]                     -- preserved verbatim
//	  [LIMIT <n>]                           -- preserved verbatim
//
// Every form outside this grammar is REJECTED with
// sqlproxy.ErrUnsupportedQueryShape — JOIN, subquery in FROM, CTE
// (WITH …), multi-table comma, set operations (UNION / INTERSECT /
// EXCEPT), and non-SELECT DML (INSERT / UPDATE / DELETE). Phase 3
// extends the grammar; the rejection is loud (HTTP 422 + the
// distinct metric label `unsupported_query_shape` so SREs can
// distinguish "policy author wrote a shape we don't support yet"
// from "client sent un-parsable SQL").
//
// Malformed SQL (unterminated string, mismatched parens, missing
// FROM, …) returns sqlproxy.ErrInjectionFailed — same fail-closed
// posture as iter-1's env-gate, only now the gate is the parser
// itself and there is no escape hatch.
//
// # Lexer primitives
//
// The lexer primitives (skipSpace / peek / consume / expectKeyword /
// expectIdent) duplicate the corresponding helpers in
// internal/policy/compiler/sql_grammar.go. The duplication is
// INTENTIONAL — Phase 2's anti-yak-shaving rule says hoisting to a
// shared `internal/sqlcommon/lex.go` is Phase 3 work (refactor when
// a third consumer appears). The two grammars are different
// (predicate vs top-level SELECT) but the lexer primitives lift
// cleanly.
//
// # Pitfall 11 (no query body logging)
//
// SpliceArtifact never logs the query string or the artifact body.
// All error returns wrap a sentinel from the parent `sqlproxy`
// package; callers (server.go) log only the metric reason label.

package dialect

import (
	"fmt"
	"strings"

	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
)

// SpliceArtifact rewrites `query` to enforce the active CompiledPolicy
// artifact. The kind argument is one of store.KindRowFilter or
// store.KindColumnMask; predicate-kind artifacts are gateway-handled
// and MUST NOT reach this function (callers in trino.go / spark.go
// skip predicate kinds before calling).
//
// The principal argument is reserved for Phase 3 binding (principal.sub
// / principal.roles in the spliced predicate); Phase 2 ignores it.
//
// Returns one of:
//   - (rewritten, nil) on success.
//   - ("", err) where errors.Is(err, sqlproxy.ErrUnsupportedQueryShape)
//     for query shapes outside the Phase 2 grammar.
//   - ("", err) where errors.Is(err, sqlproxy.ErrSpliceMismatch) for
//     column-mask requests against a SELECT * or against a projection
//     that doesn't reference the masked column.
//   - ("", err) where errors.Is(err, sqlproxy.ErrInjectionFailed) for
//     malformed SQL or an unknown kind discriminator.
func SpliceArtifact(query string, artifactBody []byte, kind string, _ sqlproxy.Claims) (string, error) {
	body := string(artifactBody)
	switch kind {
	case store.KindRowFilter, "":
		// Empty kind is tolerated as the Phase 2 default — every existing
		// compiler dialect emitter produces row-filter bodies; the
		// ArtifactKind field is new in Plan 02-12. Backwards-compat.
		return spliceRowFilter(query, body)
	case store.KindColumnMask:
		return spliceColumnMask(query, body)
	default:
		return "", fmt.Errorf("sqlproxy/dialect: unknown artifact kind %q: %w", kind, sqlproxy.ErrInjectionFailed)
	}
}

// spliceRowFilter parses query as a single-table SELECT and either
// appends `WHERE (body)` (no existing WHERE) or rewrites the WHERE
// clause as `WHERE (existing) AND (body)`. Tail clauses (GROUP BY,
// HAVING, ORDER BY, LIMIT) are preserved verbatim.
func spliceRowFilter(query, body string) (string, error) {
	s, err := parseSelect(query)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(s.projection)
	b.WriteString(" FROM ")
	b.WriteString(s.table)
	if strings.TrimSpace(s.where) == "" {
		b.WriteString(" WHERE (")
		b.WriteString(body)
		b.WriteByte(')')
	} else {
		b.WriteString(" WHERE (")
		b.WriteString(strings.TrimSpace(s.where))
		b.WriteString(") AND (")
		b.WriteString(body)
		b.WriteByte(')')
	}
	if t := strings.TrimSpace(s.tail); t != "" {
		b.WriteByte(' ')
		b.WriteString(t)
	}
	return b.String(), nil
}

// spliceColumnMask parses query as a single-table SELECT and rewrites
// the projection list to substitute each masked column from `body` with
// the masked expression. Body is parsed as a comma-separated list of
// `col AS expr` pairs (the per-dialect compiler already produced this
// shape — see internal/policy/compiler/dialect/{trino,spark}.go's
// CompileColumnMask output).
//
// Returns ErrSpliceMismatch if the projection is `*` or if a masked
// column is not present in the projection list.
func spliceColumnMask(query, body string) (string, error) {
	s, err := parseSelect(query)
	if err != nil {
		return "", err
	}
	// Phase 2 limitation: SELECT * requires schema-aware expansion which
	// is Plan 02-13 / Phase 3 work. Reject loudly so the policy author
	// sees a deterministic error rather than a silently-unmasked query.
	if strings.TrimSpace(s.projection) == "*" {
		return "", fmt.Errorf("sqlproxy/dialect: column-mask cannot apply to SELECT *: %w", sqlproxy.ErrSpliceMismatch)
	}
	masks, err := parseMaskProjections(body)
	if err != nil {
		return "", err
	}
	projCols, err := parseProjectionList(s.projection)
	if err != nil {
		return "", err
	}
	rewritten, err := applyMasks(projCols, masks)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(rewritten, ", "))
	b.WriteString(" FROM ")
	b.WriteString(s.table)
	if w := strings.TrimSpace(s.where); w != "" {
		b.WriteString(" WHERE ")
		b.WriteString(w)
	}
	if t := strings.TrimSpace(s.tail); t != "" {
		b.WriteByte(' ')
		b.WriteString(t)
	}
	return b.String(), nil
}

// selectShape carries the decomposed clauses of a parsed single-table
// SELECT. Each field carries its raw substring (post-whitespace-trim)
// from the input query so the splicer can reassemble the output
// preserving the user's identifier casing.
type selectShape struct {
	projection string // post-SELECT, pre-FROM
	table      string // post-FROM, pre-WHERE/GROUP/HAVING/ORDER/LIMIT
	where      string // post-WHERE, pre-tail; empty if no WHERE
	tail       string // GROUP BY / HAVING / ORDER BY / LIMIT verbatim
}

// parseSelect parses `query` against the Phase 2 single-table SELECT
// grammar. Returns selectShape on success, sqlproxy.ErrUnsupportedQueryShape
// for grammar-rejected forms, or sqlproxy.ErrInjectionFailed for
// malformed SQL.
//
// The parser is intentionally NOT a full SQL parser — it tokenizes
// just enough to locate the four boundary keywords (FROM, WHERE,
// GROUP/HAVING/ORDER/LIMIT) and to detect rejection markers (JOIN,
// subquery, CTE, comma, UNION) at depth 0 (outside parentheses and
// outside string literals).
func parseSelect(query string) (selectShape, error) {
	p := newSpliceParser(query)
	p.skipSpace()

	// Reject CTE outright — a CTE moves the FROM target to a synthetic
	// table name (the CTE alias) and Phase 2 cannot reason about that.
	if p.peekKeyword("WITH") {
		return selectShape{}, errUnsupported("WITH/CTE")
	}

	// Reject non-SELECT DML. The proxy only enforces read paths in
	// Phase 2, but defense-in-depth: reject INSERT / UPDATE / DELETE
	// so a misrouted request can't slip through as a no-op rewrite.
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE"} {
		if p.peekKeyword(kw) {
			return selectShape{}, errUnsupported("non-SELECT DML (" + kw + ")")
		}
	}

	if !p.consumeKeyword("SELECT") {
		return selectShape{}, errMalformed("expected SELECT at start of query")
	}

	// Projection — everything between SELECT and the top-level FROM
	// keyword. Tokenize char-by-char so we can correctly track string
	// literals and nested parens (e.g. `count(*)` in a projection).
	projStart := p.pos
	fromOffset, err := p.scanToKeyword("FROM", true /*requireWordBoundary*/)
	if err != nil {
		return selectShape{}, err
	}
	projection := strings.TrimSpace(p.src[projStart:fromOffset])
	if projection == "" {
		return selectShape{}, errMalformed("empty projection list")
	}

	// Position the parser past the FROM keyword.
	p.pos = fromOffset + len("FROM")
	p.skipSpace()

	// Table — single bare identifier (optionally dot-qualified). The
	// next non-identifier character must be whitespace, end-of-input,
	// or one of the tail keywords. Anything else (comma, JOIN, opening
	// paren) is a rejection.
	tableStart := p.pos
	if p.pos >= len(p.src) {
		return selectShape{}, errMalformed("expected table name after FROM")
	}

	// Opening paren immediately after FROM → subquery in FROM.
	if p.src[p.pos] == '(' {
		return selectShape{}, errUnsupported("subquery in FROM")
	}

	if !isIdentStart(p.src[p.pos]) {
		return selectShape{}, errMalformed("expected table name after FROM")
	}
	for p.pos < len(p.src) && (isWordChar(p.src[p.pos]) || p.src[p.pos] == '.') {
		p.pos++
	}
	table := p.src[tableStart:p.pos]
	if table == "" {
		return selectShape{}, errMalformed("expected table name after FROM")
	}

	// After the table identifier, the only legal next tokens are
	// whitespace, end-of-input, WHERE, or one of the tail-clause
	// keywords. A comma → multi-table FROM (rejected); a JOIN keyword
	// → join (rejected); a paren → unexpected.
	p.skipSpace()
	if p.pos < len(p.src) {
		// Reject comma-multi-table.
		if p.src[p.pos] == ',' {
			return selectShape{}, errUnsupported("multi-table comma FROM")
		}
		// Reject JOIN family (JOIN, INNER JOIN, LEFT JOIN, …).
		for _, jk := range []string{"JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "OUTER", "NATURAL"} {
			if p.peekKeyword(jk) {
				return selectShape{}, errUnsupported("JOIN")
			}
		}
		// Reject set operations.
		for _, sk := range []string{"UNION", "INTERSECT", "EXCEPT"} {
			if p.peekKeyword(sk) {
				return selectShape{}, errUnsupported("set operation (" + sk + ")")
			}
		}
		// At this point only WHERE / GROUP / HAVING / ORDER / LIMIT are
		// allowed leads. Anything else that looks like an identifier
		// (including bare `AS`) is a table alias — rejected in Phase 2
		// because aliasing changes WHERE-clause column-reference
		// resolution and is out of scope until Phase 3.
		if !p.peekAnyKeyword("WHERE", "GROUP", "HAVING", "ORDER", "LIMIT") {
			if p.peekKeyword("AS") || isIdentStart(p.src[p.pos]) {
				return selectShape{}, errUnsupported("table alias")
			}
		}
	}

	// WHERE — optional. Scan from current position to the first tail
	// keyword (or end-of-input) to delimit the WHERE-body substring.
	var where string
	if p.consumeKeyword("WHERE") {
		whereStart := p.pos
		tailStart := p.scanToTailKeyword()
		where = p.src[whereStart:tailStart]
		p.pos = tailStart
	}

	// Reject set operations AFTER a WHERE clause too (e.g. `… WHERE x=1 UNION …`).
	if p.peekAnyKeyword("UNION", "INTERSECT", "EXCEPT") {
		return selectShape{}, errUnsupported("set operation")
	}

	// Tail — GROUP BY / HAVING / ORDER BY / LIMIT, verbatim. Reject
	// set operations or trailing JOINs that may appear in the tail
	// region (defensive — the scanToTailKeyword above lands on the
	// FIRST tail keyword so an embedded UNION inside the tail would
	// already be detected by the consumer; this guard catches the
	// edge case where a non-tail keyword leads the tail region).
	tail := strings.TrimSpace(p.src[p.pos:])
	// Defense-in-depth: reject if tail begins with a non-tail keyword.
	// Allowed leads: GROUP / HAVING / ORDER / LIMIT.
	if tail != "" {
		if err := validateTail(tail); err != nil {
			return selectShape{}, err
		}
	}

	return selectShape{
		projection: projection,
		table:      table,
		where:      where,
		tail:       tail,
	}, nil
}

// validateTail confirms the trailing region starts with one of the
// allowed Phase 2 tail keywords and contains no rejection markers
// (JOIN, UNION, second-SELECT). Returns nil on accept.
func validateTail(tail string) error {
	p := newSpliceParser(tail)
	p.skipSpace()
	if !p.peekAnyKeyword("GROUP", "HAVING", "ORDER", "LIMIT") {
		return errUnsupported("unexpected tail clause")
	}
	// Reject any JOIN / set-op / second-SELECT anywhere in the tail.
	for _, kw := range []string{"JOIN", "UNION", "INTERSECT", "EXCEPT"} {
		if findKeyword(tail, kw) >= 0 {
			return errUnsupported("tail contains " + kw)
		}
	}
	return nil
}

// parseMaskProjections splits the column-mask artifact body into a
// list of (column → expression) pairs. The body shape is the
// comma-separated `col AS expr` form produced by the compiler's
// CompileColumnMask emitter.
type maskProj struct {
	col  string // the masked column identifier
	expr string // the masked expression (e.g. `'***'`, `sha256(ssn)`)
}

func parseMaskProjections(body string) ([]maskProj, error) {
	parts, err := splitTopLevelCommas(body)
	if err != nil {
		return nil, err
	}
	var out []maskProj
	for _, part := range parts {
		seg := strings.TrimSpace(part)
		if seg == "" {
			continue
		}
		// Find the case-insensitive ` AS ` separator at depth 0. We
		// don't allow `AS` to appear inside parens of the expression
		// (a function call argument containing `AS` is malformed for
		// Phase 2's grammar; the compiler never emits that shape).
		idx := findKeywordAtDepth0(seg, "AS")
		if idx < 0 {
			return nil, fmt.Errorf("sqlproxy/dialect: mask projection %q missing AS: %w", seg, sqlproxy.ErrInjectionFailed)
		}
		col := strings.TrimSpace(seg[:idx])
		expr := strings.TrimSpace(seg[idx+len("AS"):])
		if col == "" || expr == "" {
			return nil, fmt.Errorf("sqlproxy/dialect: malformed mask projection %q: %w", seg, sqlproxy.ErrInjectionFailed)
		}
		if !isSimpleIdent(col) {
			return nil, fmt.Errorf("sqlproxy/dialect: mask column %q must be a bare identifier: %w", col, sqlproxy.ErrInjectionFailed)
		}
		out = append(out, maskProj{col: col, expr: expr})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sqlproxy/dialect: empty column-mask artifact: %w", sqlproxy.ErrInjectionFailed)
	}
	return out, nil
}

// parseProjectionList splits a SELECT projection list into its
// constituent column expressions at the top-level commas.
func parseProjectionList(projection string) ([]string, error) {
	parts, err := splitTopLevelCommas(projection)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			return nil, fmt.Errorf("sqlproxy/dialect: empty projection element: %w", sqlproxy.ErrInjectionFailed)
		}
		out = append(out, t)
	}
	return out, nil
}

// applyMasks rewrites the projection list to substitute each masked
// column with the masked expression. The masked expression is
// emitted as `<expr> AS <col>` so downstream consumers see the same
// column name.
//
// Returns ErrSpliceMismatch if any masked column is not present in
// the projection list (the policy author referenced a column the
// user's query does not select).
func applyMasks(projCols []string, masks []maskProj) ([]string, error) {
	// Build a quick set of bare-column projection slots → index for
	// O(1) lookup. We only match BARE identifiers (no `t.col`, no
	// `col AS alias`) — anything else stays untouched. The Phase 2
	// contract: the policy author MUST list masked columns by their
	// bare name, and the user query MUST project them bare too.
	idxByCol := make(map[string]int, len(projCols))
	for i, col := range projCols {
		t := strings.TrimSpace(col)
		if isSimpleIdent(t) {
			idxByCol[t] = i
		}
	}
	rewritten := append([]string(nil), projCols...)
	for _, m := range masks {
		i, ok := idxByCol[m.col]
		if !ok {
			return nil, fmt.Errorf("sqlproxy/dialect: masked column %q not in projection: %w", m.col, sqlproxy.ErrSpliceMismatch)
		}
		rewritten[i] = m.expr + " AS " + m.col
	}
	return rewritten, nil
}

// splitTopLevelCommas splits s on commas that occur at depth 0
// (outside parens and outside single-quoted strings).
func splitTopLevelCommas(s string) ([]string, error) {
	var (
		out   []string
		start int
		depth int
		inStr bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			if c == '\'' {
				// Doubled '' is an escape inside the string.
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return nil, fmt.Errorf("sqlproxy/dialect: unbalanced parens: %w", sqlproxy.ErrInjectionFailed)
			}
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if inStr {
		return nil, fmt.Errorf("sqlproxy/dialect: unterminated string literal: %w", sqlproxy.ErrInjectionFailed)
	}
	if depth != 0 {
		return nil, fmt.Errorf("sqlproxy/dialect: unbalanced parens: %w", sqlproxy.ErrInjectionFailed)
	}
	out = append(out, s[start:])
	return out, nil
}

// findKeywordAtDepth0 finds the byte offset of a case-insensitive
// word-boundary-respecting keyword at depth 0 (outside parens / strings).
// Returns -1 if not found.
func findKeywordAtDepth0(s, kw string) int {
	var (
		depth int
		inStr bool
	)
	kwUp := strings.ToUpper(kw)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '(':
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		}
		if !inStr && depth == 0 && i+len(kw) <= len(s) {
			// Word boundary on the left.
			leftBoundary := i == 0 || !isWordChar(s[i-1])
			if !leftBoundary {
				continue
			}
			candidate := s[i : i+len(kw)]
			if strings.ToUpper(candidate) != kwUp {
				continue
			}
			// Word boundary on the right.
			rightBoundary := i+len(kw) == len(s) || !isWordChar(s[i+len(kw)])
			if rightBoundary {
				return i
			}
		}
	}
	return -1
}

// findKeyword finds the first depth-0 case-insensitive occurrence of
// kw in s (word-boundary-respecting). Returns -1 if not found.
func findKeyword(s, kw string) int {
	return findKeywordAtDepth0(s, kw)
}

// isSimpleIdent reports whether s is a bare identifier matching
// `^[A-Za-z_][A-Za-z0-9_]*$`. Used to gate column-mask substitution
// (only bare-identifier projection slots are eligible for substitution).
func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	if !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isWordChar(s[i]) {
			return false
		}
	}
	return true
}

// errMalformed wraps an ErrInjectionFailed message.
func errMalformed(msg string) error {
	return fmt.Errorf("sqlproxy/dialect: %s: %w", msg, sqlproxy.ErrInjectionFailed)
}

// errUnsupported wraps an ErrUnsupportedQueryShape message.
func errUnsupported(shape string) error {
	return fmt.Errorf("sqlproxy/dialect: %s not supported in Phase 2: %w", shape, sqlproxy.ErrUnsupportedQueryShape)
}

// -------------------- splice parser --------------------

// spliceParser is the splicer's lightweight lexer. Differs from the
// policy/compiler/sql_grammar.go parser in that it works at the
// top-level SELECT grammar (table boundary detection) rather than
// the predicate grammar; the lexer primitives are duplicated below
// (anti-yak-shaving — DRY refactor is Phase 3 work).
type spliceParser struct {
	src string
	pos int
}

func newSpliceParser(src string) *spliceParser { return &spliceParser{src: src} }

func (p *spliceParser) skipSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		return
	}
}

// peekKeyword reports whether the next non-whitespace token is kw
// (case-insensitive, word-boundary-respecting). Does not advance pos.
func (p *spliceParser) peekKeyword(kw string) bool {
	save := p.pos
	defer func() { p.pos = save }()
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
	return true
}

// peekAnyKeyword returns true if any of kws is the next keyword.
func (p *spliceParser) peekAnyKeyword(kws ...string) bool {
	for _, kw := range kws {
		if p.peekKeyword(kw) {
			return true
		}
	}
	return false
}

// consumeKeyword peeks for kw and advances past it on match.
func (p *spliceParser) consumeKeyword(kw string) bool {
	if !p.peekKeyword(kw) {
		return false
	}
	p.skipSpace()
	p.pos += len(kw)
	return true
}

// scanToKeyword scans forward from p.pos searching for a case-insensitive
// occurrence of kw at depth 0 (outside parens / strings). Returns the
// byte offset of the kw start, or an error if not found. On success
// p.pos is NOT advanced (caller advances after extracting the prefix).
//
// If kw is not found, returns ErrInjectionFailed (malformed) when
// requireWordBoundary is true and the searched keyword is mandatory
// for grammar shape (the FROM-finder path).
func (p *spliceParser) scanToKeyword(kw string, requireWordBoundary bool) (int, error) {
	kwUp := strings.ToUpper(kw)
	var (
		depth int
		inStr bool
	)
	for i := p.pos; i < len(p.src); i++ {
		c := p.src[i]
		switch {
		case inStr:
			if c == '\'' {
				if i+1 < len(p.src) && p.src[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return 0, errMalformed("unbalanced parens")
			}
			depth--
		}
		if inStr || depth != 0 {
			continue
		}
		// Match kw with optional word-boundary check.
		if i+len(kw) > len(p.src) {
			continue
		}
		// Left boundary at i.
		if i > p.pos && requireWordBoundary && isWordChar(p.src[i-1]) {
			continue
		}
		if strings.ToUpper(p.src[i:i+len(kw)]) != kwUp {
			continue
		}
		// Right boundary at i+len(kw).
		if requireWordBoundary && i+len(kw) < len(p.src) && isWordChar(p.src[i+len(kw)]) {
			continue
		}
		// Sanity: do not match keywords that are the first byte of pos.
		// (FROM at start of query → impossible because SELECT was
		// consumed; defense-in-depth.)
		return i, nil
	}
	if inStr {
		return 0, errMalformed("unterminated string literal")
	}
	if depth != 0 {
		return 0, errMalformed("unbalanced parens")
	}
	return 0, errMalformed("expected " + kw + " keyword")
}

// scanToTailKeyword scans from p.pos for the first depth-0 occurrence
// of GROUP / HAVING / ORDER / LIMIT (case-insensitive, word-boundary).
// Returns the byte offset of the keyword start, or len(p.src) if not
// found.
func (p *spliceParser) scanToTailKeyword() int {
	var (
		depth int
		inStr bool
	)
	for i := p.pos; i < len(p.src); i++ {
		c := p.src[i]
		switch {
		case inStr:
			if c == '\'' {
				if i+1 < len(p.src) && p.src[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '(':
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		}
		if inStr || depth != 0 {
			continue
		}
		// Left word boundary.
		if i > 0 && isWordChar(p.src[i-1]) {
			continue
		}
		for _, kw := range []string{"GROUP", "HAVING", "ORDER", "LIMIT"} {
			if i+len(kw) <= len(p.src) &&
				strings.EqualFold(p.src[i:i+len(kw)], kw) &&
				(i+len(kw) == len(p.src) || !isWordChar(p.src[i+len(kw)])) {
				return i
			}
		}
	}
	return len(p.src)
}

// -------------------- character classes (duplicated) --------------------

// isIdentStart reports whether b can start an SQL identifier
// ([A-Za-z_]). Duplicated from internal/policy/compiler/sql_grammar.go;
// the DRY refactor to a shared internal/sqlcommon/lex.go is Phase 3.
func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// isWordChar reports whether b is an identifier-continuation byte
// ([A-Za-z0-9_]). Mirror of the same helper in sql_grammar.go.
func isWordChar(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}
