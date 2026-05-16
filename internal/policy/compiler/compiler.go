// Cross-engine policy compiler — D-2.04 / Plan 02-04.
//
// The Compiler is the Phase 2 entry-point that takes a single Policy
// (loaded from AGE via internal/policy/store.LoadPoliciesForTable),
// looks up the tenant's registered engines from public.engines, and
// produces one CompiledPolicy node per (Policy × Engine) pair. Each
// compile is followed by a synthetic probe (ProbeRunner.Run) against
// the engine; the resulting CompiledPolicy status is one of
// {pending, active, probe_failed, compile_failed}.
//
// Design notes:
//
//   - Stateless orchestrator. The Compiler holds only the dependencies
//     (dialect registry, probe runner, AGE stores, CEL env) — no
//     per-request state. Safe for concurrent CompileAll calls across
//     different tenants.
//
//   - Per-process LRU cache keyed by (PolicyID, EngineKind,
//     EngineVersion, SHA-256(source)). A re-compile of the same
//     source-engine pair is a microsecond cache hit; the cold path
//     pays ~5-15ms of CEL/SQL compile + a 5s-budget probe RTT.
//
//   - Dialect dispatch via map[string]dialect.DialectCompiler keyed
//     by engine kind. Unknown kinds → ErrCompileFailed (compile_failed
//     status; the gateway sees the marker but routes no traffic).
//
//   - The Compiler is the ONLY caller that constructs the
//     dialect.Ast* shim types (declared in dialect/trino.go for
//     historical reasons; conceptually a sub-package contract). The
//     adaptForDialect helper translates the parent AST (sql_grammar.go)
//     into the dialect-facing shape — keeps the dialect package free
//     of upward imports.

package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"

	"github.com/neksur-com/neksur/internal/iceberg"
	policycel "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/compiler/dialect"
	"github.com/neksur-com/neksur/internal/policy/store"
)

// CompiledArtifact is the in-memory projection of a CompiledPolicy
// artifact_body. For SQL fragments this carries the lowered SQL
// string; for CEL bodies this carries the *CELArtifact + the live
// cel.Program so the gateway can evaluate without a re-compile.
type CompiledArtifact struct {
	// Kind discriminates the body shape.
	//   "sql_row_filter":   Body is the WHERE-clause string.
	//   "sql_column_mask":  Body is the SELECT-projection string.
	//   "cel":              CEL is the *CELArtifact; Program is the
	//                      ready-to-eval cel.Program.
	Kind    string
	Body    string
	CEL     *CELArtifact
	Program cel.Program
}

// PolicySource is the discriminated union the Compiler accepts from
// the store layer. Exactly one of (DefinitionCEL, DefinitionSQL) is
// non-empty. PolicyKind ∈ {"row_filter","column_mask","abac",...}.
type PolicySource struct {
	PolicyID      string
	PolicyKind    string
	DefinitionCEL string
	DefinitionSQL string
}

// Compiler orchestrates per-engine compile + probe for one Policy at
// a time. Construct via NewCompiler.
type Compiler struct {
	dialects    map[string]dialect.DialectCompiler
	probes      *ProbeRunner
	cache       *ArtifactCache
	cstore      *store.CompiledStore
	engines     *store.EngineRegistry
	celEnv      *cel.Env
	celCompiler *policycel.Compiler
}

// CompilerConfig groups the constructor inputs.
type CompilerConfig struct {
	Dialects     map[string]dialect.DialectCompiler
	Probes       *ProbeRunner
	Cache        *ArtifactCache
	CompiledStore *store.CompiledStore
	EngineRegistry *store.EngineRegistry
	CELEnv       *cel.Env
	CELCompiler  *policycel.Compiler
}

// NewCompiler builds the cross-engine compiler. The caller is
// responsible for wiring the per-engine dialect emitters + the
// probe executors at startup.
func NewCompiler(cfg CompilerConfig) (*Compiler, error) {
	if cfg.Dialects == nil {
		return nil, fmt.Errorf("compiler: nil dialects map")
	}
	if cfg.CompiledStore == nil {
		return nil, fmt.Errorf("compiler: nil compiled store")
	}
	if cfg.EngineRegistry == nil {
		return nil, fmt.Errorf("compiler: nil engine registry")
	}
	if cfg.Cache == nil {
		c, err := NewArtifactCache(0)
		if err != nil {
			return nil, fmt.Errorf("compiler: default cache: %w", err)
		}
		cfg.Cache = c
	}
	if cfg.Probes == nil {
		cfg.Probes = NewProbeRunner(nil)
	}
	return &Compiler{
		dialects:    cfg.Dialects,
		probes:      cfg.Probes,
		cache:       cfg.Cache,
		cstore:      cfg.CompiledStore,
		engines:     cfg.EngineRegistry,
		celEnv:      cfg.CELEnv,
		celCompiler: cfg.CELCompiler,
	}, nil
}

