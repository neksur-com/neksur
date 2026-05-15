#!/usr/bin/env python3
"""Generate the synthetic PII parquet fixture used by Plan 01-07 detection tests.

Plan 01-01 commits both this script and the produced binary
`pii_orders.parquet` so Plan 01-07's regex classifier has a known shape
to test against. The seeded distributions mirror the ADR-007
"regulatory_refs" patterns that the classifier targets:

  * email          100% rows  — `<user>@<domain>` shape
  * ssn             80% rows  — `\\d{3}-\\d{2}-\\d{4}` formatted
                     20% rows  — `\\d{9}` unformatted (catches the
                                  digit-run-detector path)
  * credit_card     50% rows  — 16-digit space-separated
                     50% rows  — empty string (negative-case half)
  * phone          100% rows  — `(\\d{3}) \\d{3}-\\d{4}` formatted

Deterministic output: `random.seed(42)` is set before any draws so
regenerating produces a byte-identical parquet file (modulo pyarrow
version drift; pin to >=15 if reproducibility matters across CI runs).

Regeneration:

    cd /Users/evgeny/neksur-core
    python3 tests/integration/fixtures/gen_pii_parquet.py

Plan 01-01 Task 3 ships the committed pii_orders.parquet so consumers
don't need pyarrow installed at test time.
"""

import random

import pyarrow as pa
import pyarrow.parquet as pq

ROW_COUNT = 1000
OUT_PATH = "tests/integration/fixtures/pii_orders.parquet"


def gen_email(i: int) -> str:
    domains = ["acme.example", "example.com", "test.example", "neksur.example"]
    return f"user{i}@{domains[i % len(domains)]}"


def gen_ssn(i: int) -> str:
    a = random.randint(100, 999)
    b = random.randint(10, 99)
    c = random.randint(1000, 9999)
    if i % 5 == 0:
        # 20% unformatted digit-run
        return f"{a}{b}{c}"
    return f"{a}-{b}-{c}"


def gen_credit_card(i: int) -> str:
    if i % 2 == 0:
        # 50% rows carry a 16-digit space-separated number
        groups = [str(random.randint(1000, 9999)) for _ in range(4)]
        return " ".join(groups)
    return ""


def gen_phone(i: int) -> str:
    a = random.randint(200, 999)
    b = random.randint(200, 999)
    c = random.randint(1000, 9999)
    return f"({a}) {b}-{c}"


def main() -> None:
    random.seed(42)
    order_ids = list(range(1, ROW_COUNT + 1))
    emails = [gen_email(i) for i in order_ids]
    ssns = [gen_ssn(i) for i in order_ids]
    ccs = [gen_credit_card(i) for i in order_ids]
    phones = [gen_phone(i) for i in order_ids]

    table = pa.table(
        {
            "order_id": pa.array(order_ids, type=pa.int64()),
            "email": pa.array(emails, type=pa.string()),
            "ssn": pa.array(ssns, type=pa.string()),
            "credit_card": pa.array(ccs, type=pa.string()),
            "phone": pa.array(phones, type=pa.string()),
        }
    )
    pq.write_table(table, OUT_PATH, compression="snappy")
    print(f"wrote {OUT_PATH} rows={ROW_COUNT}")


if __name__ == "__main__":
    main()
