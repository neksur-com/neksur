// CompiledPolicy lifecycle docs — D-2.04 / Plan 02-04.
//
// This file holds only the package-level documentation for the
// CompiledPolicy surface (UpsertCompiledPolicy + LoadCompiledForTable
// in compiled.go, and the EngineRegistry reader in engine.go). The
// existing doc.go in this package (currently absent — see age.go's
// package doc) covers the original P1/P2/P3 LoadPoliciesForTable
// path; this file extends that with the Phase 2 cross-engine compile
// pipeline.
//
// =====================================================================
// CompiledPolicy lifecycle (D-2.04 + D-2.08)
// =====================================================================
//
// 1) Source policy authored.
//    Customer creates a Policy node (kind ∈ {row_filter, column_mask,
//    abac, classification, residency, partition, schema, write_acl,
//    retention}). The Policy carries:
//      - definition_cel   — CEL source for ABAC / three-layer policies
//      - definition_sql   — SQL fragment for row-filter / column-mask
//    Exactly one of the two source columns is non-empty per policy.
//
// 2) Compile dispatch.
//    The cross-engine compiler (internal/policy/compiler) loops the
//    tenant's `public.engines` registry. For each (engine_kind,
//    engine_version), it dispatches to the per-dialect emitter
//    (trino.go / spark.go / dremio.go [stub]).
//
// 3) Compile result.
//    - SQL fragment: parsed by sql_grammar.go into a Fragment AST,
//      then lowered by the dialect emitter to engine-native SQL.
//    - CEL: compiled by cel_artifact.go into a CELArtifact (source +
//      binding manifest + checksum), serialized as JSON.
//    Either way, the result is stored in artifact_body.
//
// 4) UpsertCompiledPolicy.
//    The store writes (or updates) the CompiledPolicy node + the
//    three edges (COMPILED_FROM, APPLIES_TO, GOVERNED_BY) with
//    status=pending. Idempotent across re-runs.
//
// 5) Probe.
//    For dialect targets that have a live ProbeExecutor registered
//    (Trino in Phase 2), ProbeRunner.Run submits the synthetic
//    `SELECT 1 FROM <table> WHERE <fragment> AND 1=0` query against
//    the engine. Success → status=active. Failure → status=
//    probe_failed (artifact_body preserved for triage). 5s timeout
//    via context.WithTimeout.
//
// 6) Enforcement.
//    The gateway (Plan 02-05+) reads CompiledPolicy via
//    LoadCompiledForTable and routes commits against the matching
//    (table, engine) artifact. Only `active` rows are enforced; any
//    other status fails closed.
//
// =====================================================================
// Status transitions (state machine)
// =====================================================================
//
//   (no node)
//      │
//      │ compile success
//      ▼
//   pending  ─── probe success ──▶  active
//      │
//      │ probe failure / engine 5xx / timeout
//      ▼
//   probe_failed (terminal until recompile)
//
//   (no node)
//      │
//      │ dialect emitter ErrDialectStub / parse failure
//      ▼
//   compile_failed (terminal until source edit)
//
// =====================================================================
// Cypher injection defence (CR-01)
// =====================================================================
//
// Every spliced literal in compiled.go's MERGE statements is routed
// through graph.MustSanitizeCypherLiteral. The artifact_body itself
// (which may contain `{`, `}`, `;`, etc. for JSON / SQL syntax) is
// base64-encoded before the splice via the encodeArtifactForCypher /
// decodeArtifactFromCypher helpers in this package — keeping the
// on-wire literal inside the safe-Cypher allowlist.
//
// =====================================================================
// Why the artifact body is base64-encoded
// =====================================================================
//
// graph.MustSanitizeCypherLiteral's allowlist is intentionally narrow
// (printable ASCII letters/digits + URI-safe punctuation). JSON-encoded
// CELArtifacts contain `{`, `}`, `"`, `\` — all rejected by the
// allowlist. SQL fragments contain operators and parentheses — the
// parentheses are in the allowlist but the operators (`<`, `>`, `|`)
// are not.
//
// Rather than relax the allowlist (which would weaken CR-01), we
// base64-encode the body before storage and decode on read. The
// encoding alphabet is `[A-Za-z0-9+/=]` — all in the allowlist. No
// loss of fidelity, no allowlist relaxation.

package store

import "encoding/base64"

// encodeArtifactForCypher base64-encodes the artifact body so the
// resulting string contains only safe-Cypher allowlist characters.
// Empty input round-trips to empty output.
func encodeArtifactForCypher(s string) string {
	if s == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// decodeArtifactFromCypher reverses encodeArtifactForCypher. Returns
// the empty string on decode failure — the caller (LoadCompiledForTable)
// will surface this as CompiledPolicyStatusCompileFailed via the
// validity check, so the gateway fails closed on a corrupted row.
func decodeArtifactFromCypher(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}
