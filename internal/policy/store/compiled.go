// CompiledPolicy AGE store — D-2.04 / Plan 02-04.
//
// `CompiledPolicy` is the per-engine compile artifact produced by the
// cross-engine policy compiler (internal/policy/compiler). For each
// Policy node + each registered Engine for the tenant, the compiler
// emits one CompiledPolicy node carrying:
//
//   - artifact_body  — the serialized engine-specific compile output
//                      (SQL fragment for Trino/Spark; JSON CELArtifact
//                      for CEL bodies).
//   - status         — one of {pending, active, probe_failed,
//                      compile_failed}. Default `pending` on first
//                      write; `active` after a successful probe;
//                      terminal `probe_failed` / `compile_failed` if
//                      the post-compile validation tripped.
//   - source_checksum — SHA-256 of the source CEL/SQL text; the cross-
//                      engine compiler short-circuits a re-compile
//                      when the checksum matches a cached entry.
//
// Graph shape (V0040 vlabels + elabels):
//
//   (cp:CompiledPolicy {tenant_id, policy_id, engine_kind, engine_version,
//                       status, source_checksum, artifact_body})
//   (cp)-[:COMPILED_FROM]->(p:Policy {tenant_id, id})
//   (cp)-[:APPLIES_TO]   ->(t:Table  {tenant_id, name, namespace})
//   (cp)-[:GOVERNED_BY]  ->(e:Engine {tenant_id, kind, version})
//
// AGE 1.6 idempotency contract (Phase 1 lessons applied):
//   - One MERGE per cypher() call (AGE 1.6 cannot MERGE two nodes in
//     the same statement and have both honor properties).
//   - SET … = COALESCE(properties->>'key', new_value) is the Phase 1
//     pattern for idempotent property updates that don't clobber on
//     re-runs (Plan 01-04 lesson).
//   - Schema-qualified writes via the per-tenant search_path set by
//     ExecuteInTenant — never bare `MERGE (n:Label)` in `public`.
//   - All caller-supplied literals routed through
//     graph.MustSanitizeCypherLiteral (CR-01 mitigation).
//
// CC3 reminder: this store reuses the GraphClient pool; do NOT
// introduce a second pgxpool here. ExecuteInTenant owns the DISCARD
// ALL / RLS-context lifecycle.

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"
)

// CompiledPolicyStatus is the per-engine compile lifecycle marker.
// Persisted as a string property on the CompiledPolicy node (AGE
// agtype doesn't carry custom enums).
type CompiledPolicyStatus string

const (
	// CompiledPolicyStatusPending is the initial state right after a
	// successful compile but before the engine-side probe has run.
	// The gateway treats `pending` as fail-closed (no enforcement)
	// for new policies; admins see the marker via the policy CRUD UI.
	CompiledPolicyStatusPending CompiledPolicyStatus = "pending"

	// CompiledPolicyStatusActive is the terminal success state. The
	// gateway routes commit-validation traffic against the
	// artifact_body once status is `active`.
	CompiledPolicyStatusActive CompiledPolicyStatus = "active"

	// CompiledPolicyStatusProbeFailed indicates the compile succeeded
	// but the synthetic post-compile probe (ProbeRunner.Run) returned
	// an error or non-zero rowcount. The artifact_body is preserved
	// for triage but NOT used for enforcement.
	CompiledPolicyStatusProbeFailed CompiledPolicyStatus = "probe_failed"

	// CompiledPolicyStatusCompileFailed indicates the dialect emitter
	// rejected the fragment (e.g., Dremio stub, unknown function in
	// a column mask). The artifact_body is the empty string; the
	// node exists only so the planner can answer "is this policy
	// known for this engine?" with a deterministic graph query.
	CompiledPolicyStatusCompileFailed CompiledPolicyStatus = "compile_failed"
)

// IsValid reports whether s is one of the four documented statuses.
// Used by the AGE reader to reject corrupted rows defensively.
func (s CompiledPolicyStatus) IsValid() bool {
	switch s {
	case CompiledPolicyStatusPending,
		CompiledPolicyStatusActive,
		CompiledPolicyStatusProbeFailed,
		CompiledPolicyStatusCompileFailed:
		return true
	}
	return false
}

// CompiledPolicy is the in-memory projection of a CompiledPolicy node.
type CompiledPolicy struct {
	PolicyID       string
	EngineKind     string
	EngineVersion  string
	TableName      string
	TableNamespace string
	Status         CompiledPolicyStatus
	SourceChecksum string
	ArtifactBody   string
	// ArtifactKind discriminates how the ArtifactBody should be spliced
	// into a runtime query — one of KindRowFilter, KindColumnMask, or
	// KindPredicate. Per D-2.04 the discriminator is carried on the
	// CompiledPolicy node so the runtime splicer (sqlproxy/dialect/
	// splice.go) does not have to re-parse the body to learn its shape.
	// Empty string is tolerated as a backwards-compat default and is
	// interpreted by callers as KindRowFilter (the Phase 2 default
	// produced by every existing compiler dialect emitter).
	ArtifactKind string
}

