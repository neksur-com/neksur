// MultiTableCommitHandler — POST /v1/iceberg/{prefix}/transactions/commit.
//
// Pitfall 6 — multi-table transaction support with Reject-All semantics.
//
// Body shape:
//
//	{
//	  "table-changes": [
//	    { "identifier": {"namespace": ["test"], "name": "orders"}, "requirements": [...], "updates": [...] },
//	    { "identifier": {"namespace": ["test"], "name": "items"},  "requirements": [...], "updates": [...] }
//	  ]
//	}
//
// Pipeline:
//   1. Tenant ctx assertion + path parse + principal extract + body
//      read + creds fetch + adapter build (steps 1-7 of CommitHandler).
//   2. For each (ref, commit) pair: LoadTable + LoadPolicies + Evaluate.
//   3. ANY policy Deny → reject the ENTIRE transaction (Reject-All):
//      emit WriteEvent {REJECTED} for the OFFENDING ref ONLY; do NOT
//      forward; do NOT emit WriteEvents for the other refs. Return 403.
//   4. ANY policy-engine-unavailable → 503 (D-1.09 fail-closed).
//   5. ALL refs PASS → forward each ref's commit sequentially. Phase 1
//      simplification: independent upstream commits (no upstream
//      multi-table tx; the upstream Iceberg REST commits are
//      independent). On ANY upstream failure, emit WriteEvents for the
//      successful prefix and log + 502 for the failed one.
//   6. Emit WriteEvent {APPROVED} for each successful ref. Return 200
//      with array of CommitResults.
//
// The "table-changes" Reject-All shape mirrors RESEARCH §Pitfall 6
// recommended semantics — multi-table transactions in Phase 1 are
// all-or-nothing at the policy gate; partial success at the upstream
// is logged but accepted (Phase 2 may add 2PC across upstream commits
// when iceberg-go ships multi-table transaction support).

package iceberg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/neksur-com/neksur/internal/catalog"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/observability"
	"github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/tenant"
)

// multiTableBody is the wire shape Spark / Trino send for a multi-table
// transactional commit. The "table-changes" key matches the Iceberg
// REST transaction commit body convention.
type multiTableBody struct {
	TableChanges []tableChange `json:"table-changes"`
}

// tableChange is one (ref, commit) pair inside a transaction.
type tableChange struct {
	Identifier   tableIdentifier            `json:"identifier"`
	Requirements []iceberg.TableRequirement `json:"requirements"`
	Updates      []iceberg.TableUpdate      `json:"updates"`
}

// tableIdentifier is the namespace + name pair as the Iceberg REST
// transaction body encodes it.
type tableIdentifier struct {
	Namespace []string `json:"namespace"`
	Name      string   `json:"name"`
}

