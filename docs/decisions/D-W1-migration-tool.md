# D-W1-migration-tool — Atlas vs sqitch for AGE-bearing migrations

**Status:** LOCKED
**Decided:** 2026-05-12
**Decision-makers:** Phase 0 Wave 1 executor (spike outcome)
**Supersedes:** Open Question #2 in 00-RESEARCH.md, Assumption A8
**Applies to:** All phases that produce SQL or Cypher DDL touching the
`neksur` AGE graph or its underlying Postgres tables.

---

## Decision

**Use sqitch** as Neksur's schema migration tool. Atlas was evaluated
against the first three migrations (V0001 / V0010 / V0020) and shown to
provide no advantage over a "raw SQL file runner" once AGE is in the picture.

---

## Context — what the spike actually compared

Phase 0 W1 has six versioned migrations to ship (V0001, V0010, V0020, V0025,
and V0030 here; future plans add more). They split into two flavours:

| Migration | Content | Atlas-representable? |
|-----------|---------|----------------------|
| `V0001__enable_extensions.sql` | `CREATE EXTENSION age`, `CREATE EXTENSION pgaudit`, `CREATE EXTENSION pg_stat_statements` | YES — extensions are first-class objects in Atlas HCL (`extension "age" {}`). |
| `V0010__create_graph_and_labels.sql` | `LOAD 'age'` + `SELECT create_graph('neksur')` + 19 × `create_vlabel(...)` + 24 × `create_elabel(...)` + a `DO $$ ... $$` verification block | **NO** — Atlas HCL has no concept of "AGE graph" or "AGE vlabel/elabel". Forces use of `migration { sql = file(...) }` raw-SQL escape hatch. |
| `V0020__property_indexes.sql` | 11 × `create_property_index(...)` + 3 × `create_property_index_edge(...)` + 2 × `CREATE INDEX` | **PARTIAL** — the `CREATE INDEX` statements are HCL-representable (`index` blocks on `table` objects), but the AGE-specific `create_property_index*` calls are not. Same `migration { sql = ... }` escape hatch needed. |
| `V0025__tenant_indexes_and_gin.sql` | 38 × `CREATE INDEX` (btree + GIN) on `neksur."<Label>"` tables | PARTIAL — the schema introspection misses AGE tables; Atlas's diff would offer to drop them on every plan-generate. Must declare them out-of-band. |
| `V0030__rls_policies.sql` (Task 2b) | 43 × ENABLE RLS + 43 × FORCE RLS + 172 × CREATE POLICY + 43 × ADD CONSTRAINT, generated via plpgsql DO loop | NO — RLS policies on AGE-managed tables are not representable in Atlas HCL's `policy` block (the underlying tables are extension-owned). Raw SQL only. |

**Spike result:** 4 of 5 migrations need Atlas's `migration { sql = ... }`
raw-SQL escape hatch. Atlas's declarative HCL is unusable for everything
except `V0001`. We would essentially be shipping a directory of raw SQL
files **and** a separate Atlas HCL definition that excludes everything
AGE-managed (otherwise Atlas's introspector keeps offering to DROP AGE
tables on every run).

## Decision rule (from PLAN 00-02 Task 2a)

> "If Atlas requires more than a single `data` block per migration (i.e.,
> needs custom HCL extensions beyond `sql` blocks), declare sqitch the
> winner."

Hit: 4 of 5 migrations need it, and `V0030` cannot avoid it. **sqitch wins.**

## Why sqitch is the right shape

| Property | sqitch | Atlas (forced into file-runner mode) |
|----------|--------|--------------------------------------|
| First-class non-declarative DDL | YES — every change is `deploy/verify/revert` scripts; no schema introspection assumed | NO — fights you when it sees AGE tables it doesn't know about |
| Revert scripts per change | YES — `sqitch revert` is the standard workflow | Atlas has `down` migrations but they don't sit alongside `up` in the same file the way sqitch does |
| Plan-file ordering / dependencies | YES — `sqitch.plan` has explicit `requires` syntax | Atlas migration directory is purely lexicographic |
| Postgres-native (no Go binary) | YES — pgxn-installable, Perl-based; battle-tested at scale (CrunchyData ships it) | Single static Go binary — but binary distribution is not a feature we need at the cost of feature-fit |
| AGE-aware? | No tool is AGE-aware; sqitch doesn't pretend to be (no introspection mode) | Atlas pretends to be schema-aware, then breaks on AGE |

## Observed friction (concrete)

1. **`LOAD 'age'`** must be the first statement of any Cypher-touching
   migration. Atlas migration files can contain this, but Atlas's
   `inspect` mode does not — meaning any Atlas diff workflow against a
   migrated database will fail to load AGE first and report spurious
   "ag_catalog does not exist" errors. sqitch never introspects, so the
   class of error doesn't exist.
2. **Atlas's `inspect` of an AGE-populated database** offers to DROP the
   `_ag_label_vertex` and `_ag_label_edge` partitions (Atlas sees them as
   "orphaned tables not described in HCL"). We would have to maintain a
   `tables[exclude_pattern]` list, which is exactly the kind of inverted-
   gravity HCL Phase 0 should avoid for a tool with this much complexity.
3. **DO $$ ... $$ verification blocks** inside migrations (Task 1's
   pattern) work fine in sqitch's `deploy` scripts; in Atlas they require
   wrapping the migration body inside a heredoc-style HCL string, doubling
   the escaping pain.

## What the runner does

`infra/migrations/run-migrations.sh` invokes `sqitch deploy --target $DB_URL`.
It does NOT invoke Atlas. Atlas is not installed on the CI image.

`infra/migrations/atlas.hcl.rejected` is retained with this header for
traceability and to make the W1 spike auditable; it is NOT used at runtime.

## Reversibility

If a future phase finds Atlas a better fit (e.g., once we have real
relational schemas in `neksur_app_*` that benefit from HCL declarative
mode, with the AGE migrations factored out via `migration_directory`),
this decision can be revisited. Until then sqitch is the floor.

---

## See also

- 00-RESEARCH.md §Open Questions #2 — Atlas vs sqitch (original question)
- 00-RESEARCH.md §Standard Stack — Atlas / sqitch / Flyway / Liquibase tradeoffs
- 00-RESEARCH.md §Common Pitfalls — Pitfall 7 (AGE label DDL non-transactional;
  drives the DO-block verification pattern)
- `infra/migrations/run-migrations.sh` — the runner that invokes sqitch