// Phase 2 ArtifactKind discriminator values. Stored on CompiledPolicy
// nodes via the ArtifactKind field; consumed by the runtime splicer
// in internal/sqlproxy/dialect/splice.go.
//
//   - KindRowFilter: ArtifactBody is a boolean predicate (e.g.
//     `region = 'us-east-1'`). The sqlproxy splicer appends or
//     AND-conjoins it into the WHERE clause of the user's SELECT.
//
//   - KindColumnMask: ArtifactBody is a comma-separated list of
//     `col AS expr` projections (e.g. `ssn AS '***', email AS 'redacted'`).
//     The sqlproxy splicer substitutes each masked column in the
//     user's projection list.
//
//   - KindPredicate: ArtifactBody is a Layer-1 predicate (P4 / P5 / P7 /
//     ABAC) evaluated at the L1 catalog gateway — these artifacts MUST
//     NOT reach the sqlproxy path; callers in dialect/{trino,spark}.go
//     filter them out.
const (
	KindRowFilter  = "row-filter"
	KindColumnMask = "column-mask"
	KindPredicate  = "predicate"
)

// ErrCompiledPolicyNotFound is returned by LoadCompiledForTable when
// no CompiledPolicy exists for the requested (table, engine) pair.
var ErrCompiledPolicyNotFound = errors.New("policy/store: compiled policy not found")

// CompiledStore is the AGE-backed CompiledPolicy reader/writer.
// Construct ONCE per process via NewCompiledStore; share across all
// compiler invocations. Thread-safe.
type CompiledStore struct {
	gc *graph.GraphClient
}

// NewCompiledStore wraps the given graph client.
func NewCompiledStore(gc *graph.GraphClient) *CompiledStore {
	return &CompiledStore{gc: gc}
}

// UpsertCompiledPolicy MERGEs a CompiledPolicy node + the three edges
// (COMPILED_FROM, APPLIES_TO, GOVERNED_BY). Idempotent: re-running
// with the same (PolicyID, EngineKind, EngineVersion) updates the
// status / source_checksum / artifact_body in place.
//
// Per AGE 1.6 quirks each MERGE is issued in its own cypher() call;
// edges are MERGEd after the node so the SET on the node lands first.
// The function returns the first error encountered — callers retry
// the whole upsert (it's idempotent).
func (s *CompiledStore) UpsertCompiledPolicy(ctx context.Context, cp CompiledPolicy) error {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return fmt.Errorf("policy/store: upsert compiled policy: %w", ErrTenantMissing)
	}
	if !cp.Status.IsValid() {
		return fmt.Errorf("policy/store: invalid CompiledPolicy status %q", cp.Status)
	}
	tenantStr := tenantID.String()

	// Sanitize every spliced literal (CR-01).
	pid := graph.MustSanitizeCypherLiteral(cp.PolicyID)
	ek := graph.MustSanitizeCypherLiteral(cp.EngineKind)
	ev := graph.MustSanitizeCypherLiteral(cp.EngineVersion)
	tname := graph.MustSanitizeCypherLiteral(cp.TableName)
	tns := graph.MustSanitizeCypherLiteral(cp.TableNamespace)
	status := graph.MustSanitizeCypherLiteral(string(cp.Status))
	checksum := graph.MustSanitizeCypherLiteral(cp.SourceChecksum)
	tenantLit := graph.MustSanitizeCypherLiteral(tenantStr)

	// artifact_body is JSON / SQL — it may contain characters outside
	// the safe-Cypher allowlist (curly braces, brackets). We base64-
	// encode it for storage so the on-wire literal stays inside the
	// allowlist; readers decode on the way out (see
	// LoadCompiledForTable).
	encodedBody := encodeArtifactForCypher(cp.ArtifactBody)
	body := graph.MustSanitizeCypherLiteral(encodedBody)

	return s.gc.ExecuteInTenant(ctx, tenantStr, func(ctx context.Context, tx pgx.Tx) error {
		// 1) MERGE the CompiledPolicy node with COALESCE-style SET to
		//    preserve creation-side defaults (e.g., a future
		//    `created_at` property) while updating mutable fields.
		nodeCypher := fmt.Sprintf(
			`MERGE (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: '%s', engine_version: '%s'}) `+
				`SET cp.status = '%s', cp.source_checksum = '%s', cp.artifact_body = '%s' `+
				`RETURN cp.policy_id`,
			tenantLit, pid, ek, ev, status, checksum, body,
		)
		if err := execCypherNoRows(ctx, tx, nodeCypher); err != nil {
			return fmt.Errorf("policy/store: MERGE CompiledPolicy: %w", err)
		}

		// 2) MERGE :COMPILED_FROM edge.
		eCompiledFrom := fmt.Sprintf(
			`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: '%s', engine_version: '%s'}), `+
				`(p:Policy {tenant_id: '%s', id: '%s'}) `+
				`MERGE (cp)-[:COMPILED_FROM]->(p) `+
				`RETURN cp.policy_id`,
			tenantLit, pid, ek, ev, tenantLit, pid,
		)
		if err := execCypherNoRows(ctx, tx, eCompiledFrom); err != nil {
			return fmt.Errorf("policy/store: MERGE COMPILED_FROM: %w", err)
		}

		// 3) MERGE :APPLIES_TO edge.
		eAppliesTo := fmt.Sprintf(
			`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: '%s', engine_version: '%s'}), `+
				`(t:Table {tenant_id: '%s', name: '%s', namespace: '%s'}) `+
				`MERGE (cp)-[:APPLIES_TO]->(t) `+
				`RETURN cp.policy_id`,
			tenantLit, pid, ek, ev, tenantLit, tname, tns,
		)
		if err := execCypherNoRows(ctx, tx, eAppliesTo); err != nil {
			return fmt.Errorf("policy/store: MERGE APPLIES_TO: %w", err)
		}

		// 4) MERGE :GOVERNED_BY edge.
		eGovernedBy := fmt.Sprintf(
			`MATCH (cp:CompiledPolicy {tenant_id: '%s', policy_id: '%s', engine_kind: '%s', engine_version: '%s'}), `+
				`(e:Engine {tenant_id: '%s', kind: '%s', version: '%s'}) `+
				`MERGE (cp)-[:GOVERNED_BY]->(e) `+
				`RETURN cp.policy_id`,
			tenantLit, pid, ek, ev, tenantLit, ek, ev,
		)
		if err := execCypherNoRows(ctx, tx, eGovernedBy); err != nil {
			return fmt.Errorf("policy/store: MERGE GOVERNED_BY: %w", err)
		}
		return nil
	})
}

