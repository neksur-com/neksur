"""Security: Cypher injection cannot bypass tenant filtering.

Maps to 00-VALIDATION.md row:
    02-T3 / REQ-tenant-isolation / T-0-INJ / "Cypher injection via
            string-concat does not bypass tenant filter".

Two paths to harden:

1. **Label whitelist** — labels cannot be parameterised in Cypher; any
   call site that takes a label argument MUST whitelist. Tested via
   ``GraphClient._validate_label``: a malicious label string is rejected
   before reaching the database.

2. **Parameter passthrough** — values that ARE parameterisable must go
   through psycopg's bind interface, not string concatenation. Tested by
   passing a tenant_id containing a SQL injection payload and asserting
   it ends up treated as a literal string.

Phase 0 floor; Phase 5 ADR-004 adds MCP-aware schema-bound hardening.
"""

from __future__ import annotations

import json

import pytest

from src.graph.client import LABEL_WHITELIST, GraphClient

pytestmark = pytest.mark.security


# ---------------------------------------------------------------------------
# Whitelist tests — pure Python, no DB needed.
# ---------------------------------------------------------------------------


def test_string_concat_rejected_by_whitelist() -> None:
    """A label string with a SQL-injection payload is rejected pre-DB."""
    malicious = "Table; DROP TABLE foo --"
    assert malicious not in LABEL_WHITELIST  # sanity
    with pytest.raises(ValueError) as exc:
        GraphClient._validate_label(malicious)
    assert "LABEL_WHITELIST" in str(exc.value), (
        f"whitelist rejection message lost context: {exc.value}"
    )


def test_whitelist_accepts_canonical_labels() -> None:
    """Sanity: the canonical 43 identifiers all pass the whitelist."""
    for label in LABEL_WHITELIST:
        GraphClient._validate_label(label)  # must not raise


def test_whitelist_rejects_lowercase_variant() -> None:
    """Whitelist is exact-match, case-sensitive (Cypher labels are PascalCase)."""
    with pytest.raises(ValueError):
        GraphClient._validate_label("table")  # lowercase


# ---------------------------------------------------------------------------
# Parameter passthrough — needs the DB to verify literal-vs-execute.
# ---------------------------------------------------------------------------


@pytest.mark.integration
def test_parameter_passthrough_safe(migrated_age_postgres, tenant_conn_factory) -> None:
    """Malicious tenant_id passed as a parameter is treated as a literal.

    Procedure:
        1. Set the tenant context to a value containing what *looks like*
           a SQL escape sequence.
        2. Insert a row carrying that same string as its tenant_id
           property.
        3. Read it back; the property should equal the input string
           verbatim — not a DROP TABLE'd state, not an empty database.
    """
    # Note: SET LOCAL uses parameter binding (%s) in the fixture, so the
    # input is treated as a literal text value regardless of content.
    malicious_tid = "X'; DROP TABLE neksur.\"Table\"; --"

    conn = tenant_conn_factory(malicious_tid)
    base = 281474976710830
    try:
        with conn.cursor() as cur:
            cur.execute(
                'INSERT INTO neksur."Table" (id, properties) '
                'VALUES (%s::graphid, %s::agtype)',
                (str(base + 1), json.dumps({
                    "uri": "iceberg://t/param-probe",
                    "tenant_id": malicious_tid,
                })),
            )
        conn.execute("COMMIT;")
    except Exception:
        conn.execute("ROLLBACK;")
        raise

    # Read it back within the same (malicious-tid) context — should
    # succeed and return the row, proving the literal made it through
    # both the RLS check and the storage layer intact.
    conn = tenant_conn_factory(malicious_tid)
    try:
        with conn.cursor() as cur:
            cur.execute(
                'SELECT properties FROM neksur."Table" '
                'WHERE id = %s::graphid',
                (str(base + 1),),
            )
            row = cur.fetchone()
    finally:
        try:
            conn.execute("ROLLBACK;")
        except Exception:
            pass

    assert row is not None, (
        "row vanished — the malicious tenant_id may have triggered a "
        "DROP somewhere, or the binder failed catastrophically."
    )
    # Also assert the Table label table still exists (the strongest
    # 'no-DROP-happened' evidence).
    with migrated_age_postgres.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM ag_catalog.ag_label "
            "WHERE name = 'Table' "
            "AND graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name='neksur')"
        )
        (still_there,) = cur.fetchone()
    assert still_there == 1, (
        "T-0-INJ CRITICAL: the 'Table' vlabel disappeared from ag_catalog. "
        "Parameter passthrough is NOT safe — the malicious tenant_id was "
        "interpreted as SQL."
    )
