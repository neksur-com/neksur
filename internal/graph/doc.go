// Package graph is the Phase 0 application gateway for every Postgres + AGE
// 1.6.0 Cypher call. It enforces the Phase 0 hardening contract — the
// "floor" that Phase 5 ADR-004 will layer MCP-aware hardening on top of:
//
//  1. Parameterised Cypher. GraphClient.Cypher binds caller-supplied values
//     via jackc/pgx's positional placeholders ($1, $2, ...). No string
//     concatenation. Verified by tests/security/cypher_injection_test.go.
//
//  2. Label whitelist. IsAllowedLabel(name) checks membership against
//     LabelWhitelist — the LOCKED set of 43 identifiers from D-001.05
//     (19 vlabels) + D-001.06 (24 elabels), as AMENDED by D-003.06
//     (adds WriteEvent, DetectionRun, INTENDED_WRITE, ACTUAL_WRITE,
//     VIOLATION_DETECTED_BY for the write-path enforcement architecture).
//
//  3. Tenant context. ExecuteInTenant opens a transaction, calls
//     set_config('app.current_tenant', $1, true) — bind-safe and
//     transaction-scoped — runs the user callback, commits, and triggers
//     ResetSession (DISCARD ALL equivalent) on the pool-return boundary
//     to prevent session-var bleed (Pitfall 5 / T-0-SESS).
//
//  4. D-001.08 depth cap. ValidateTraversalDepth runs BEFORE the query
//     reaches Postgres, rejecting bare `*`, `*N..`, and `*..` Cypher
//     variable-length-traversal patterns. Bounded forms (`*N`, `*N..M`,
//     `*..M`) pass through to AGE. Even an AGE planner regression cannot
//     turn a missed depth cap into a tenant-wide DoS (T-0-DOS).
//
// All four surfaces map 1:1 to the Python-era Wave 1 implementation
// removed in the 2026-05-13 D-PHASE0-stack correction; the underlying
// SQL/Postgres behaviour is unchanged and the 7 reality-vs-ADR-001
// deviations from the Python tier are preserved (agtype casts, polyfilled
// create_property_index{,_edge}, set_config over SET LOCAL, non-superuser
// neksur_app role for RLS testing, graphid type cast, AGE catalog phantom
// label filter, idx_snapshot_time STABLE timestamptz workaround). See
// .planning/phases/00-metadata-graph-foundation/00-02-SUMMARY.md
// "Deviations from Plan" for the canonical reference.
//
// References:
//   - docs/phase-0-stack.md §2.1 (Go monorepo lock; D-PHASE0-stack)
//   - docs/decisions/D-W0-runtime-pick.md (Go runtime lock 2026-05-13)
//   - docs/decisions/D-W1-migration-tool.md (sqitch lock; migration runner)
//   - ADR-001 D-001.05 / D-001.06 amended by D-003.06 (label set)
//   - ADR-001 D-001.08 (depth cap)
//   - D-OQ.03 (Phase 5 cypher hardening; ADR-004 will layer on top)
package graph
