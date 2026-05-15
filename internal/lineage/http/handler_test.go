//go:build integration

// Integration tests for POST /v1/lineage. These tests build on
// StartPhase1Fixture (tests/integration/phase1_fixtures.go) so they
// have a real Postgres+AGE+V0063 lineage_inbox table + tenant
// provisioning + an ingest.Service backed by a real graph client.
//
// The end-to-end test (TestHandlerAcceptsValidRunEvent +
// TestHandlerDedupsRetry) lives under tests/integration/ alongside
// the rest of the BLOCKING gates (see openlineage_consumer_test.go).
// What you'll find here is the in-package shape tests that don't
// need a container:
//
//   - TestRunEventValidateRejectsMissingFields
//   - TestDatasetURIFormat
//
// The wire-level shape tests in tests/integration/ require the
// pgxpool + ingest.Service + a fully-wired TenantMiddleware path; the
// handler is essentially trivial in its non-IO branches, so keeping
// most coverage in the integration tier is intentional.

package http

import (
	"errors"
	"strings"
	"testing"
)

func TestRunEventValidateRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		ev   RunEvent
	}{
		{"empty", RunEvent{}},
		{"missing event type", RunEvent{Run: Run{RunID: "r1"}, Producer: "spark"}},
		{"missing run id", RunEvent{EventType: "COMPLETE", Producer: "spark"}},
		{"missing producer", RunEvent{EventType: "COMPLETE", Run: Run{RunID: "r1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ev.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil; expected ErrInvalidPayload")
			}
			if !errors.Is(err, ErrInvalidPayload) {
				t.Errorf("errors.Is(err, ErrInvalidPayload) = false; err = %v", err)
			}
		})
	}
}

func TestRunEventValidateAcceptsMinimal(t *testing.T) {
	ev := RunEvent{
		EventType: "COMPLETE",
		Run:       Run{RunID: "run-1"},
		Producer:  "spark/3.5.0",
	}
	if err := ev.Validate(); err != nil {
		t.Errorf("Validate() = %v; expected nil for minimal valid event", err)
	}
}

func TestDatasetURIFormat(t *testing.T) {
	d := Dataset{Namespace: "iceberg", Name: "prod.sales.orders"}
	got := d.URI()
	want := "iceberg://prod.sales.orders"
	if got != want {
		t.Errorf("Dataset.URI() = %q; want %q", got, want)
	}
}

func TestErrInvalidPayloadMessage(t *testing.T) {
	// Sanity-check the error message format so callers can string-match
	// in error-handling code without depending on internal text.
	if !strings.Contains(ErrInvalidPayload.Error(), "OpenLineage") {
		t.Errorf("ErrInvalidPayload.Error() should mention OpenLineage; got %q",
			ErrInvalidPayload.Error())
	}
}
