// POST /v1/lineage — OpenLineage v2 RunEvent HTTP receiver.
//
// Pipeline (RESEARCH §Pattern 4 lines 793-825):
//
//  1. Method gate — only POST accepted; everything else → 405.
//  2. Tenant context assertion (CC1 per PATTERNS line 19) — the
//     handler MUST mount behind `workosauth.TenantMiddleware`. If
//     tenant ctx is missing, return 500 "tenant missing" (defensive —
//     normal request paths can never produce this).
//  3. Body decode with 1 MiB cap (http.MaxBytesReader, RESEARCH line
//     809) — bounds OOM exposure via T-1-large-payload-handler-oom.
//  4. Validate required fields → 400 on missing.
//  5. **Pitfall 5 — at-least-once durability:** INSERT into the
//     per-tenant `lineage_inbox` table FIRST, with ON CONFLICT
//     (producer, run_id) DO NOTHING. Spark's OpenLineage HTTP
//     transport retries on transient failures; the UNIQUE constraint
//     (V0063) swallows duplicates so the consumer worker never
//     MERGEs the same event twice.
//  6. Translate Inputs/Outputs to LINEAGE_OF MERGE calls via the
//     ingest.Service. On *LineageCycleError → 422; on other errors → 503.
//  7. Success → 202 Accepted (at-least-once semantics; the event is
//     durably persisted regardless of MERGE outcome).

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/ingest"
	"github.com/neksur-com/neksur/internal/tenant"
)

// maxBodyBytes is the 1 MiB body cap (RESEARCH line 809). Larger
// payloads return 400 BEFORE any allocation — bounds T-1-large-
// payload-handler-oom DoS exposure. 1 MiB comfortably accommodates a
// typical OpenLineage RunEvent with ~50 datasets + facets; pipelines
// emitting larger events should split runs.
const maxBodyBytes = 1 << 20

// Handler constructs the POST /v1/lineage http.HandlerFunc. The
// handler depends on:
//
//   - pool: the Phase 0.5 pgxpool.Pool with BeforeAcquire DISCARD ALL.
//     The lineage_inbox INSERT runs through `tenant.WithTenantTx` so
//     the per-request transaction applies search_path + role +
//     app.current_tenant GUC (D-0.5.03 three-layer isolation).
//
//   - ing: the ingest.Service for the LINEAGE_OF MERGE pipeline.
//     The Service has its own pgxpool via graph.GraphClient — those
//     two pools share the same Postgres backend but have different
//     AfterConnect / BeforeAcquire wiring (the graph pool LOADs AGE).
//
// Mount under `workosauth.TenantMiddleware` so tenant.IDFromContext
// returns the resolved tenant; the handler asserts but does not
// fabricate the tenant ID (T-1-openlineage-spoof mitigation).
func Handler(pool *pgxpool.Pool, ing *ingest.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Step 1 — method gate.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Step 2 — tenant ctx (CC1). Belt-and-suspenders: TenantMiddleware
		// is the wire-layer gate, but a misconfigured route mounting the
		// handler outside the middleware would otherwise silently 200.
		tenantID, ok := tenant.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "tenant missing", http.StatusInternalServerError)
			return
		}

		// Step 3 — body decode with 1 MiB cap (Pitfall: T-1-large-
		// payload-handler-oom). Read the raw bytes once so we can both
		// parse + persist the original JSON in lineage_inbox.payload.
		limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
		rawBody, err := io.ReadAll(limited)
		if err != nil {
			http.Error(w, "invalid OpenLineage payload", http.StatusBadRequest)
			return
		}
		var event RunEvent
		if err := json.Unmarshal(rawBody, &event); err != nil {
			http.Error(w, "invalid OpenLineage payload", http.StatusBadRequest)
			return
		}

		// Step 4 — validation.
		if err := event.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Step 5 — Pitfall 5 durability: INSERT into lineage_inbox
		// FIRST with ON CONFLICT DO NOTHING. The UNIQUE (producer,
		// run_id) constraint (V0063) swallows duplicates.
		if err := persistInbox(r.Context(), pool, rawBody, event); err != nil {
			http.Error(w, "lineage inbox persist failed", http.StatusServiceUnavailable)
			return
		}

		// Step 6 — translate to LINEAGE_OF MERGE calls. For each
		// (input, output) pair, MERGE one edge. We deliberately do
		// NOT make this transactional across pairs — partial failure
		// on a downstream MERGE doesn't invalidate the inbox row, and
		// the consumer worker can re-drive from the inbox on restart
		// if needed. Cycles are detected per-edge so the operator
		// can pinpoint which (input, output) is the offender.
		ts, err := time.Parse(time.RFC3339Nano, event.EventTime)
		if err != nil {
			ts = time.Now().UTC()
		}
		for _, in := range event.Inputs {
			for _, out := range event.Outputs {
				srcURI := in.URI()
				tgtURI := out.URI()
				if err := ing.MergeLineageEdge(
					r.Context(),
					tenantID.String(),
					srcURI,
					tgtURI,
					event.Run.RunID,
					ts,
				); err != nil {
					// Cycle → 422 Unprocessable Entity (RESEARCH line 815).
					// Other errors → 503 (RESEARCH line 820).
					var cyc *ingest.LineageCycleError
					if errors.As(err, &cyc) {
						http.Error(w, cyc.Error(), http.StatusUnprocessableEntity)
						return
					}
					http.Error(w, "lineage merge failed", http.StatusServiceUnavailable)
					return
				}
			}
		}

		// Step 7 — 202 Accepted (at-least-once semantics; RESEARCH line 823).
		w.WriteHeader(http.StatusAccepted)
	}
}

// persistInbox INSERTs the raw OpenLineage payload into the per-tenant
// lineage_inbox table. ON CONFLICT (producer, run_id) DO NOTHING is
// the Pitfall 5 dedup hook — Spark retries land here as a no-op
// instead of cascading into double-MERGE.
//
// Runs inside tenant.WithTenantTx so the search_path + role + GUC
// layers are applied — the INSERT lands in the correct tenant_<uuid>
// schema and the audit trail in V0066 RLS predicate sees the right
// tenant.
func persistInbox(ctx context.Context, pool *pgxpool.Pool, rawBody []byte, event RunEvent) error {
	return tenant.WithTenantTx(ctx, pool, func(tx pgx.Tx) error {
		// Pre-encode rawBody so we pass it as a single $4 jsonb cast
		// rather than concatenating in SQL.
		_, err := tx.Exec(ctx, `
			INSERT INTO lineage_inbox (producer, run_id, event_type, payload)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (producer, run_id) DO NOTHING
		`, event.Producer, event.Run.RunID, event.EventType, string(rawBody))
		if err != nil {
			return fmt.Errorf("lineage inbox insert: %w", err)
		}
		return nil
	})
}
