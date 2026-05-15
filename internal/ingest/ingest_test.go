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

func TestParseCyclePathHandlesAGEListSuffix(t *testing.T) {
	// AGE returns lists with `::list` suffix; parseCyclePath strips it.
	raw := []byte(`["a","b","c"]::list`)
	got := parseCyclePath(raw, "a", "c")
	if len(got) != 4 {
		t.Errorf("parseCyclePath returned %d elements; expected 4 (closing src appended): %v", len(got), got)
	}
	if got[0] != "a" || got[len(got)-1] != "a" {
		t.Errorf("parseCyclePath did not close cycle properly: %v", got)
	}
}

func TestParseCyclePathDegradesGracefullyOnGarbage(t *testing.T) {
	got := parseCyclePath([]byte("not-valid-json"), "src", "tgt")
	if len(got) != 3 || got[0] != "src" || got[2] != "src" {
		t.Errorf("parseCyclePath degraded path wrong: %v", got)
	}
}