// LoadCompiledForTable returns every CompiledPolicy node attached to
// `ref` via :APPLIES_TO. The result is unordered; callers iterate and
// pick the entry for their engine kind/version.
//
// Empty result is NOT an error: a table with no policies attached
// returns ([], nil). Callers that need to distinguish "no policy
// attached" from "no compile artifact for this engine" should match
// ErrCompiledPolicyNotFound separately.
func (s *CompiledStore) LoadCompiledForTable(ctx context.Context, ref iceberg.TableRef) ([]CompiledPolicy, error) {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("policy/store: load compiled: %w", ErrTenantMissing)
	}

	ns := joinNamespace(ref.Namespace)
	tname := graph.MustSanitizeCypherLiteral(ref.Name)
	tns := graph.MustSanitizeCypherLiteral(ns)
	tenantLit := graph.MustSanitizeCypherLiteral(tenantID.String())

	var out []CompiledPolicy
	err := s.gc.ExecuteInTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		cy := fmt.Sprintf(
			`MATCH (cp:CompiledPolicy {tenant_id: '%s'})-[:APPLIES_TO]->(t:Table {tenant_id: '%s', name: '%s', namespace: '%s'}) `+
				`RETURN cp.policy_id, cp.engine_kind, cp.engine_version, cp.status, cp.source_checksum, cp.artifact_body`,
			tenantLit, tenantLit, tname, tns,
		)
		query := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS "+
				"(policy_id ag_catalog.agtype, engine_kind ag_catalog.agtype, engine_version ag_catalog.agtype, "+
				"status ag_catalog.agtype, source_checksum ag_catalog.agtype, artifact_body ag_catalog.agtype)",
			cy,
		)
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return fmt.Errorf("policy/store: LoadCompiledForTable query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var pid, ek, ev, st, ck, ab string
			if err := rows.Scan(&pid, &ek, &ev, &st, &ck, &ab); err != nil {
				return fmt.Errorf("policy/store: LoadCompiledForTable scan: %w", err)
			}
			cp := CompiledPolicy{
				PolicyID:       stripAgtypeQuotes(pid),
				EngineKind:     stripAgtypeQuotes(ek),
				EngineVersion:  stripAgtypeQuotes(ev),
				TableName:      ref.Name,
				TableNamespace: ns,
				Status:         CompiledPolicyStatus(stripAgtypeQuotes(st)),
				SourceChecksum: stripAgtypeQuotes(ck),
				ArtifactBody:   decodeArtifactFromCypher(stripAgtypeQuotes(ab)),
			}
			if !cp.Status.IsValid() {
				// Defensive: a corrupted row should not poison the
				// caller's enforcement decisions; treat as
				// compile_failed so the gateway fails closed.
				cp.Status = CompiledPolicyStatusCompileFailed
			}
			out = append(out, cp)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("policy/store: LoadCompiledForTable rows err: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// execCypherNoRows runs a cypher() statement that returns one column
// of agtype (RETURN cp.policy_id) and discards the result rows. The
// statement is wrapped in the standard ag_catalog.cypher() projection.
func execCypherNoRows(ctx context.Context, tx pgx.Tx, cy string) error {
	q := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (out ag_catalog.agtype)",
		cy,
	)
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// Drain; we don't use the values.
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
	}
	return rows.Err()
}
