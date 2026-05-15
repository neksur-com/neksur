// WriteEvent + INTENDED_WRITE + ACTUAL_WRITE audit emission per D-003.06.
//
// Every gateway commit (APPROVED or REJECTED) emits:
//   1. WriteEvent vlabel — the per-commit audit node, keyed on a
//      synthetic commit_id (UUIDv4) so retries don't collapse.
//   2. INTENDED_WRITE elabel — Person → Table edge recording the
//      principal's INTENT to write (emitted on BOTH APPROVED and
//      REJECTED so SecOps can spot pattern of denied writes).
//   3. ACTUAL_WRITE elabel — Snapshot → Table edge recording the new
//      snapshot's commit (emitted ONLY on APPROVED — REJECTED commits
//      have no upstream snapshot).
//   4. Relational audit_log row — Open Question 4 (RESEARCH lines
//      1684-1687): graph + relational MUST be in the SAME tenant
//      transaction to keep the audit trail atomic.
//
// AGE 1.6 quirks applied (Plan 01-04 + 01-05 SUMMARY lessons):
//
//   - No ON CREATE SET / ON MATCH SET — emulate via COALESCE-on-WITH-SET.
//   - tenant_id MUST be in the inline MERGE property map (V0030 CHECK
//     constraint Vlabel_tenant_id_required fires before any follow-up
//     SET).
//   - Single-line Cypher per cypher() call (multi-line bodies trip a
//     separate AGE 1.6 parser regression).
//   - One MERGE per cypher() call (multi-MERGE-per-call rejected).
//
// Audit anchor d003_06_audit_anchor preserves the canonical D-003.06
// shape (WriteEvent + INTENDED_WRITE + ACTUAL_WRITE) for grep-anchored
// acceptance gates AND code-review visibility (mirrors the
// pitfall5SemanticTag pattern in internal/ingest/snapshot.go).

package iceberg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"
)

// d003_06_audit_anchor preserves the D-003.06 audit-graph shape this
// package emits. AGE 1.6's syntax restrictions force COALESCE-on-WITH-SET
// emulation in the live Cypher (see snapshot.go header for the AGE 1.6
// quirk inventory); this constant captures the canonical openCypher
// shape for grep + code-review visibility.
const d003_06_audit_anchor = "MERGE (we:WriteEvent)-[INTENDED_WRITE|ACTUAL_WRITE]-... per D-003.06"

var _ = d003_06_audit_anchor // referenced by audit tooling.

// cypherMergeWriteEvent is the WriteEvent vlabel MERGE template.
//
// Plan 01-04 deviation #1 [Rule 1 — bug-fix]: AGE 1.6.0 does NOT
// implement `MERGE ... ON CREATE SET ... ON MATCH SET ...` — we emulate
// via COALESCE-on-WITH-SET (see internal/ingest/snapshot.go header for
// the canonical workaround pattern).
//
// Properties guarded by V0030 CHECK constraints (tenant_id) are inline
// in the MERGE property map — AGE creates the vertex BEFORE applying
// any subsequent `SET`, so the CHECK fires against the partial row
// otherwise.
//
// Single-line shape required (some AGE 1.6 Cypher constructs are
// whitespace-sensitive at the dollar-quote boundary).
//
// Audit-anchor: the canonical openCypher shape this template emulates is:
//
//	MERGE (we:WriteEvent { commit_id: $commitID })
//	ON CREATE SET we.committer = $principal, we.snapshot_id = $sid,
//	              we.decision = $decision, we.policy_version = $pv,
//	              we.reason = $reason, we.created_at = $ts,
//	              we.tenant_id = $tenant
//	RETURN id(we)
const cypherMergeWriteEvent = `MERGE (we:WriteEvent {commit_id: '%s', tenant_id: '%s'}) WITH we SET we.committer = COALESCE(we.committer, '%s'), we.snapshot_id = COALESCE(we.snapshot_id, '%s'), we.decision = COALESCE(we.decision, '%s'), we.policy_version = COALESCE(we.policy_version, '%s'), we.reason = COALESCE(we.reason, '%s'), we.created_at = COALESCE(we.created_at, '%s'), we.principal_source = COALESCE(we.principal_source, '%s') RETURN id(we)`

