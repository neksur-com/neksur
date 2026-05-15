// Unit tests (no integration build tag) — exercise the handler's
// pre-IO rejection paths (method gate, body cap, validation, CR-01
// Cypher-injection guard). These tests do not touch the pool or the
// ingest.Service because the rejection paths return early before any
// IO. The integration-tagged tests in handler_test.go cover the
// happy-path persistence + MERGE flow.

package http

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/neksur-com/neksur/internal/tenant"
)

// TestHandler_RejectsCypherInjectionInDatasetURI exercises the
// REVIEW.md CR-01 fix: a Dataset URI carrying Cypher-injection
// vector characters (single quote, semicolon, etc.) MUST be
// rejected with 400 BEFORE the inbox INSERT and BEFORE
// MergeLineageEdge is called. The handler returns early on the
// first unsafe URI so we don't need a real pool/ingest service
// (passing nil is safe — the rejection happens before they are
// touched).
func TestHandler_RejectsCypherInjectionInDatasetURI(t *testing.T) {
	// Canonical CR-01 attack payload from REVIEW.md:
	// "evil://x', tenant_id: 'victim-tenant-uuid'}) RETURN id(src);
	//   MATCH (n) DETACH DELETE n; //"
	// We embed it in a structurally-valid OpenLineage event.
	cases := []struct {
		name      string
		body      string
		wantOn    string // expect this string in the 400 body
	}{
		{
			name: "input_dataset_uri_with_single_quote",
			body: `{
				"eventType":"COMPLETE",
				"eventTime":"2026-05-15T00:00:00Z",
				"producer":"spark/3.5.0",
				"run":{"runId":"r1"},
				"job":{"namespace":"jobs","name":"j1"},
				"inputs":[{"namespace":"evil","name":"x', tenant_id: 'victim'"}],
				"outputs":[{"namespace":"iceberg","name":"prod.t"}]
			}`,
			wantOn: "unsafe input dataset URI",
		},
		{
			name: "output_dataset_uri_with_semicolon",
			body: `{
				"eventType":"COMPLETE",
				"eventTime":"2026-05-15T00:00:00Z",
				"producer":"spark/3.5.0",
				"run":{"runId":"r1"},
				"job":{"namespace":"jobs","name":"j1"},
				"inputs":[{"namespace":"iceberg","name":"prod.t"}],
				"outputs":[{"namespace":"evil","name":"x; MATCH (n) DELETE n"}]
			}`,
			wantOn: "unsafe output dataset URI",
		},
		{
			name: "output_dataset_uri_with_dollar_sign",
			body: `{
				"eventType":"COMPLETE",
				"eventTime":"2026-05-15T00:00:00Z",
				"producer":"spark/3.5.0",
				"run":{"runId":"r1"},
				"job":{"namespace":"jobs","name":"j1"},
				"inputs":[{"namespace":"iceberg","name":"prod.t"}],
				"outputs":[{"namespace":"evil","name":"x$$end"}]
			}`,
			wantOn: "unsafe output dataset URI",
		},
		{
			name: "run_id_with_double_quote",
			body: `{
				"eventType":"COMPLETE",
				"eventTime":"2026-05-15T00:00:00Z",
				"producer":"spark/3.5.0",
				"run":{"runId":"r\"id"},
				"job":{"namespace":"jobs","name":"j1"},
				"inputs":[{"namespace":"iceberg","name":"prod.t"}],
				"outputs":[{"namespace":"iceberg","name":"prod.u"}]
			}`,
			wantOn: "unsafe run id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/lineage", strings.NewReader(tc.body))
			// Attach a tenant ID so Step 2 (tenant ctx assertion) passes.
			ctx := tenant.WithID(context.Background(), uuid.New())
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()

			// Pool + ingest service are nil — the CR-01 rejection
			// happens BEFORE they are touched (the handler returns
			// 400 at Step 4b).
			h := Handler(nil, nil)
			h(rec, req)

			if rec.Code != 400 {
				t.Fatalf("status = %d; want 400. Body: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantOn) {
				t.Errorf("body = %q; want substring %q", rec.Body.String(), tc.wantOn)
			}
		})
	}
}

// TestHandler_AcceptsBenignURIs is the negative companion to the
// CR-01 rejection test: a valid OpenLineage Dataset URI (no
// Cypher-unsafe characters) should NOT be rejected by the guard.
// We don't reach the inbox INSERT here (pool=nil) so we expect a
// downstream nil-pointer panic OR an inbox-persist error — but NOT a
// 400 from the URI guard. We trap the panic and just confirm the
// guard didn't fire (rec.Code != 400 with our "unsafe...URI" body).
func TestHandler_GuardAcceptsBenignURIs(t *testing.T) {
	defer func() {
		// pool=nil will panic when persistInbox is reached. That is
		// PROOF the URI guard passed (we got past Step 4b). Swallow
		// the panic — this test is only confirming the guard is not
		// over-rejecting valid input.
		_ = recover()
	}()
	body := `{
		"eventType":"COMPLETE",
		"eventTime":"2026-05-15T00:00:00Z",
		"producer":"spark/3.5.0",
		"run":{"runId":"a1b2c3d4-e5f6-4789-8abc-def012345678"},
		"job":{"namespace":"jobs","name":"j1"},
		"inputs":[{"namespace":"iceberg","name":"prod.sales.orders"}],
		"outputs":[{"namespace":"s3","name":"bucket/path/file.parquet"}]
	}`
	req := httptest.NewRequest("POST", "/v1/lineage", strings.NewReader(body))
	ctx := tenant.WithID(context.Background(), uuid.New())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h := Handler(nil, nil)
	h(rec, req)

	// Either we panicked (recovered above) or we got past the guard.
	// Confirm we did NOT get the 400-with-"unsafe URI" response.
	if rec.Code == 400 && strings.Contains(rec.Body.String(), "unsafe") {
		t.Errorf("benign URIs over-rejected by CR-01 guard. status=%d body=%q",
			rec.Code, rec.Body.String())
	}
}
