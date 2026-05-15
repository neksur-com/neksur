// Unit tests for the pure-function surface of internal/ingest.
//
// These tests do NOT spin up a Postgres+AGE container — that's the
// integration-tag tier (tests/integration/ingest_*_test.go). Here we
// cover:
//
//   - LineageCycleError.Error() formatting (CONTEXT specifics line 171).
//   - LineageCycleError errors.Is(ErrLineageCycle) discrimination.
//   - internal/graph/helpers.go::BoundedAncestors panic on depth > Max.
//
// The package builds with no integration tag so `go test ./internal/...`
// includes them and CI fails fast on shape regressions.

package ingest

import (
	"errors"
	"strings"
	"testing"

	"github.com/neksur-com/neksur/internal/graph"
)

func TestLineageCycleErrorMessageContainsCyclePath(t *testing.T) {
	err := &LineageCycleError{
		SourceID: "iceberg://a",
		TargetID: "iceberg://b",
		Cycle:    []string{"a", "b", "c", "a"},
	}
	msg := err.Error()
	if !strings.Contains(msg, "a -> b -> c -> a") {
		t.Errorf("LineageCycleError.Error() missing cycle path; got %q", msg)
	}
	if !strings.Contains(msg, "iceberg://a") {
		t.Errorf("LineageCycleError.Error() missing source; got %q", msg)
	}
	if !strings.Contains(msg, "iceberg://b") {
		t.Errorf("LineageCycleError.Error() missing target; got %q", msg)
	}
}

func TestLineageCycleErrorIsSentinel(t *testing.T) {
	err := &LineageCycleError{
		SourceID: "x", TargetID: "y", Cycle: []string{"x", "y", "x"},
	}
	if !errors.Is(err, ErrLineageCycle) {
		t.Errorf("errors.Is(*LineageCycleError, ErrLineageCycle) = false; expected true")
	}
	// Negative case — a different sentinel must NOT match.
	if errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("errors.Is(*LineageCycleError, ErrSnapshotNotFound) = true; expected false")
	}
}

func TestLineageCycleErrorAsRecoversTypedStruct(t *testing.T) {
	orig := &LineageCycleError{
		SourceID: "src", TargetID: "tgt", Cycle: []string{"src", "tgt", "src"},
	}
	var wrapped error = orig
	var recovered *LineageCycleError
	if !errors.As(wrapped, &recovered) {
		t.Fatal("errors.As did not recover *LineageCycleError")
	}
	if recovered.SourceID != "src" || recovered.TargetID != "tgt" {
		t.Errorf("recovered struct fields wrong: %+v", recovered)
	}
	if len(recovered.Cycle) != 3 {
		t.Errorf("recovered cycle length %d; expected 3", len(recovered.Cycle))
	}
}

func TestBoundedAncestorsRespectsMax(t *testing.T) {
	// Depth = max should produce a valid fragment.
	frag := graph.BoundedAncestors("Table", graph.BoundedDepthMax)
	want := "(n:Table)<-[:LINEAGE_OF*1..3]-(ancestor)"
	if frag != want {
		t.Errorf("BoundedAncestors(Table, %d) = %q; want %q",
			graph.BoundedDepthMax, frag, want)
	}
}

func TestBoundedAncestorsPanicsAboveMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on depth > BoundedDepthMax; got none")
		}
	}()
	_ = graph.BoundedAncestors("Table", graph.BoundedDepthMax+1)
}

func TestBoundedAncestorsPanicsBelowOne(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on depth < 1; got none")
		}
	}()
	_ = graph.BoundedAncestors("Table", 0)
}

func TestBoundedDescendantsRespectsMax(t *testing.T) {
	frag := graph.BoundedDescendants("Snapshot", 2)
	want := "(n:Snapshot)-[:LINEAGE_OF*1..2]->(descendant)"
	if frag != want {
		t.Errorf("BoundedDescendants(Snapshot, 2) = %q; want %q", frag, want)
	}
}

func TestBoundedDescendantsPanicsAboveMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on depth > BoundedDepthMax; got none")
		}
	}()
	_ = graph.BoundedDescendants("Table", 100)
}

// parseCyclePath unit tests removed — Plan 01-04 deviation #3
// (AGE 1.6 `nodes(path)` quirk) replaced the AGE-list-parsing helper
// with application-side path reconstruction (cycle.go::fetchCyclePath).
// The fetchCyclePath path is exercised end-to-end by the
// TestIngestLineageCycleRejected integration test.

func TestStripAgtypeQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"hello"`, "hello"},
		{`"hello"::text`, "hello"},
		{`bare`, "bare"},
		{`""`, ""},
	}
	for _, tc := range cases {
		got := stripAgtypeQuotes(tc.in)
		if got != tc.want {
			t.Errorf("stripAgtypeQuotes(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
