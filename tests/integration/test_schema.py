"""Integration: Phase 0 W1 schema present and complete after migrations.

Maps to 00-VALIDATION.md row:
    02-T3 / REQ-knowledge-graph-foundation / "All 19 vlabels + 24 elabels
    present after migration (per D-003.06 amendment)".

The migrations are applied by the session-scoped ``migrated_age_postgres``
fixture (see tests/integration/conftest.py). Each test here is read-only
against the catalog tables.
"""

from __future__ import annotations

import pytest

from src.graph.client import LABEL_WHITELIST

pytestmark = pytest.mark.integration


# ---------------------------------------------------------------------------
# Expected label sets — kept in lockstep with src/graph/client.py
# LABEL_WHITELIST. The PLAN locks these via D-001.05 / D-001.06 amended by
# D-003.06. Drift between this test and the whitelist fails fast.
# ---------------------------------------------------------------------------
EXPECTED_VLABELS = frozenset({
    # 19 vlabels (D-001.05 + D-003.06)
    "Table", "Column", "Snapshot", "Metric", "Dimension",
    "View", "Dashboard", "Pipeline", "Query", "Person",
    "Team", "Policy", "GlossaryTerm", "Tag", "DataContract",
    "Engine", "Catalog", "WriteEvent", "DetectionRun",
})

EXPECTED_ELABELS = frozenset({
    # 18 mandatory elabels (D-001.06 + D-003.06)
    "LINEAGE_OF", "OWNS", "MEMBER_OF", "DEPENDS_ON", "CLASSIFIED_AS",
    "APPLIES_TO", "DEFINED_BY", "WROTE", "READ", "PRODUCES",
    "CONSUMES", "GOVERNED_BY", "STORED_IN", "RUNS_ON", "SUPERSEDES",
    "INTENDED_WRITE", "ACTUAL_WRITE", "VIOLATION_DETECTED_BY",
    # 6 supplement elabels (D-001.06)
    "BELONGS_TO", "OF_TABLE", "USED_ENGINE", "USES_DIMENSION",
    "RAN_ON", "GOVERNS",
})


def test_all_labels_present(migrated_age_postgres) -> None:
    """All 19 vlabels and 24 elabels exist in ag_catalog after V0010.

    Note: AGE auto-creates synthetic `_ag_label_vertex` / `_ag_label_edge`
    rows via `create_graph()`. They are NOT part of the D-001.05/.06
    contract — we filter them out with a name-prefix exclusion.
    """
    # Count assertions (the headline contract).
    with migrated_age_postgres.cursor() as cur:
        cur.execute(r"""
            SELECT count(*) FROM ag_catalog.ag_label
            WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
              AND kind = 'v'
              AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
        """)
        (vcount,) = cur.fetchone()
        cur.execute(r"""
            SELECT count(*) FROM ag_catalog.ag_label
            WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
              AND kind = 'e'
              AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
        """)
        (ecount,) = cur.fetchone()
    assert vcount == 19, (
        f"vlabel count {vcount} != 19 — D-001.05 amended by D-003.06 requires 19"
    )
    assert ecount == 24, (
        f"elabel count {ecount} != 24 — D-001.06 amended by D-003.06 requires 24"
    )

    # Exact-set assertions (catches drift in identifier spelling).
    with migrated_age_postgres.cursor() as cur:
        cur.execute(r"""
            SELECT name FROM ag_catalog.ag_label
            WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
              AND kind = 'v'
              AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
        """)
        vnames = {row[0] for row in cur.fetchall()}
        cur.execute(r"""
            SELECT name FROM ag_catalog.ag_label
            WHERE graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')
              AND kind = 'e'
              AND name NOT LIKE E'\\_ag\\_label\\_%' ESCAPE E'\\'
        """)
        enames = {row[0] for row in cur.fetchall()}

    assert vnames == EXPECTED_VLABELS, (
        f"vlabel set mismatch.\nExtra: {vnames - EXPECTED_VLABELS}\n"
        f"Missing: {EXPECTED_VLABELS - vnames}"
    )
    assert enames == EXPECTED_ELABELS, (
        f"elabel set mismatch.\nExtra: {enames - EXPECTED_ELABELS}\n"
        f"Missing: {EXPECTED_ELABELS - enames}"
    )


def test_whitelist_matches_catalog(migrated_age_postgres) -> None:
    """src.graph.client.LABEL_WHITELIST is exactly the catalog set.

    This is the drift gate: any addition to the schema MUST be reflected
    in LABEL_WHITELIST (and vice versa), or the application gateway will
    reject legitimate label-bearing queries / accept removed-label queries.
    """
    expected_union = EXPECTED_VLABELS | EXPECTED_ELABELS
    assert LABEL_WHITELIST == expected_union, (
        f"LABEL_WHITELIST drift.\nExtra: {LABEL_WHITELIST - expected_union}\n"
        f"Missing: {expected_union - LABEL_WHITELIST}"
    )
    assert len(LABEL_WHITELIST) == 43


def test_extensions_present(migrated_age_postgres) -> None:
    """V0001 enabled age + pg_stat_statements; pgaudit is conditional.

    pgaudit is REQUIRED in production (Phase 0 production image bundles it).
    In testcontainer dev/test runs the base apache/age image does not
    include it; V0001 conditionally installs and emits a NOTICE. The
    Phase 0 test gates don't depend on pgaudit's presence — but `age`
    and `pg_stat_statements` MUST be there.
    """
    with migrated_age_postgres.cursor() as cur:
        cur.execute(
            "SELECT extname FROM pg_extension WHERE extname IN "
            "('age', 'pgaudit', 'pg_stat_statements')"
        )
        names = {row[0] for row in cur.fetchall()}
    assert "age" in names, "AGE extension missing — V0001 failed"
    assert "pg_stat_statements" in names, (
        "pg_stat_statements missing — V0001 failed"
    )
    # pgaudit is optional in dev/test; verify the image *advertises* it
    # as available if installed — but tolerate absence.
    with migrated_age_postgres.cursor() as cur:
        cur.execute(
            "SELECT 1 FROM pg_available_extensions WHERE name='pgaudit'"
        )
        pgaudit_available = cur.fetchone() is not None
    if pgaudit_available:
        assert "pgaudit" in names, (
            "pgaudit advertised as available but not installed by V0001"
        )
