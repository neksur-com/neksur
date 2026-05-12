# AGE / Postgres Naming Convention

> Status: LOCKED for Phase 0 — referenced by every migration in `migrations/`.
> Source: 00-RESEARCH.md §Common Pitfalls — Pitfall 1 "AGE schema vs Postgres
> schema confusion."

## TL;DR

| Concept | Name | Where it lives | Created by |
|---------|------|----------------|------------|
| **AGE graph** | `neksur` | `ag_catalog.ag_graph` | `SELECT create_graph('neksur')` in `V0010` |
| **AGE label tables (vertex/edge)** | `neksur."Table"`, `neksur."Column"`, … | Postgres schema `neksur` (auto-created by AGE under the graph name) | `SELECT create_vlabel('neksur', 'Table')` etc. in `V0010` |
| **AGE catalog** | `ag_catalog.*` | Postgres schema `ag_catalog` (extension-owned) | `CREATE EXTENSION age` |
| **Application schemas (future)** | `neksur_app_*` (e.g., `neksur_app_audit`, `neksur_app_envelope`) | Postgres schema named with the `neksur_app_` prefix | Future plans (Phase 1+) |

## Why this matters (Pitfall 1)

AGE's `create_graph('neksur')` does TWO things:

1. Adds a row to `ag_catalog.ag_graph` (the AGE metadata of "there is a graph
   named `neksur`").
2. Creates a Postgres schema (namespace) also called `neksur` that holds the
   per-label tables (`neksur."Table"`, `neksur."Column"`, …, plus the
   underlying `_ag_label_vertex` / `_ag_label_edge` partitions).

This means **the AGE graph name and the Postgres schema name are the same
identifier** — they are not separable. Operators sometimes try to write
`ALTER SCHEMA neksur OWNER TO ...` or `DROP SCHEMA neksur CASCADE` thinking
they are operating on a generic schema; in reality they are mutating AGE's
own catalog. Conversely, an RLS policy on `neksur."Table"` IS the right
target — that is the Postgres table where the vertex rows actually live.

## The rule

- **Graph name** = `neksur` (singular, lowercase, no namespacing).
- **Postgres schema for AGE label tables** = `neksur` (forced by AGE).
- **Application Postgres schemas** = `neksur_app_*` prefix. NEVER reuse
  `neksur` itself for non-AGE-managed tables. The prefix gives `pg_dump`,
  `pgaudit`, and operator scripts a clear demarcation.

## Cross-references

- `migrations/postgres/V0001__enable_extensions.sql` — installs `age`,
  preconditions for the graph to exist.
- `migrations/graph/V0010__create_graph_and_labels.sql` — creates the
  `neksur` graph and the 19 vlabels + 24 elabels.
- `migrations/postgres/V0030__rls_policies.sql` — applies RLS to
  `neksur."<Label>"` tables (one row per of the 43 label tables).
- `infra/postgres/postgresql.base.conf` — `shared_preload_libraries =
  'age,pgaudit,pg_stat_statements'` (the extension load order; Pitfall 9
  also addresses this for post-failover startup).