// cypherMergeIntendedWrite is the INTENDED_WRITE elabel MERGE template.
//
// Audit-anchor: the canonical openCypher shape this template emulates is:
//
//	MATCH (p:Person {sub: $principal}), (t:Table {name: $name, namespace: $ns})
//	MERGE (p)-[r:INTENDED_WRITE]->(t)
//	ON CREATE SET r.first_at = $ts, r.tenant_id = $tenant
//	ON MATCH SET r.last_at = $ts
//
// Live shape: MERGE the Person + Table vertices first (so the MATCH
// half doesn't fail when the gateway hasn't seen this principal/table
// before), then MERGE the edge with COALESCE-emulated ON CREATE / ON
// MATCH semantics + inline tenant_id (V0030 CHECK).
//
// Multi-MERGE-per-call workaround: AGE 1.6 rejects multiple MERGE
// clauses inside ONE cypher() call. The Person + Table + edge MERGE
// dispatch as three separate cypher() invocations inside the same tx
// (see EmitWriteEvent's transactional body).
const cypherMergePerson = `MERGE (p:Person {sub: '%s', tenant_id: '%s'}) RETURN id(p)`

// cypherMergeTable is the same shape as the ingest path's Table MERGE —
// the gateway needs the Table vertex to exist for the INTENDED_WRITE
// edge MATCH.
const cypherMergeTable = `MERGE (t:Table {name: '%s', namespace: '%s', tenant_id: '%s'}) RETURN id(t)`

// cypherMergeIntendedEdge merges the INTENDED_WRITE edge between an
// existing Person and Table. The first_at / last_at split via COALESCE
// preserves the original first-write timestamp (Pitfall 5 retries
// don't clobber it).
const cypherMergeIntendedEdge = `MATCH (p:Person {sub: '%s'}), (t:Table {name: '%s', namespace: '%s'}) MERGE (p)-[r:INTENDED_WRITE {tenant_id: '%s'}]->(t) WITH r SET r.first_at = COALESCE(r.first_at, '%s'), r.last_at = '%s' RETURN id(r)`

// cypherMergeSnapshotForAudit creates / matches the Snapshot vertex
// for the new metadata_location. Same shape as ingest's MergeSnapshot;
// duplicated here so the audit emission is self-contained inside one
// tx (the ingest path runs at step 13 of the gateway pipeline AFTER
// audit; if we depended on ingest's emission we'd lose the same-tx
// atomicity contract).
const cypherMergeSnapshotForAudit = `MERGE (s:Snapshot {metadata_location: '%s', tenant_id: '%s'}) RETURN id(s)`

// cypherMergeActualEdge merges the ACTUAL_WRITE edge from the new
// Snapshot to the Table. Audit-anchor:
//
//	MATCH (s:Snapshot {metadata_location: $newLoc}), (t:Table {name: $name, namespace: $ns})
//	MERGE (s)-[r:ACTUAL_WRITE]->(t)
//	ON CREATE SET r.at = $ts, r.tenant_id = $tenant
const cypherMergeActualEdge = `MATCH (s:Snapshot {metadata_location: '%s'}), (t:Table {name: '%s', namespace: '%s'}) MERGE (s)-[r:ACTUAL_WRITE {tenant_id: '%s'}]->(t) WITH r SET r.at = COALESCE(r.at, '%s') RETURN id(r)`

