# CLAUDE.md — guidance для AI-assisted development в Neksur

## Project context

Neksur — Open Lakehouse Governance Plane. Cross-engine policy enforcement
для Apache Iceberg lakehouses. Per `docs/decisions/` ADRs and the
authoritative Phase 0 constraint document `docs/phase-0-stack.md`.

## Phase scope (CRITICAL)

We're in **Phase 0 (M1-M4)**. Do **not** add features outside Phase 0 scope
без explicit approval. См. `docs/phase-0-stack.md` для definitive list.

If you're about to:

- Add new dependency
- Introduce new language
- Add new external service
- Build a feature not in this milestone

→ **Stop, ask, document justification**.

## Language constraints

- **Backend:** Go only. Не Rust, не Java, не Python.
- **Spark code:** Scala (in separate repo `neksur-com/neksur-spark`).
- **Python SDK:** Python (in separate repo `neksur-com/neksur-python`).
- **Frontend:** TypeScript + React + Tailwind (under `web/`, M1+).

## Architectural constraints

- **Storage:** PostgreSQL 16 + Apache AGE. Никакой Redis (unless benchmark
  shows need), никакой Kafka, никакой OpenSearch.
- **Catalog:** Polaris first. Other catalogs — Phase 1+.
- **Engine:** Trino first (read), Spark next (write через Extension в M3+).
- **Policy:** OPA + Rego, embedded as Go library.
- **Standards:** Iceberg REST Catalog API, OpenLineage, MCP, OpenTelemetry,
  OpenAPI 3.0, pgwire.

## Code style

- **Go:** standard formatting (`gofmt`), idiomatic Go (review Effective Go).
- **Errors:** wrapped errors с context, no panics в production paths.
- **Logging:** structured (slog), correlation IDs propagated через context.
- **Testing:** table-driven tests, integration tests с real Postgres + AGE
  через testcontainers-go, no mocks для critical paths.
- **License headers:** BSL 1.1 в каждом .go file (deferred to a separate
  hygiene PR; the repo's root `LICENSE` file governs in the meantime).

## Critical rules

1. **Never commit secrets.** Use `.env.example`, real `.env` в `.gitignore`.
2. **BSL license** в каждом file header (eventually). Premium code goes к
   `neksur-com/neksur-premium` (separate repo, private).
3. **Test before commit** — `make test` passes.
4. **One PR, one logical change** — readable git history matters.
5. **Spec changes need ADR amendment** — не silently diverge от
   spec/ADR/constraint document.

## What to defer (do NOT build in Phase 0)

См. `docs/phase-0-stack.md` §4 "Anti-stack". Common temptations:

- GraphQL API — NO, use REST.
- gRPC между services — NO, мы monolith в Phase 0.
- Kafka — NO, Postgres queue достаточно.
- OpenSearch — NO, Postgres FTS достаточно.
- Rust components — NO, Go достаточно.
- Microservices split — NO, modules in monolith.
- ABAC — NO, RBAC only в Phase 0.
- Multiple catalogs (Unity, Glue) — NO, Polaris only.

## When in doubt

- Re-read `docs/phase-0-stack.md` (the Phase 0 implementation constraint).
- Re-read relevant ADR (`docs/decisions/`).
- Default: simpler is better для bootstrap stage.

## D-PHASE0-stack note (2026-05-13)

The initial Wave 0 + Wave 1 of Phase 0 were executed in Python 3.12 (planning
error: 00-RESEARCH.md Open Question #5 suggested Python "if no other signal
exists" but missed the constraint document). The founder intake on
2026-05-13 ratified `docs/phase-0-stack.md` as authoritative and the
corrective executor moved the work to Go. See
`docs/decisions/D-W0-runtime-pick.md` (the LOCKED Go decision) and the
`CORRECTION-NOTE-2026-05-13.md` in the planning tree for the full mapping.

The 7 reality-vs-ADR-001 SQL/Postgres deviations from the Python-era Wave 1
are language-neutral and survive into the Go monorepo unchanged:

1. AGE auto-creates synthetic `_ag_label_*` labels — filter with `name NOT LIKE`.
2. AGE 1.6.0 lacks `create_property_index{,_edge}` — polyfilled in V0020.
3. agtype-correct property access requires `::text` casts (not jsonb form).
4. `text::timestamptz` is STABLE — `idx_snapshot_time` indexes text directly.
5. pgaudit is not in the base apache/age image — V0001 conditionally installs.
6. `SET LOCAL <name> = $1` is a parse error — use `set_config(name, value, true)`.
7. Postgres superusers bypass RLS unconditionally — tests use `neksur_app`
   non-superuser role, and `ALTER TABLE ... OWNER TO neksur_app` is used to
   probe the FORCE RLS owner-bypass attack surface.
