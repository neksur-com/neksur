//go:build integration

// Plan 01-04 Task 3 [BLOCKING] — OpenLineage v2 receiver round-trip +
// at-least-once dedup (Pitfall 5).
//
// Wires the POST /v1/lineage handler INSIDE a httptest.NewServer with
// a thin TenantMiddleware shim (tenant.WithID injected by hand — the
// full WorkOS JWKS rotation path is already proven by
// workos_session_test.go; here we only need the tenant ctx). Then:
//
//   1. POST a valid RunEvent → assert 202 Accepted.
//   2. POST IDENTICAL body again (simulating Spark retry) → assert 202.
//   3. SELECT count(*) FROM lineage_inbox WHERE producer=$1 AND run_id=$2
//      → assert exactly 1 row (Pitfall 5 UNIQUE constraint dedup).
//   4. MATCH ()-[r:LINEAGE_OF]->() RETURN count(r) → assert 1 edge
//      (MERGE idempotency across retries).

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/ingest"
	lineagehttp "github.com/neksur-com/neksur/internal/lineage/http"
	"github.com/neksur-com/neksur/internal/tenant"
)

// TestOpenLineageConsumerIdempotent — the BLOCKING gate for Pitfall 5.
//
// Sends the same OpenLineage payload twice; asserts the lineage_inbox
// holds exactly one row and the LINEAGE_OF graph edge count is 1.
func TestOpenLineageConsumerIdempotent(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantStr = "99999999-9999-4999-9999-999999999999"
	tenantUUID := uuid.MustParse(tenantStr)
	_ = fx.ProvisionTenant(t, tenantStr)

	// Pool for the lineage_inbox INSERT path (tenant.WithTenantTx).
	// MUST have the BeforeAcquire DISCARD ALL hook so session bleed
	// across acquisitions doesn't pollute the test (Pitfall 5 / T-0-SESS).
	pool := newTenantPool(t, fx.Container.SuperuserDSN)
	defer pool.Close()

	// GraphClient for the LINEAGE_OF MERGE path. Separate pool — same
	// Postgres backend, different AfterConnect / BeforeAcquire wiring.
	gc, err := graph.NewGraphClient(fx.ctx, fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()
	svc := ingest.NewService(gc)

	// Seed the Table nodes the LINEAGE_OF MERGE expects.
	uriIn := "iceberg://src"
	uriOut := "iceberg://tgt"
	seedTableNodes(t, fx.ctx, gc, tenantStr, []string{uriIn, uriOut})

	// Wire the handler behind a minimal TenantMiddleware that injects
	// the resolved tenant ID directly. The full WorkOS path is proven
	// by workos_session_test.go; the wire-layer correctness here is
	// the at-least-once durability + cycle handling.
	handler := lineagehttp.Handler(pool, svc)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := tenant.WithID(r.Context(), tenantUUID)
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	// Build a valid RunEvent. The producer + run_id together form the
	// lineage_inbox UNIQUE key.
	event := lineagehttp.RunEvent{
		EventType: "COMPLETE",
		EventTime: "2026-05-15T12:00:00Z",
		Producer:  "spark/3.5.0",
		Run:       lineagehttp.Run{RunID: "run-idempotency-test"},
		Job:       lineagehttp.Job{Namespace: "etl", Name: "orders_pipeline"},
		Inputs:    []lineagehttp.Dataset{{Namespace: "iceberg", Name: "src"}},
		Outputs:   []lineagehttp.Dataset{{Namespace: "iceberg", Name: "tgt"}},
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	// First POST.
	status, respBody := postLineage(t, srv.URL, body)
	if status != http.StatusAccepted {
		t.Fatalf("first POST status %d; expected 202. body=%s", status, respBody)
	}

	// Second POST — identical body (Spark retry).
	status2, respBody2 := postLineage(t, srv.URL, body)
	if status2 != http.StatusAccepted {
		t.Fatalf("second POST status %d; expected 202 (idempotent retry). body=%s", status2, respBody2)
	}

	// Assert lineage_inbox dedup — UNIQUE (producer, run_id) collapses
	// the duplicate to a single row.
	inboxCount := countInbox(t, fx.ctx, pool, tenantUUID, event.Producer, event.Run.RunID)
	if inboxCount != 1 {
		t.Errorf("lineage_inbox count = %d; expected 1 (Pitfall 5 dedup)", inboxCount)
	}

	// Assert LINEAGE_OF edge count — MERGE idempotency in the graph.
	edges := countMatching(t, fx.ctx, gc, tenantStr,
		fmt.Sprintf("MATCH (s {iceberg_id: '%s'})-[r:LINEAGE_OF]->(t {iceberg_id: '%s'}) RETURN count(r)", uriIn, uriOut))
	if edges != 1 {
		t.Errorf("LINEAGE_OF edge count = %d; expected 1 (MERGE idempotency)", edges)
	}
}

// --- helpers ----------------------------------------------------------

// newTenantPool builds a pgxpool wired the same way as cmd/neksur-server's
// production pool: BeforeAcquire DISCARD ALL (T-0-SESS) + describe_exec
// (avoids prepared-statement cache issues with DISCARD ALL).
//
// This is the pool tenant.WithTenantTx consumes inside Handler →
// persistInbox.
func newTenantPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	graph.WithBeforeAcquireDiscardAll(cfg)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// LOAD age + search_path so any future Cypher-touching test
		// helpers on this pool work. The persistInbox path itself
		// does not need AGE, but the AfterConnect runs once per
		// physical conn so the cost is bounded.
		if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	return pool
}

// postLineage POSTs body to url and returns (status, responseBody).
func postLineage(t *testing.T, url string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// countInbox SELECTs count(*) from the per-tenant lineage_inbox table.
// Runs inside tenant.WithTenantTx so search_path resolves to the
// tenant_<uuid> schema.
func countInbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantUUID uuid.UUID, producer, runID string) int64 {
	t.Helper()
	tctx := tenant.WithID(ctx, tenantUUID)
	var n int64 = -1
	err := tenant.WithTenantTx(tctx, pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			"SELECT count(*) FROM lineage_inbox WHERE producer = $1 AND run_id = $2",
			producer, runID,
		)
		return row.Scan(&n)
	})
	if err != nil {
		t.Logf("countInbox WithTenantTx error: %v", err)
		return -1
	}
	return n
}