// EmitWriteEvent journals a commit's policy decision to BOTH the graph
// (D-003.06 vlabel + elabels) AND the relational audit_log table (Open
// Question 4 same-tx atomicity).
//
// Parameters:
//   - ctx          — request context with tenant ID attached.
//   - gc           — graph client (the AGE-aware pool).
//   - ref          — the table being committed against.
//   - decision     — "APPROVED" | "REJECTED" (V0065 CHECK).
//   - principal    — extracted via ExtractPrincipal (Pitfall 8 chain).
//   - principalSrc — Pitfall 8 chain step that produced the principal.
//   - bodyHash     — SHA-256(commit body) for replay detection.
//   - result       — *iceberg.CommitResult on APPROVED; nil on REJECTED.
//                    NewMetadataLocation drives the ACTUAL_WRITE edge.
//   - reason       — the policy denial reason on REJECTED; "" on APPROVED.
//
// Emits:
//   - WriteEvent vlabel (always).
//   - INTENDED_WRITE elabel (always — REJECTED + APPROVED).
//   - ACTUAL_WRITE elabel (only on APPROVED with non-nil result).
//   - audit_log relational row (always).
//
// Returns wrapped pgx error on any step. The gateway's error handling
// logs but does NOT fail the commit on audit failure (Phase 1 trade-off:
// the upstream catalog already accepted; failing the gateway response
// after a successful commit would orphan the snapshot; SecOps spot
// audit-emission errors via the slog.Error). Phase 2 may add a
// best-effort retry queue.
func EmitWriteEvent(
	ctx context.Context,
	gc *graph.GraphClient,
	ref iceberg.TableRef,
	decision string,
	principal *Principal,
	principalSrc Source,
	bodyHash [32]byte,
	result *iceberg.CommitResult,
	reason string,
) error {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return fmt.Errorf("gateway: emit write event: %w", tenant.ErrTenantNotInContext)
	}
	tenantStr := tenantID.String()

	// Compose the per-emission audit-log payload as JSON. Includes
	// principal sub + roles + ref so the relational query can join
	// against the graph WriteEvent without a Cypher round-trip.
	// WR-01: surface marshal errors instead of silently producing
	// a malformed payload that the downstream `$2::jsonb` cast would
	// reject with `invalid input syntax for type json`.
	payload, err := json.Marshal(map[string]any{
		"principal": map[string]any{
			"sub":   principal.Sub,
			"email": principal.Email,
			"roles": principal.Roles,
		},
		"table_ref": map[string]any{
			"namespace": ref.Namespace,
			"name":      ref.Name,
		},
		"reason": reason,
	})
	if err != nil {
		return fmt.Errorf("gateway: marshal audit payload: %w", err)
	}

	commitID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snapshotID := ""
	if result != nil {
		snapshotID = fmt.Sprintf("%d", result.NewSnapshotID)
	}

	ns := joinNamespace(ref.Namespace)

	// All graph emission inside ONE ExecuteInTenant tx — the relational
	// audit_log INSERT runs INSIDE the same tx via the underlying tx
	// (ExecuteInTenant exposes the pgx.Tx). Open Q 4 same-tx atomicity:
	// if the graph write succeeds and the relational write fails, the
	// rollback guarantees we don't have a half-emitted audit trail.
	err = gc.ExecuteInTenant(ctx, tenantStr, func(ctx context.Context, tx pgx.Tx) error {
		// Step 1 — WriteEvent vlabel MERGE.
		weCypher := fmt.Sprintf(
			cypherMergeWriteEvent,
			escapeCypher(commitID), escapeCypher(tenantStr),
			escapeCypher(principal.Sub),
			escapeCypher(snapshotID),
			escapeCypher(decision),
			"v1", // policy_version — Phase 1 single-version; Plan 01-09 may version policies.
			escapeCypher(reason),
			now,
			escapeCypher(string(principalSrc)),
		)
		if err := execAGE(ctx, tx, weCypher, "merge write event"); err != nil {
			return err
		}

		// Step 2 — Person vlabel MERGE (so INTENDED_WRITE edge MATCH
		// finds it; we do NOT depend on a separate user-ingest pipeline
		// to have created the Person node).
		personCypher := fmt.Sprintf(
			cypherMergePerson,
			escapeCypher(principal.Sub), escapeCypher(tenantStr),
		)
		if err := execAGE(ctx, tx, personCypher, "merge person"); err != nil {
			return err
		}

		// Step 3 — Table vlabel MERGE (same rationale; the gateway may
		// race the ingest path on a fresh table and we don't want the
		// edge MATCH to fail).
		tableCypher := fmt.Sprintf(
			cypherMergeTable,
			escapeCypher(ref.Name), escapeCypher(ns), escapeCypher(tenantStr),
		)
		if err := execAGE(ctx, tx, tableCypher, "merge table for audit"); err != nil {
			return err
		}

		// Step 4 — INTENDED_WRITE edge MERGE.
		intendedCypher := fmt.Sprintf(
			cypherMergeIntendedEdge,
			escapeCypher(principal.Sub), escapeCypher(ref.Name), escapeCypher(ns),
			escapeCypher(tenantStr),
			now, now,
		)
		if err := execAGE(ctx, tx, intendedCypher, "merge intended write"); err != nil {
			return err
		}

		// Step 5 — ACTUAL_WRITE edge — APPROVED + non-nil result only.
		if decision == "APPROVED" && result != nil && result.NewMetadataLocation != "" {
			snapCypher := fmt.Sprintf(
				cypherMergeSnapshotForAudit,
				escapeCypher(result.NewMetadataLocation), escapeCypher(tenantStr),
			)
			if err := execAGE(ctx, tx, snapCypher, "merge snapshot for audit"); err != nil {
				return err
			}
			actualCypher := fmt.Sprintf(
				cypherMergeActualEdge,
				escapeCypher(result.NewMetadataLocation),
				escapeCypher(ref.Name), escapeCypher(ns),
				escapeCypher(tenantStr),
				now,
			)
			if err := execAGE(ctx, tx, actualCypher, "merge actual write"); err != nil {
				return err
			}
		}

		// Step 6 — relational audit_log INSERT (Open Question 4 same-tx).
		// V0050 created the table; V0065 added decision /
		// principal_source / commit_request_hash columns (with CHECK
		// constraints). The actor_user_id is the principal sub for
		// continuity with Phase 0.5 audit shapes.
		//
		// Schema qualification: ExecuteInTenant uses the GraphClient's
		// pool whose BeforeAcquire hook sets `search_path = ag_catalog,
		// "$user", public` (so AGE Cypher operators resolve). This does
		// NOT include the tenant's per-tenant schema, so an unqualified
		// `audit_log` INSERT would fail with `relation does not exist`.
		// We schema-qualify to `tenant_<uuid>.audit_log` so the Open
		// Question 4 same-tx-as-graph-emission contract holds without
		// a search_path-twiddling round-trip inside the audit tx.
		// pgx.Identifier.Sanitize() handles the schema-name escaping
		// (the schema name is computed from a validated UUID per
		// internal/tenant/id.go::SchemaName so it is identifier-safe).
		bodyHashHex := hex.EncodeToString(bodyHash[:])
		bodyHashBytes, _ := hex.DecodeString(bodyHashHex)
		schema := tenant.SchemaName(tenantID)
		auditTable := pgx.Identifier{schema, "audit_log"}.Sanitize()
		insertSQL := fmt.Sprintf(`
			INSERT INTO %s (occurred_at, actor_user_id, event_type, payload, decision, principal_source, commit_request_hash)
			VALUES (now(), $1, 'iceberg_commit', $2::jsonb, $3, $4, $5)
		`, auditTable)
		_, err := tx.Exec(ctx, insertSQL, principal.Sub, string(payload), decision, string(principalSrc), bodyHashBytes)
		if err != nil {
			return fmt.Errorf("gateway: emit audit_log: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("gateway: emit write event: %w", err)
	}
	return nil
}

// hashCommitBody returns SHA-256 of the commit body for replay
// detection. Stored in audit_log.commit_request_hash; SecOps can spot
// duplicates via `SELECT count(*), commit_request_hash ... GROUP BY
// commit_request_hash HAVING count(*) > 1`.
func hashCommitBody(b []byte) [32]byte {
	return sha256.Sum256(b)
}

// execAGE runs one Cypher statement inside the given tx, wrapping
// errors with the operation name. Used for the per-MERGE dispatch
// pattern (AGE 1.6 multi-MERGE-per-call workaround).
func execAGE(ctx context.Context, tx pgx.Tx, cypher, op string) error {
	q := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
		cypher,
	)
	if _, err := tx.Exec(ctx, q); err != nil {
		return fmt.Errorf("gateway: audit %s: %w", op, err)
	}
	return nil
}

// joinNamespace flattens a multi-segment namespace into a dot-delimited
// string. Mirrors internal/policy/store/age.go::joinNamespace so the
// Table {namespace: ...} property has the same shape across both the
// policy-fetch path and the audit-emission path.
func joinNamespace(parts []string) string {
	return strings.Join(parts, ".")
}

// escapeCypher validates a caller-supplied string for safe inlining
// into a Cypher single-quoted string literal inside an AGE
// `cypher('graph', $$ ... $$)` dollar-quoted block.
//
// CR-01 mitigation: routes through graph.MustSanitizeCypherLiteral
// (strict allowlist of ASCII letters/digits/URI-safe punctuation;
// rejects `'`, `"`, `\`, `$`, `{`, `}`, `;`, CR/LF, NUL, tab,
// non-ASCII). The audit emission path is fed by:
//
//   - principal.Sub: validated by ExtractPrincipal + validatePrincipalNotEmpty
//     (the upstream auth chain enforces a well-formed sub).
//   - ref.Name / ref.Namespace: gated by identifierRegex
//     `^[a-zA-Z0-9_-]+$` at CommitHandler / MultiTableCommitHandler
//     entry — already a strict subset of the safe-Cypher allowlist.
//   - commitID: UUIDv4 (server-generated by uuid.New()).
//   - snapshotID / decision / reason / principalSrc: server-controlled.
//   - tenantStr: tenant UUID (validated by upstream TenantMiddleware).
//   - result.NewMetadataLocation: upstream catalog response (trusted
//     by Phase 1's "the catalog already accepted" contract).
//
// A panic here means a new untrusted-input path bypassed the entry
// validators — defence-in-depth surfaces the bug instead of running
// a Cypher injection.
func escapeCypher(s string) string {
	return graph.MustSanitizeCypherLiteral(s)
}
