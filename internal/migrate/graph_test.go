// Unit tests for stripWrappingTx — REVIEW.md CR-04 coverage.
//
// The migrate package's main entry points (ApplyTenantGraph,
// ApplyTenant) require a live Postgres connection so those tests
// live under the integration build tag in tests/integration/. This
// file covers the pure-string stripWrappingTx helper whose CR-04
// dollar-quoted-block-depth-tracking contract can be verified
// without IO.

package migrate

import (
	"strings"
	"testing"
)

func TestStripWrappingTx_StripsTopLevelMarkers(t *testing.T) {
	in := `BEGIN;
CREATE TABLE foo (id INT);
COMMIT;
`
	got := stripWrappingTx(in)
	if strings.Contains(got, "BEGIN;") {
		t.Errorf("expected top-level BEGIN; stripped; got %q", got)
	}
	if strings.Contains(got, "COMMIT;") {
		t.Errorf("expected top-level COMMIT; stripped; got %q", got)
	}
	if !strings.Contains(got, "CREATE TABLE foo (id INT);") {
		t.Errorf("expected body preserved; got %q", got)
	}
}

func TestStripWrappingTx_PreservesBeginInsideDollarBlock(t *testing.T) {
	// CR-04 canonical bug case: BEGIN;/COMMIT; lines INSIDE a DO $$
	// plpgsql block MUST be preserved verbatim — they are plpgsql
	// statements, not SQL transaction markers. The previous
	// line-equality match would have stripped them, corrupting
	// the plpgsql body.
	in := `BEGIN;
DO $$
BEGIN;
  INSERT INTO foo SELECT 1;
COMMIT;
END $$;
COMMIT;
`
	got := stripWrappingTx(in)
	// Outer BEGIN; / COMMIT; (top-level) ARE stripped.
	if strings.HasPrefix(strings.TrimSpace(got), "BEGIN;") {
		t.Errorf("expected top-level BEGIN; stripped at start; got %q", got)
	}
	// Inner BEGIN; / COMMIT; inside $$...$$ ARE preserved.
	// Count occurrences — exactly 1 each (the inner ones, since
	// the outer top-level ones are stripped).
	if got, want := strings.Count(got, "BEGIN;"), 1; got != want {
		t.Errorf("inner BEGIN; count = %d; want %d (only inner survives) — full body: %q",
			got, want, in)
	}
	if got, want := strings.Count(got, "COMMIT;"), 1; got != want {
		t.Errorf("inner COMMIT; count = %d; want %d (only inner survives)", got, want)
	}
	// The plpgsql body itself MUST survive (no whitespace shrink).
	if !strings.Contains(got, "INSERT INTO foo SELECT 1;") {
		t.Errorf("plpgsql body lost: %q", got)
	}
	if !strings.Contains(got, "END $$;") {
		t.Errorf("$$ block close lost: %q", got)
	}
}

func TestStripWrappingTx_HandlesTaggedDollarQuotes(t *testing.T) {
	// $tag$ ... $tag$ tagged dollar-quotes — same depth-tracking
	// semantics. Inner BEGIN; line should be preserved.
	in := `BEGIN;
DO $body$
  BEGIN;
  -- inner content
  COMMIT;
END $body$;
COMMIT;
`
	got := stripWrappingTx(in)
	if !strings.Contains(got, "  BEGIN;") {
		t.Errorf("inner BEGIN; inside $body$ tagged block lost: %q", got)
	}
	if !strings.Contains(got, "  COMMIT;") {
		t.Errorf("inner COMMIT; inside $body$ tagged block lost: %q", got)
	}
}

func TestStripWrappingTx_NoBeginNoCommit(t *testing.T) {
	// Body with no BEGIN/COMMIT survives unchanged (other than the
	// trailing newline added by the line-iterator).
	in := `SELECT 1;
SELECT 2;`
	got := stripWrappingTx(in)
	if !strings.Contains(got, "SELECT 1;") || !strings.Contains(got, "SELECT 2;") {
		t.Errorf("body lost: in=%q got=%q", in, got)
	}
}

func TestStripWrappingTx_PreservesIndentedBeginInsideDollarBlock(t *testing.T) {
	// Even when the inner BEGIN; has trailing whitespace OR is
	// indented inside the dollar-quoted block, it should be
	// preserved (the strip rule applies ONLY to TOP-LEVEL standalone
	// BEGIN;/COMMIT; lines).
	in := `BEGIN;
DO $$
    BEGIN;
END $$;
COMMIT;
`
	got := stripWrappingTx(in)
	if !strings.Contains(got, "    BEGIN;") {
		t.Errorf("indented inner BEGIN; lost: %q", got)
	}
}