// CompileAll compiles the Policy for every engine registered under
// the calling tenant and persists a CompiledPolicy node per (engine,
// table) pair. Returns the per-engine results so the caller can
// surface partial failures (e.g., Dremio stub) in the API response.
//
// The function is idempotent — re-running with the same inputs
// updates the CompiledPolicy nodes in place (UpsertCompiledPolicy is
// idempotent at the AGE layer). The cache short-circuits the dialect
// emission when the (PolicyID, Engine, source-hash) triple matches a
// prior compile.
//
// `ref` is the table this policy applies to; the caller (Plan 02-05+
// trigger) supplies it from the Policy's :SCHEMA_GOVERNS / :ROW_FILTER_GOVERNS
// / etc edge. Phase 2 limits one table per policy; Plan 02-04 Part B
// (multi_table.go) lifts that to N tables per policy.
func (c *Compiler) CompileAll(ctx context.Context, policy PolicySource, ref iceberg.TableRef) ([]CompileResult, error) {
	engines, err := c.engines.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("compiler: load engine registry: %w", err)
	}
	if len(engines) == 0 {
		// No engines registered → nothing to compile. Not an error.
		return nil, nil
	}

	results := make([]CompileResult, 0, len(engines))
	ns := strings.Join(ref.Namespace, ".")
	for _, e := range engines {
		res := c.compileOne(ctx, policy, ref, ns, e)
		results = append(results, res)
	}
	return results, nil
}

// CompileResult is the per-engine outcome surfaced to the caller.
type CompileResult struct {
	EngineKind    string
	EngineVersion string
	Status        store.CompiledPolicyStatus
	Err           error // non-nil on compile_failed or probe_failed
}

func (c *Compiler) compileOne(ctx context.Context, p PolicySource, ref iceberg.TableRef, ns string, e store.Engine) CompileResult {
	res := CompileResult{EngineKind: e.Kind, EngineVersion: e.Version}

	source := p.DefinitionSQL
	if source == "" {
		source = p.DefinitionCEL
	}
	checksum := sha256Hex(source)

	// LRU short-circuit. A hit means we've already lowered this
	// source for this (engine, version) and it landed an active
	// CompiledPolicy — re-persist (idempotent) and skip the probe.
	if cached, ok := c.cache.Get(p.PolicyID, e.Kind, e.Version, source); ok {
		res.Status = store.CompiledPolicyStatusActive
		if persistErr := c.persist(ctx, p, ref, ns, e, cached, store.CompiledPolicyStatusActive, checksum); persistErr != nil {
			res.Status = store.CompiledPolicyStatusCompileFailed
			res.Err = persistErr
		}
		return res
	}

	// Cold path: dispatch to the dialect emitter (SQL fragments) or
	// the CEL artifact compiler (CEL bodies).
	artifact, compileErr := c.compileArtifact(p, e)
	if compileErr != nil {
		res.Status = store.CompiledPolicyStatusCompileFailed
		res.Err = compileErr
		// Persist the failed marker so the planner sees the row.
		_ = c.persist(ctx, p, ref, ns, e, &CompiledArtifact{Kind: "failed", Body: ""}, store.CompiledPolicyStatusCompileFailed, checksum)
		return res
	}

	// Probe (5s timeout inside ProbeRunner.Run). Only meaningful for
	// SQL row-filter fragments — column masks and CEL artifacts have
	// no engine-side WHERE clause to probe.
	if artifact.Kind == "sql_row_filter" {
		tableFQN := engineTableFQN(e.Kind, ns, ref.Name)
		if probeErr := c.probes.Run(ctx, e.Kind, tableFQN, artifact.Body); probeErr != nil {
			res.Status = store.CompiledPolicyStatusProbeFailed
			res.Err = probeErr
			_ = c.persist(ctx, p, ref, ns, e, artifact, store.CompiledPolicyStatusProbeFailed, checksum)
			return res
		}
	}

	// Persist as pending → active on success (no separate pending
	// window in Phase 2 — the compile+probe is synchronous).
	res.Status = store.CompiledPolicyStatusActive
	if persistErr := c.persist(ctx, p, ref, ns, e, artifact, store.CompiledPolicyStatusActive, checksum); persistErr != nil {
		res.Status = store.CompiledPolicyStatusCompileFailed
		res.Err = persistErr
		return res
	}

	// Warm the LRU.
	c.cache.Put(p.PolicyID, e.Kind, e.Version, source, artifact)
	return res
}

