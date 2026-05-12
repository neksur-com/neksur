"""Integration: hybrid storage rule (D-001.04) — node property bag <2KB avg.

Maps to 00-VALIDATION.md row:
    02-T3 / REQ-knowledge-graph-foundation / "Hybrid storage rule honored —
            per-node property bag avg <2KB".

The point: per D-001.04, the graph stores *relations and identifiers*;
high-volume properties (full SQL text, full Iceberg manifest payloads,
raw audit blobs) belong in auxiliary Postgres tables. This is empirically
enforced by sampling realistic node shapes and asserting the avg
``pg_column_size(properties)`` stays under 2048 bytes.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone

import pytest

pytestmark = pytest.mark.integration


def _insert_node(conn, label: str, vertex_id: int, properties: dict) -> None:
    """Direct underlying-table insert for property-size measurement.

    AGE's vertex `id` column is of custom type `graphid` (a packed
    label_id + entry_id big-integer). psycopg's int binder produces a
    `bigint`, which Postgres won't implicitly cast. We pass the id as
    text and use the graphid input function via ``::graphid``.
    """
    with conn.cursor() as cur:
        cur.execute(
            f'INSERT INTO neksur."{label}" (id, properties) '
            'VALUES (%s::graphid, %s::agtype)',
            (str(vertex_id), json.dumps(properties)),
        )


def test_node_property_size_under_2kb(migrated_age_postgres, tenant_conn_factory) -> None:
    """Average pg_column_size(properties) across realistic nodes stays <2KB."""
    tid = "test-tenant-storage"
    conn = tenant_conn_factory(tid)
    base_id = 281474976710700  # AGE-encoded vertex id base

    # Three typical node shapes per ADR §3.4 property-bag examples.
    nodes = [
        ("Table", base_id + 1, {
            "uri": "iceberg://polaris-prod/sales/orders",
            "catalog_id": "polaris-prod",
            "namespace": "sales",
            "name": "orders",
            "current_snapshot_id": 7842901234567890,
            "partition_spec_id": 3,
            "format_version": 2,
            "owner_team_id": "team-data-eng",
            "created_at": "2025-03-15T10:30:00Z",
            "description": "Order events from Kafka stream",
            "tenant_id": tid,
        }),
        ("Column", base_id + 2, {
            "uri": "iceberg://polaris-prod/sales/orders#user_email",
            "parent_table_uri": "iceberg://polaris-prod/sales/orders",
            "name": "user_email",
            "type": "string",
            "ordinal_position": 4,
            "nullable": False,
            "field_id": 4,
            "tenant_id": tid,
        }),
        ("Snapshot", base_id + 3, {
            "snapshot_id": 7842901234567890,
            "table_uri": "iceberg://polaris-prod/sales/orders",
            "parent_snapshot_id": 7842876543210987,
            "committed_at": "2025-05-10T14:23:45.123Z",
            "committed_by_engine": "spark",
            "committed_by_engine_id": "spark-prod-cluster-1",
            "operation": "append",
            "added_files": 47,
            "added_records": 1450000,
            "summary": "Hourly batch ingestion from Kafka",
            "tenant_id": tid,
        }),
    ]

    try:
        for label, vid, props in nodes:
            _insert_node(conn, label, vid, props)

        # Measure pg_column_size on each row we just inserted.
        sizes: list[int] = []
        with conn.cursor() as cur:
            for label, vid, _ in nodes:
                cur.execute(
                    f'SELECT pg_column_size(properties) FROM neksur."{label}" '
                    'WHERE id = %s::graphid',
                    (str(vid),),
                )
                row = cur.fetchone()
                assert row is not None, f"{label} {vid} not found after insert"
                sizes.append(row[0])

        avg = sum(sizes) / len(sizes)
        max_seen = max(sizes)
    finally:
        try:
            conn.execute("ROLLBACK;")
        except Exception:
            pass

    assert avg < 2048, (
        f"avg property-bag size {avg:.0f} bytes >= 2048 — hybrid storage "
        f"D-001.04 violated. Per-node sizes: {sizes}"
    )
    # Sanity floor: per-node max also shouldn't blow past 2KB on these
    # canonical examples; if it does, the shape is wrong.
    assert max_seen < 2048, (
        f"max node size {max_seen} bytes >= 2048. Sizes: {sizes}"
    )
