"""Integration: D-001.07 indexes exist AND are actually used by EXPLAIN.

Maps to 00-VALIDATION.md rows:
    02-T3 / REQ-knowledge-graph-foundation / "All D-001.07 indexes exist"
    02-T3 / REQ-knowledge-graph-foundation / "Indexes are *used* by
            hot-path queries (EXPLAIN shows Index Scan)"

The "indexes used" assertion is critical: AGE issue #1010 lets a GIN
index on `properties` created AFTER data load silently miss existing
rows. V0025 mitigates by creating GIN BEFORE any data; this test
empirically verifies the Index Scan path on a freshly-seeded row.
"""

from __future__ import annotations

import json

import pytest

pytestmark = pytest.mark.integration


# AGE creates per-label tables; the canonical D-001.07 property indexes
# we EXPECT to exist after V0020 (by ag_catalog metadata or by pg_indexes
# on the underlying table).
EXPECTED_AGE_PROPERTY_INDEXES = [
    # (label, property)  — created via SELECT create_property_index(...)
    ("Table", "uri"),
    ("Table", "catalog_id"),
    ("Column", "uri"),
    ("Column", "parent_table_uri"),
    ("Snapshot", "snapshot_id"),
    ("Snapshot", "table_uri"),
    ("Snapshot", "committed_at"),
    ("Metric", "name"),
    ("Person", "email"),
    ("Tag", "id"),
    ("Query", "query_id"),
]

EXPECTED_AGE_EDGE_INDEXES = [
    ("LINEAGE_OF", "created_at"),
    ("READ", "at"),
    ("WROTE", "at"),
]

EXPECTED_FUNCTIONAL_INDEXES = [
    "idx_table_namespace",
    "idx_snapshot_time",
]


def _count_indexes_on(conn, table_name: str) -> int:
    with conn.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM pg_indexes WHERE schemaname='neksur' AND tablename=%s",
            (table_name,),
        )
        (n,) = cur.fetchone()
    return n


def test_required_indexes(migrated_age_postgres) -> None:
    """All D-001.07 property + edge indexes + 2 functional indexes exist."""
    conn = migrated_age_postgres

    # Each AGE property index creates an index on the underlying
    # neksur."<Label>" table. We assert at least one non-PK index per
    # (label, prop) pair exists. The exact name AGE picks varies by
    # version; the strong invariant is "the underlying table has more
    # than just its primary key after V0020."
    #
    # For the Postgres functional indexes (idx_table_namespace,
    # idx_snapshot_time) the names ARE deterministic so we check exact.
    with conn.cursor() as cur:
        cur.execute("""
            SELECT indexname FROM pg_indexes WHERE schemaname='neksur'
            AND indexname IN ('idx_table_namespace', 'idx_snapshot_time')
        """)
        names = {row[0] for row in cur.fetchall()}
    missing = set(EXPECTED_FUNCTIONAL_INDEXES) - names
    assert not missing, f"functional indexes missing: {missing}"

    # AGE-created property indexes — count by exact name match against
    # the polyfill's deterministic naming (`idx_<Label>_<Property>`).
    with conn.cursor() as cur:
        cur.execute("""
            SELECT indexname FROM pg_indexes WHERE schemaname='neksur'
        """)
        all_neksur_indexes = {row[0] for row in cur.fetchall()}

    for label, prop in EXPECTED_AGE_PROPERTY_INDEXES:
        expected = f"idx_{label}_{prop}"
        assert expected in all_neksur_indexes, (
            f"D-001.07 property index missing: {expected} "
            f"(found: {sorted(n for n in all_neksur_indexes if label in n)})"
        )

    # Edge indexes — polyfill appends `_edge` suffix on elabel indexes
    # to avoid name collisions with any vlabel index that might share
    # a (label, property) pair.
    for label, prop in EXPECTED_AGE_EDGE_INDEXES:
        expected = f"idx_{label}_{prop}_edge"
        assert expected in all_neksur_indexes, (
            f"D-001.07 edge timestamp index missing: {expected}"
        )


