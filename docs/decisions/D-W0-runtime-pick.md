# D-W0-runtime-pick — Phase 0 Runtime: Go

**Status:** LOCKED (2026-05-13 — supersedes prior Python 3.12 lock)
**Date:** 2026-05-13
**Phase:** 0 — Metadata Graph Foundation
**Wave:** 0 (foundation)
**Decided by:** Founder, per `docs/phase-0-stack.md` §2.1 + §7 (CLAUDE.md template)
**Supersedes:** D-W0-runtime-pick (prior version locked Python 3.12 on 2026-05-12 — was a planning error; corrected per constraint document `neksur-phase0-stack.md` v0.1 founder intake 2026-05-13)
**Superseded by:** none

## Context

Phase 0 builds the metadata graph foundation on Apache AGE 1.6.0 / PostgreSQL 16
(ADR-001 locks the storage stack via D-001.01..15). The Phase 0 implementation
constraint document `docs/phase-0-stack.md` §2.1 explicitly locks **Go as the
single backend language** for the monorepo: Catalog Gateway (L1 enforcement per
ADR-003), Semantic Engine, Policy Service (OPA embedded as Go library), SQL
Proxy (pgwire), MCP Server, REST API, and L3 Detection Worker all live in
`internal/` of `neksur-com/neksur` and share one toolchain, one CI pipeline,
one observability setup, one hire profile.

Anti-stack rules in `docs/phase-0-stack.md` §4 explicitly forbid Rust, Java,
Python (for the backend), GraphQL, gRPC, microservices, and other technology
sprawl. Cross-language artifacts are isolated to their own repos: Spark
extension in `neksur-com/neksur-spark` (Scala, M3+), Python SDK in
`neksur-com/neksur-python` (Python, M3+).

## Decision

**Go 1.24+ is the runtime for the Phase 0 backend.**

Rationale:

- **`docs/phase-0-stack.md` §2.1 is authoritative** — locks Go as the single
  backend language for all services in the monorepo. The constraint document
  exists specifically to prevent scope creep and tech sprawl during the
  bootstrapped Phase 0.
- **High-throughput, low-latency network-heavy profile** — Catalog Gateway (L1
  REST proxy), SQL Proxy (pgwire), MCP Server are all network-bound services.
  Go is the industry standard for this profile (Kubernetes, etcd, Patroni,
  Polaris, OPA, Sourcegraph, HashiCorp — all Go).
- **OPA is a Go library** — embedding OPA + Rego for policy evaluation is a
  one-import-line affair in Go. Any other language would require gRPC or HTTP
  embedding of OPA, adding latency and operational complexity.
- **Single language, single dependency manager (go modules), single test
  framework (`go test`), single CI workflow.** Matches the constraint
  document's bootstrap discipline.
- **AGE driver:** `jackc/pgx/v5` connects to Postgres + AGE without issue; AGE
  Cypher queries are sent as parameterized SQL strings calling `cypher(...)`.
  No language-specific AGE library is needed; the driver is pure pgx.

Concretely: `go.mod` with `go 1.24` directive. Monorepo layout per
`docs/phase-0-stack.md` §6: `cmd/{neksur-server,neksur-worker,neksur-cli}/`,
`internal/{gateway,policy,sqlproxy,semantic,graph,catalog,engines,lineage,detection,api,audit,observability}/`,
`pkg/types/`, `web/` (TypeScript+React+Tailwind), `tests/integration/`.

## Consequences

**Positive:**

- Single toolchain (`go test`, `go build`, `golangci-lint`) — fast, no venv
  drift, native cross-compilation.
- OPA + Rego embedded natively, no IPC overhead.
- Standard library covers the entire Phase 0 surface (`net/http`, `encoding/json`,
  `database/sql`); minimal third-party dependency surface.
- Aligns with constraint document and CLAUDE.md guidance — future AI work has a
  hard guardrail.
- Hire profile: Go backend engineers are abundant and culturally aligned with
  bootstrap-stage infrastructure work.

**Negative / accepted:**

- Initial Wave 0 + Wave 1 of Phase 0 were executed in Python 3.12 (planning
  error: 00-RESEARCH.md Open Question #5 suggested Python "if no other signal
  exists" but missed the constraint document that locks Go). Correction
  applied 2026-05-13 — Python artifacts removed, Go monorepo introduced. The 7
  reality-vs-ADR-001 deviations from the initial Wave 1 (agtype casts,
  polyfilled `create_property_index` / `create_property_index_edge` functions,
  `set_config()` over `SET LOCAL`, non-superuser `neksur_app` for RLS testing)
  are preserved — they are at the SQL/Postgres layer, language-neutral.
- AGE-Python AGEFreighter (the original rationale-anchor for Python) is not
  available in Go. Phase 0 bulk seeding (Plan 00-06 W5) uses pure SQL COPY +
  Cypher MERGE through pgx — proven approach, just without the AGEFreighter
  helper. The Phase 0 acceptance gate verifies the seed approach independently.

**Neutral:**

- Dev dependency surface in `go.mod` will accumulate as Phase 0 progresses;
  versions pinned via `go.sum`. Standard `go mod tidy` + Renovate/Dependabot
  in Phase 1 for upgrade hygiene.
- The Python SDK in `neksur-com/neksur-python` (M3+) is a separate concern; it
  is a customer-facing client library, not part of the Phase 0 backend
  decision.

## References

- `docs/phase-0-stack.md` §2.1 (single backend language), §6 (repo layout), §7
  (CLAUDE.md template with language constraints)
- ADR-001 (D-001.01..15) — storage stack lock (language-agnostic at the DB
  layer)
- ADR-002 D-002.05/.06 — BSL Core / Commercial boundary
- ADR-003 D-003.01..06 — write-path enforcement architecture
- Prior version of this ADR (Python 3.12) — corrected 2026-05-13, retained as
  git history at commit `5318414` for traceability
