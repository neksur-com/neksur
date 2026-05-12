"""Security: D-001.08 depth-cap pre-parser rejects unbounded traversals.

Maps to PLAN 00-02 Task 3's W-1 mitigation requirement:
    "tests/security/test_depth_cap.py with: (1) test_unbounded_star_rejected,
     (2) test_open_upper_bound_rejected, (3) test_bounded_traversal_accepted,
     (4) test_default_depth_three_accepted".

These tests run purely in Python — no Postgres needed. They exercise
``src.graph.client._validate_traversal_depth`` directly, which is the
gateway floor for D-001.08. The pre-parser MUST reject the dangerous
shapes BEFORE the query reaches Postgres so that even an AGE planner
regression cannot turn a missed depth cap into a tenant-wide DoS
(T-0-DOS in the plan's threat register).
"""

from __future__ import annotations

import pytest

from src.graph.client import (
    UnboundedTraversalError,
    _validate_traversal_depth,
)

pytestmark = pytest.mark.security


# ---------------------------------------------------------------------------
# Rejection cases — must raise UnboundedTraversalError.
# ---------------------------------------------------------------------------


def test_unbounded_star_rejected() -> None:
    """Bare ``*`` in a variable-length traversal is rejected."""
    with pytest.raises(UnboundedTraversalError) as exc:
        _validate_traversal_depth("MATCH p=(a)-[*]->(b) RETURN p")
    assert "D-001.08" in str(exc.value), (
        f"error message lost the D-001.08 anchor: {exc.value}"
    )


def test_open_upper_bound_rejected() -> None:
    """``*N..`` (lower bound only, no upper) is rejected."""
    with pytest.raises(UnboundedTraversalError):
        _validate_traversal_depth("MATCH p=(a)-[*1..]->(b) RETURN p")
    # Also a high lower bound — same rejection.
    with pytest.raises(UnboundedTraversalError):
        _validate_traversal_depth("MATCH p=(a)-[*10..]->(b) RETURN p")


def test_no_bounds_rejected() -> None:
    """``*..`` (neither bound) is rejected."""
    with pytest.raises(UnboundedTraversalError):
        _validate_traversal_depth("MATCH p=(a)-[*..]->(b) RETURN p")


# ---------------------------------------------------------------------------
# Acceptance cases — must NOT raise.
# ---------------------------------------------------------------------------


def test_bounded_traversal_accepted() -> None:
    """``*N..M`` with both bounds is accepted by the pre-parser.

    Note: "accepted by the pre-parser" means no UnboundedTraversalError.
    The query may still fail downstream for unrelated reasons (label
    missing, syntax error inside AGE, etc.); the assertion is only about
    the depth-cap gate.
    """
    _validate_traversal_depth("MATCH p=(a)-[*1..5]->(b) RETURN p")
    _validate_traversal_depth("MATCH p=(a)-[*2..4]->(b) RETURN p")


def test_default_depth_three_accepted() -> None:
    """D-001.08 default depth 3 — ``*1..3`` passes."""
    _validate_traversal_depth("MATCH p=(a)-[*1..3]->(b) RETURN p")


def test_upper_only_accepted() -> None:
    """``*..M`` (upper bound only, lower defaults to 1) is accepted.

    Per the PLAN's interfaces block: "bare ``*..N`` IS ALLOWED (lower
    bound defaults to 1, upper is bounded)".
    """
    _validate_traversal_depth("MATCH p=(a)-[*..3]->(b) RETURN p")


def test_exact_length_accepted() -> None:
    """``*N`` (exact length) — bounded by definition, accepted."""
    _validate_traversal_depth("MATCH p=(a)-[*3]->(b) RETURN p")


def test_query_without_vlp_accepted() -> None:
    """A plain MATCH with no variable-length traversal is untouched."""
    _validate_traversal_depth(
        "MATCH (t:Table {uri: 'iceberg://x/y/z'}) RETURN t"
    )


def test_multiline_query_with_unbounded_rejected() -> None:
    """Multi-line query containing a bare ``*`` is still rejected.

    Regression guard: the regex is MULTILINE; a `\\n` between OUR pattern
    and the terminator would not be a defence.
    """
    query = (
        "MATCH p=(a)-[*]->(b)\n"
        "WHERE a.tenant_id = 'X'\n"
        "RETURN p"
    )
    with pytest.raises(UnboundedTraversalError):
        _validate_traversal_depth(query)