func (c *Compiler) compileArtifact(p PolicySource, e store.Engine) (*CompiledArtifact, error) {
	// CEL body branch.
	if p.DefinitionCEL != "" {
		if c.celEnv == nil || c.celCompiler == nil {
			return nil, fmt.Errorf("%w: cel env / compiler not wired", ErrCompileFailed)
		}
		art, prog, err := CompileCELArtifact(c.celEnv, c.celCompiler, p.PolicyID, p.DefinitionCEL)
		if err != nil {
			return nil, err
		}
		encoded, err := art.Encode()
		if err != nil {
			return nil, fmt.Errorf("%w: encode cel artifact: %v", ErrCompileFailed, err)
		}
		return &CompiledArtifact{
			Kind:    "cel",
			Body:    encoded,
			CEL:     art,
			Program: prog,
		}, nil
	}

	// SQL fragment branch.
	d, ok := c.dialects[e.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: no dialect emitter for engine kind %q", ErrCompileFailed, e.Kind)
	}
	switch p.PolicyKind {
	case "row_filter":
		frag, err := ParseRowFilter(p.DefinitionSQL)
		if err != nil {
			return nil, fmt.Errorf("%w: parse row-filter: %v", ErrCompileFailed, err)
		}
		shimRoot := adaptNodeForDialect(frag.Row)
		sql, err := d.CompileRowFilter(joinTable(e.Kind, p.PolicyID), shimRoot)
		if err != nil {
			if errors.Is(err, dialect.ErrDialectStub) {
				return nil, fmt.Errorf("%w: %v", ErrDialectStub, err)
			}
			return nil, fmt.Errorf("%w: dialect %s row-filter: %v", ErrCompileFailed, e.Kind, err)
		}
		return &CompiledArtifact{Kind: "sql_row_filter", Body: sql}, nil
	case "column_mask":
		frag, err := ParseColumnMask(p.DefinitionSQL)
		if err != nil {
			return nil, fmt.Errorf("%w: parse column-mask: %v", ErrCompileFailed, err)
		}
		shimMask := adaptMaskForDialect(frag.Mask)
		sql, err := d.CompileColumnMask(joinTable(e.Kind, p.PolicyID), shimMask)
		if err != nil {
			if errors.Is(err, dialect.ErrDialectStub) {
				return nil, fmt.Errorf("%w: %v", ErrDialectStub, err)
			}
			return nil, fmt.Errorf("%w: dialect %s column-mask: %v", ErrCompileFailed, e.Kind, err)
		}
		return &CompiledArtifact{Kind: "sql_column_mask", Body: sql}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported SQL policy kind %q", ErrCompileFailed, p.PolicyKind)
	}
}

func (c *Compiler) persist(ctx context.Context, p PolicySource, ref iceberg.TableRef, ns string, e store.Engine, art *CompiledArtifact, st store.CompiledPolicyStatus, checksum string) error {
	cp := store.CompiledPolicy{
		PolicyID:       p.PolicyID,
		EngineKind:     e.Kind,
		EngineVersion:  e.Version,
		TableName:      ref.Name,
		TableNamespace: ns,
		Status:         st,
		SourceChecksum: checksum,
		ArtifactBody:   art.Body,
	}
	if err := c.cstore.UpsertCompiledPolicy(ctx, cp); err != nil {
		return fmt.Errorf("compiler: persist CompiledPolicy: %w", err)
	}
	return nil
}

// engineTableFQN builds the engine-side fully-qualified table name
// used in the probe SQL. Trino: `iceberg.<ns>.<name>`. Spark:
// `<ns>.<name>` (assumes the catalog is the default). Dremio: same
// as Trino as a placeholder until the live emitter ships.
func engineTableFQN(engineKind, ns, name string) string {
	switch engineKind {
	case "trino":
		return fmt.Sprintf("iceberg.%s.%s", ns, name)
	case "spark":
		return fmt.Sprintf("%s.%s", ns, name)
	case "dremio":
		return fmt.Sprintf("iceberg.%s.%s", ns, name)
	default:
		return fmt.Sprintf("%s.%s", ns, name)
	}
}

// joinTable is a passthrough used as the dialect emitter's `table`
// argument. Phase 2 dialect emitters don't use this argument (they
// reference columns by name only, not table-qualified) but the
// signature is reserved for Plan 02-07's multi-table cross-join
// support.
func joinTable(engineKind, policyID string) string {
	return policyID
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// adaptNodeForDialect converts the parent compiler-package AST into
// the dialect-package's shim types. This keeps the dialect package
// import-cycle-free (it cannot import compiler).
func adaptNodeForDialect(n Node) any {
	switch v := n.(type) {
	case BinaryOp:
		return dialect.AstBinaryOp{Op: v.Op, Left: adaptNodeForDialect(v.Left), Right: adaptNodeForDialect(v.Right)}
	case UnaryOp:
		return dialect.AstUnaryOp{Op: v.Op, Operand: adaptNodeForDialect(v.Operand)}
	case ColumnRef:
		return dialect.AstColumnRef{Table: v.Table, Column: v.Column}
	case Literal:
		return dialect.AstLiteral{Kind: v.Kind, Value: v.Value}
	case InList:
		out := dialect.AstInList{Left: adaptNodeForDialect(v.Left)}
		for _, lit := range v.Values {
			out.Values = append(out.Values, dialect.AstLiteral{Kind: lit.Kind, Value: lit.Value})
		}
		return out
	case FuncCall:
		return dialect.AstFuncCall{Name: v.Name, Arg: adaptNodeForDialect(v.Arg)}
	case nil:
		return nil
	default:
		// Unreachable in correct callers; the parent package validates
		// the AST shape before invoking the emitter.
		return nil
	}
}

func adaptMaskForDialect(mask []MaskProject) []dialect.AstMaskProject {
	out := make([]dialect.AstMaskProject, 0, len(mask))
	for _, p := range mask {
		out = append(out, dialect.AstMaskProject{Column: p.Column, Expr: adaptNodeForDialect(p.Expr)})
	}
	return out
}