// MultiTableCommitHandler returns the http.HandlerFunc for the
// transaction commit endpoint. Mount behind workosauth.TenantMiddleware.
//
// Per Pitfall 6 the policy gate is all-or-nothing: ANY denied table
// causes the ENTIRE transaction to fail — the offending ref is
// audit-logged as REJECTED and no upstream forwards happen (Reject-All
// semantics). This prevents the "policy bypass via multi-table commit"
// attack where a denied write hides inside a permitted batch.
func MultiTableCommitHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Step 0 — method gate.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Step 1 — tenant ctx (CC1).
		tenantID, ok := tenant.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "tenant missing", http.StatusInternalServerError)
			return
		}
		_ = tenantID // reserved for per-tx bookkeeping in Phase 2

		// Step 2 — path parse + identifier validation.
		prefix := r.PathValue("prefix")
		if !identifierRegex.MatchString(prefix) {
			http.Error(w, "malformed path identifier", http.StatusBadRequest)
			return
		}

		// Step 3 — principal extract (Pitfall 8).
		principal, principalSrc, perr := ExtractPrincipal(r)
		if perr != nil {
			http.Error(w, "unauthorized: principal missing", http.StatusUnauthorized)
			return
		}
		if err := validatePrincipalNotEmpty(principal); err != nil {
			http.Error(w, "unauthorized: principal sub empty", http.StatusUnauthorized)
			return
		}

		// Step 4 — body read with 16MB cap + hash for replay detection.
		limited := http.MaxBytesReader(w, r.Body, maxCommitBodyBytes)
		body, err := io.ReadAll(limited)
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		bodyHash := hashCommitBody(body)

		// Step 5 — body unmarshal.
		var multi multiTableBody
		if err := json.Unmarshal(body, &multi); err != nil {
			http.Error(w, "invalid multi-table body", http.StatusBadRequest)
			return
		}
		if len(multi.TableChanges) == 0 {
			http.Error(w, "multi-table body: empty table-changes", http.StatusBadRequest)
			return
		}

		// Step 6 — catalog creds fetch (RLS-scoped).
		cred, err := deps.CredStore.GetCatalogCredentials(r.Context(), prefix)
		if err != nil {
			if errors.Is(err, catalog.ErrCredentialsNotFound) {
				http.Error(w, "catalog credentials not found", http.StatusNotFound)
				return
			}
			slog.Error("gateway: multi-table creds fetch failed", "err", err, "prefix", prefix)
			http.Error(w, "catalog creds fetch failed", http.StatusInternalServerError)
			return
		}

		// Step 7 — adapter build (one adapter; all refs route through it).
		// Production: BuildAdapter; tests may inject Deps.AdapterFactory.
		adapter, err := deps.adapterFor(r.Context(), cred)
		if err != nil {
			slog.Error("gateway: multi-table adapter build failed", "err", err, "kind", cred.Kind)
			http.Error(w, "catalog adapter build failed", http.StatusInternalServerError)
			return
		}

		// Step 8 — policy gate ALL refs FIRST (Reject-All semantics per
		// Pitfall 6). Iterate every (ref, commit), load policies, evaluate;
		// stash the (ref, commit, currentMeta) for the forward step so we
		// don't re-LoadTable.
		type refCtx struct {
			Ref         iceberg.TableRef
			Commit      iceberg.CommitRequest
			CurrentMeta *iceberg.TableMetadata
		}
		approvedRefs := make([]refCtx, 0, len(multi.TableChanges))
		for _, tc := range multi.TableChanges {
			ref := iceberg.TableRef{
				Namespace: tc.Identifier.Namespace,
				Name:      tc.Identifier.Name,
			}
			// Identifier validation per ref (defence-in-depth — the
			// prefix is validated above, but namespace + name come from
			// the body which is attacker-controllable).
			if !identifierRegex.MatchString(ref.Name) {
				http.Error(w, "multi-table: malformed table identifier", http.StatusBadRequest)
				return
			}
			for _, n := range ref.Namespace {
				if !identifierRegex.MatchString(n) {
					http.Error(w, "multi-table: malformed namespace identifier", http.StatusBadRequest)
					return
				}
			}

			currentMeta, err := adapter.LoadTable(r.Context(), ref)
			if err != nil {
				switch {
				case errors.Is(err, iceberg.ErrTableNotFound):
					http.Error(w, "multi-table: table not found", http.StatusNotFound)
					return
				case errors.Is(err, iceberg.ErrCredentialsExpired):
					http.Error(w, "upstream credentials expired", http.StatusUnauthorized)
					return
				default:
					slog.Error("gateway: multi-table load failed", "err", err, "ref", ref)
					http.Error(w, "upstream load failed", http.StatusBadGateway)
					return
				}
			}

			// Policy fetch — FAIL-CLOSED for this ref.
			policies, err := deps.PolicyStore.LoadPoliciesForTable(r.Context(), ref)
			if err != nil {
				observability.CommitRejectedTotal.WithLabelValues(
					observability.ReasonPolicyEngineUnavailable).Inc()
				slog.Error("gateway: multi-table policy fetch failed (fail-closed)",
					"err", err, "ref", ref)
				http.Error(w, "policy-engine-unavailable", http.StatusServiceUnavailable)
				return
			}

			// Build the per-ref commit + evaluate.
			rc := iceberg.CommitRequest{
				Requirements: tc.Requirements,
				Updates:      tc.Updates,
			}
			inputs := &cel.Inputs{
				Table:     tableMetadataToMap(currentMeta),
				Commit:    commitRequestToMap(rc),
				Principal: principalToMap(principal),
			}
			for _, p := range policies {
				decision, err := deps.Evaluator.Evaluate(r.Context(), p, inputs)
				if err != nil {
					observability.CommitRejectedTotal.WithLabelValues(
						observability.ReasonPolicyEngineUnavailable).Inc()
					slog.Error("gateway: multi-table eval failed (fail-closed)",
						"err", err, "policy_id", p.ID, "ref", ref)
					http.Error(w, "policy-engine-unavailable", http.StatusServiceUnavailable)
					return
				}
				if decision.Action == cel.ActionDeny {
					// Pitfall 6: Reject-All — emit WriteEvent {REJECTED}
					// for the OFFENDING ref only, do NOT forward,
					// do NOT emit for the other refs.
					observability.CommitRejectedTotal.WithLabelValues(
						observability.ReasonPolicyDenied).Inc()
					if auditErr := EmitWriteEvent(r.Context(), deps.Graph, ref,
						"REJECTED", principal, principalSrc, bodyHash, nil, decision.Reason); auditErr != nil {
						slog.Error("gateway: multi-table emit audit (REJECTED) failed", "err", auditErr)
					}
					http.Error(w, "multi-table reject-all: policy denied on "+
						ref.Name+": "+decision.Reason, http.StatusForbidden)
					return
				}
			}
			approvedRefs = append(approvedRefs, refCtx{
				Ref: ref, Commit: rc, CurrentMeta: currentMeta,
			})
		}

		// Step 9 — ALL refs PASS — forward each ref's commit
		// sequentially. Phase 1 simplification: no cross-upstream
		// transaction; Iceberg REST commits are independent. On a
		// per-ref upstream failure we log + 502 (the prior successful
		// commits stay).
		results := make([]*iceberg.CommitResult, 0, len(approvedRefs))
		for _, rc := range approvedRefs {
			result, err := adapter.CommitTable(r.Context(), rc.Ref, rc.Commit)
			if err != nil {
				if errors.Is(err, iceberg.ErrCommitConflict) {
					http.Error(w, "multi-table: commit conflict on "+rc.Ref.Name,
						http.StatusConflict)
					return
				}
				slog.Error("gateway: multi-table upstream forward failed",
					"err", err, "ref", rc.Ref)
				http.Error(w, "upstream commit failed on "+rc.Ref.Name,
					http.StatusBadGateway)
				return
			}
			results = append(results, result)

			// Emit WriteEvent APPROVED for this ref.
			if auditErr := EmitWriteEvent(r.Context(), deps.Graph, rc.Ref,
				"APPROVED", principal, principalSrc, bodyHash, result, ""); auditErr != nil {
				slog.Error("gateway: multi-table emit audit (APPROVED) failed",
					"err", auditErr, "ref", rc.Ref)
			}

			// Best-effort ingest of new snapshot.
			if result != nil && result.NewMetadataLocation != "" {
				snap := iceberg.Snapshot{
					SnapshotID:       result.NewSnapshotID,
					TimestampMs:      time.Now().UnixMilli(),
					Operation:        "commit",
					MetadataLocation: result.NewMetadataLocation,
				}
				if ingErr := deps.IngestSvc.MergeSnapshot(r.Context(), tenantID.String(), snap); ingErr != nil {
					slog.Error("gateway: multi-table snapshot ingest failed (non-fatal)",
						"err", ingErr, "meta_loc", result.NewMetadataLocation)
				}
			}
		}

		// Step 10 — echo upstream responses (array of CommitResults).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}
}

// Compile-time guard: keep the context import alive even if the only
// use sites are inside the closure passed to ExecuteInTenant in audit.go.
var _ context.Context
