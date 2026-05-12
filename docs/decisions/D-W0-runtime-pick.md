# D-W0-runtime-pick — Phase 0 Runtime: Python 3.12

**Status:** LOCKED
**Date:** 2026-05-12
**Phase:** 0 — Metadata Graph Foundation
**Wave:** 0 (foundation)
**Decided by:** Phase 0 planning (Plan 00-01)
**Supersedes:** none
**Superseded by:** none

## Context

Phase 0 builds the metadata graph foundation on Apache AGE 1.6.0 / PostgreSQL 16
(ADR-001 locks the storage stack via D-001.01..15). Phase 0 is infrastructure-
heavy: testcontainers fixtures, chaos drivers (Patroni kill-primary / failover
timing), DR drill scripts (pgBackRest PITR), load seeding into the 10M-node /
50M-edge envelope, and Cypher-query observability middleware. No production
business logic ships in Phase 0 — only the substrate every later phase asserts
against.

The phase needs a single locked runtime so all subsequent plans (00-02 .. 00-06)
can scaffold against it without re-deciding. 00-RESEARCH.md §Open Questions #5
explicitly defers the runtime choice to Wave 0 with the note: "If no other signal
exists, prefer Python — AGEFreighter, testcontainers-python and psycopg are all
documented for the exact stack."

## Decision

**Python 3.12+ is the runtime for Phase 0.**

Rationale (verbatim from 00-01-PLAN.md `<runtime_decision>` block):

- **AGEFreighter (Microsoft-maintained) is Python** and gives a documented
  bulk-load path that fits the W5 envelope test
  (`apache/age:release_PG16_1.6.0` image with 725K nodes + 2.8M edges loaded in
  83s per the Microsoft Learn benchmark).
- **testcontainers-python and psycopg[binary] are both stable and widely
  deployed** for the exact stack (Postgres + extension via Docker image).
- **pytest is the strongest fixture-based test framework** for the chaos /
  load / DR / security mixed-mode tests this phase needs (per-tier conftests,
  marker-based selection, asyncio support).
- **00-RESEARCH.md Open Question #5 explicitly suggests Python** if no other
  signal exists.
- The runtime can change in later phases; Phase 0 is infrastructure-heavy and
  Python's operational maturity (chrony bindings, asyncpg, pgBackRest
  invocation via subprocess) is the lowest-risk path.

Concretely: `requires-python = ">=3.12"` in `pyproject.toml`, `.python-version`
pinned to `3.12`. Wave 1 (Plan 00-02) will add a `uv.lock` lock-file pinning
exact versions of every dev dependency (closing supply-chain threat
T-0-W0-SUPPLY).

## Consequences

**Positive:**

- All Phase 0 tooling (testcontainers, psycopg, pytest, AGEFreighter) reads
  from a single dependency set in `pyproject.toml`.
- Standard editor / language-server tooling (Pyright, Ruff) works out of the
  box on every contributor's machine.
- CI workflows can use the official `actions/setup-python@v5` action with
  `python-version-file: .python-version` for reproducibility.

**Negative / accepted:**

- Python is not the long-term runtime for *all* of Neksur — Phase 2 may
  introduce JVM-based engine adapters (Spark / Trino) and Phase 4 may
  introduce Go / Rust for the Semantic Engine (see PROJECT.md deferred items).
  Those phases will make their own runtime decisions; this ADR scopes only to
  Phase 0.
- AGE 1.6.0 image (`apache/age:release_PG16_1.6.0`) is what testcontainers
  will pull — confirmed as the most recent GA pairing for PG16 per
  00-RESEARCH.md §Summary. PG17 has an open AGE compatibility issue (#2111),
  PG18 only has AGE 1.7.0-rc.

**Neutral:**

- The dev dependency set is intentionally specified with **lower bounds only**
  in this plan (e.g. `pytest>=8.0`). The exact lock-file is deferred to Wave 1
  per threat T-0-W0-SUPPLY. Until then, contributors may see drift across
  machines — accepted for Phase 0 W0.

## References

- ADR-001 (D-001.01..15) — storage stack lock
- 00-RESEARCH.md §Standard Stack, §Open Questions #5
- 00-01-PLAN.md `<runtime_decision>` block
- 00-VALIDATION.md (Per-Task Verification Map — every command assumes Python)
