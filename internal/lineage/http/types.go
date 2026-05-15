// Package http exposes the OpenLineage v2 HTTP receiver at
// POST /v1/lineage. Only the subset of the OpenLineage v2 RunEvent
// schema that Phase 1 ingests is typed here; the consumer worker
// (Plan 01-04 cycle_sweep + Plan 01-06 gateway) reads the durable
// lineage_inbox row to access the FULL payload as jsonb.
//
// Per RESEARCH §Pattern 4 lines 827-841 the typed subset covers:
//   - eventType (START / RUNNING / COMPLETE / ABORT / FAIL)
//   - run.runId  (the OpenLineage run identifier — UNIQUE key on the inbox)
//   - producer   (the upstream tool emitting events — Spark, Flink, etc.)
//   - inputs / outputs (the source + target datasets — each Dataset
//     translates to a graph node URI via Namespace + Name).
//
// The full RunEvent schema is at https://openlineage.io/docs/spec; we
// deliberately do NOT pull in OpenLineage's official Go bindings —
// those bring 50+ optional facets we don't ingest in Phase 1, and a
// stable Phase-1-narrow typed subset is easier to evolve.

package http

import (
	"errors"
	"fmt"
)

// RunEvent is the Phase 1 OpenLineage v2 RunEvent shape. Optional
// fields (facets, schema URLs, parent runs) are deliberately omitted —
// Phase 1 ingests Inputs/Outputs only; the original JSON body lands
// in lineage_inbox.payload so Phase 2 / Phase 3 enrichment can read
// the full facet set without breaking the wire contract.
type RunEvent struct {
	EventType string    `json:"eventType"` // START / RUNNING / COMPLETE / ABORT / FAIL
	EventTime string    `json:"eventTime"` // RFC 3339
	Producer  string    `json:"producer"`  // e.g. "spark/3.5.0" — UNIQUE inbox key prefix
	SchemaURL string    `json:"schemaURL"` // OpenLineage spec URL
	Run       Run       `json:"run"`
	Job       Job       `json:"job"`
	Inputs    []Dataset `json:"inputs"`
	Outputs   []Dataset `json:"outputs"`
}

// Run is the OpenLineage run identifier carrier. RunID is the natural
// dedup key — paired with Producer it forms the UNIQUE constraint on
// the lineage_inbox table (V0063) per Pitfall 5.
type Run struct {
	RunID string `json:"runId"` // UUID-shaped per OpenLineage spec
}

// Job is the OpenLineage job descriptor — namespace + name identify
// the logical pipeline producing the event.
type Job struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Dataset is one Iceberg-or-other dataset referenced by the run.
// URI() returns the canonical graph-node URI form `<namespace>://<name>`.
type Dataset struct {
	Namespace string `json:"namespace"` // e.g. "iceberg"
	Name      string `json:"name"`      // e.g. "prod.sales.orders"
}

// URI returns the canonical Phase 1 graph-node URI for this dataset.
// Format: `<namespace>://<name>`. Used as the `iceberg_id` property
// lookup key in the LINEAGE_OF MERGE template (internal/ingest/lineage.go).
func (d Dataset) URI() string {
	return d.Namespace + "://" + d.Name
}

// ErrInvalidPayload is returned by RunEvent.Validate() when a required
// field is missing or malformed. Maps to HTTP 400 at the handler boundary.
var ErrInvalidPayload = errors.New("lineage: invalid OpenLineage payload")

// Validate checks the minimal required-field set for Phase 1
// processing. EventType, Run.RunID, and Producer are mandatory — they
// drive the lineage_inbox UNIQUE constraint + cycle pre-check shape.
// Inputs/Outputs MAY be empty (some OpenLineage events report only
// run-state transitions); the handler skips MERGE in that case.
//
// Returns ErrInvalidPayload wrapped with a per-field message.
func (e RunEvent) Validate() error {
	if e.EventType == "" {
		return fmt.Errorf("eventType required: %w", ErrInvalidPayload)
	}
	if e.Run.RunID == "" {
		return fmt.Errorf("run.runId required: %w", ErrInvalidPayload)
	}
	if e.Producer == "" {
		return fmt.Errorf("producer required: %w", ErrInvalidPayload)
	}
	return nil
}