def test_per_vlabel_tenant_and_gin_indexes(migrated_age_postgres) -> None:
    """V0025: every vlabel has idx_<Label>_tenant + idx_<Label>_props_gin."""
    conn = migrated_age_postgres
    with conn.cursor() as cur:
        cur.execute("""
            SELECT count(*) FROM pg_indexes WHERE schemaname='neksur'
            AND indexname ~ '^idx_[A-Za-z]+_tenant$'
        """)
        (tenant_count,) = cur.fetchone()
        cur.execute("""
            SELECT count(*) FROM pg_indexes WHERE schemaname='neksur'
            AND indexname ~ '^idx_[A-Za-z]+_props_gin$'
        """)
        (gin_count,) = cur.fetchone()
    assert tenant_count == 19, f"tenant idx count {tenant_count} != 19"
    assert gin_count == 19, f"GIN idx count {gin_count} != 19"


def test_indexes_used_in_explain(migrated_age_postgres, tenant_conn_factory) -> None:
    """EXPLAIN of a uri-keyed MATCH shows Index Scan when seqscan is off.

    This is the AGE issue #1010 smoking gun: if GIN were created AFTER
    data load, even disabling seqscan would NOT yield an Index Scan
    (the index physically doesn't contain the existing rows). With
    V0025's BEFORE-load ordering, disabling seqscan forces the planner
    to pick the GIN-on-properties index — proving the index is
    populated and usable.

    Why disable seqscan: at small row counts, Postgres's cost model
    prefers Seq Scan regardless of index availability. The contract is
    "is the index *usable*", not "does the planner *choose* the index
    at single-row scale" — the load-tier tests (Plan 00-06) will probe
    planner-choice at envelope scale where row counts make the index
    cheaper than the scan.
    """
    # Seed one row inside a tenant context.
    tid = "test-tenant-explain"
    conn = tenant_conn_factory(tid)
    try:
        with conn.cursor() as cur:
            # Use direct underlying-table insert so we don't have to
            # navigate AGE's quirky CREATE-via-Cypher value escaping.
            cur.execute(
                """
                INSERT INTO neksur."Table" (id, properties)
                VALUES (
                    '281474976710657',
                    %s::agtype
                )
                """,
                (json.dumps({
                    "uri": "iceberg://test/explain/probe",
                    "name": "probe",
                    "tenant_id": tid,
                }),),
            )
        conn.execute("COMMIT;")
    except Exception:
        conn.execute("ROLLBACK;")
        raise

    # Force the planner to consider the index by disabling seqscan for
    # the duration of this EXPLAIN.
    conn = tenant_conn_factory(tid)
    try:
        conn.execute("SET LOCAL enable_seqscan = off")
        with conn.cursor() as cur:
            cur.execute(
                """
                EXPLAIN (FORMAT JSON, ANALYZE)
                SELECT * FROM cypher('neksur', $$
                    MATCH (t:Table {uri: 'iceberg://test/explain/probe'})
                    RETURN t
                $$) AS (t agtype)
                """
            )
            plan_rows = cur.fetchall()
        plan_text = json.dumps(plan_rows)
    finally:
        try:
            conn.execute("ROLLBACK;")
        except Exception:
            pass

    # The plan should mention Index Scan (or Bitmap Index Scan).
    has_index_scan = (
        "Index Scan" in plan_text
        or "Index Only Scan" in plan_text
        or "Bitmap Index Scan" in plan_text
        or "Bitmap Heap Scan" in plan_text
    )
    assert has_index_scan, (
        "EXPLAIN does not show Index Scan even with enable_seqscan=off — "
        "AGE issue #1010 may be biting (GIN populated AFTER load), OR "
        "the index expression does not match the Cypher predicate shape. "
        f"Plan dump: {plan_text[:2000]}"
    )
