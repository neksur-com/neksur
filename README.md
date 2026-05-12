# Neksur

> **The Open Lakehouse Control Plane for Apache Iceberg.**
> Three coordinated capabilities across Spark, Trino, Snowflake, and Dremio: **Semantic Consistency**, **Runtime Coordination**, and **Policy Enforcement**.

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-blue.svg)](LICENSE)
[![Change Date: 2030-05-10](https://img.shields.io/badge/Change%20Date-2030--05--10-green.svg)](LICENSE)
[![Status: Pre-MVP](https://img.shields.io/badge/Status-Pre--MVP-orange.svg)](#status)

---

## What This Is

Neksur is a **Control Plane** that sits on top of any Apache Iceberg lakehouse and delivers three coordinated capabilities:

1. **Semantic Consistency** — bit-identical metric and dimension semantics across every query engine. Semantic layer with per-engine dialect compilation, OSI roundtrip-stable import/export, AST as source of truth.
2. **Runtime Coordination** — snapshot pinning, schema cache invalidation, write-conflict resolution, partition-spec versioning, and compaction coordination across heterogeneous engines that write and read the same Iceberg tables.
3. **Policy Enforcement** — one declarative policy (row filter, column mask, RBAC, ABAC) compiled and enforced identically when Spark writes and Trino, Snowflake, or Dremio read. Write-path enforcement is structured as four additive levels (catalog gateway, writer-side transformation, post-commit detection, credential vending) per [ADR-003](#design-decisions).

AI agents access the same governed metadata graph through MCP (Model Context Protocol), under the same policy contract.

## Why It Exists

Open lakehouses are almost always multi-engine in real production: Spark and Flink on the write side, Trino, Dremio, and Snowflake on the read side, with AI agents on top. Native catalogs (Databricks Unity, Snowflake Horizon, Polaris RBAC) enforce policies only on their own compute. When external engines read the same Iceberg tables through the REST API, **row filters and column masks are not enforced** — a [documented limitation](https://docs.databricks.com/aws/en/data-governance/unity-catalog/filters-and-masks/) of Unity Catalog (April 2026) and a structural property of every platform-native solution.

Neksur closes this gap: one declarative governance contract, enforced uniformly across every engine, with semantic and coordination guarantees baked in.

## Status

**Pre-MVP / Discovery (as of 2026-05-12).** Architecture is locked in ADRs; implementation is in Phase 0 (Metadata Graph Foundation). This repository will become populated over the course of Phase 0 and Phase 1. Expect significant churn.

If you are a prospective design partner, see [`hello@neksur.com`](mailto:hello@neksur.com).

## Architecture (Phase 0 contract)

| Layer | Choice | Source |
|---|---|---|
| Metadata graph | **Apache AGE 1.6.0 on Postgres 16** | ADR-001 §3 |
| Query language | openCypher | ADR-001 D-001.02 |
| Storage discipline | Hybrid LPG (graph holds references <2KB/node; bulk data in JSONB / relational) | ADR-001 D-001.04 |
| HA | Patroni + etcd + HAProxy (3-node), <30s failover | Phase 0 plan |
| Backup / DR | pgBackRest + WAL streaming, RTO 1h / RPO 15min | D-001.13 amended by D-OQ.04 |
| Observability | OpenTelemetry → Prometheus → AlertManager → PagerDuty | D-001.14 |
| Multi-tenancy | Single AGE graph + `tenant_id` property + Postgres RLS (FORCE) | ADR-001; D-SPEC.09 |
| Write-path enforcement | 4-level defense-in-depth (L1 + L2 + L3 + L4) | ADR-003 |
| Runtime | Python 3.12 | Phase 0 W0 decision |

Iceberg catalog interface is Polaris (reference) with adapter model for Glue, Unity, Snowflake Horizon, and Nessie. Lineage via OpenLineage. Semantic interchange via OSI (Open Semantic Interchange, January 2026 standard). AI interface via MCP.

## License

Neksur Core is licensed under the **Business Source License 1.1** (BSL 1.1). The full license text is in the [`LICENSE`](LICENSE) file. A human-readable summary is in [`LICENSE.md`](LICENSE.md).

**Key terms:**

- **Source-available**, not OSI-defined open source. Anyone can read, modify, and use the code for almost any purpose.
- **Change Date: 2030-05-10** (four years from ratification of ADR-002). On that date, the source becomes licensed under **Apache License, Version 2.0** — fully open source — automatically and irrevocably.
- **Additional Use Grant:** you may use Neksur Core for almost anything, **except** offering it (in whole or in part) as a managed or hosted service that competes with Neksur's commercial offerings on cross-engine policy enforcement, semantic consistency, or runtime coordination over open lakehouses. This blocks hyperscalers from packaging Neksur Core as a service while it is under BSL; it does **not** block any other use — internal production, modification, redistribution as part of your own (non-competing) product, or research.

For premium features (cross-engine RLS / column-masking enforcement, Multi-Engine L2/L3, compliance bundles, ML anomaly detection, air-gapped distribution, advanced FinOps, write-path enforcement levels L1-advanced / L2 / L3 ML / L4), see the separate `neksur-com/neksur-premium` repository (private) or contact `hello@neksur.com`.

## Contributing

We use the **Developer Certificate of Origin (DCO 1.1)** for contributions — no CLA. Sign your commits with `git commit -s`. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full process.

Code of conduct: [Contributor Covenant 2.1](CODE_OF_CONDUCT.md). Security reports: [`SECURITY.md`](SECURITY.md).

## Repository Map

This is the **Neksur Core** repository (BSL 1.1, source-available). Related repositories under the `neksur-com` organization:

| Repository | Visibility | License | Purpose |
|---|---|---|---|
| `neksur-com/neksur` (this repo) | public | BSL 1.1 → Apache 2.0 in 2030 | Core source: semantic engine, SQL proxy, basic catalog, OpenLineage, OSI, multi-engine L1, MCP basic, AGE schemas, Web UI core, L1 basic catalog gateway, L3 basic regex detection |
| `neksur-com/neksur-premium` | private | Neksur Commercial License | Commercial Premium components |
| `neksur-com/docs` | public | Apache 2.0 | Public documentation (docs.neksur.com) |

## Design Decisions

Architecture decisions are captured as ADRs. The locked decisions in force as of 2026-05-12:

- **ADR-001** — Metadata Graph Foundation (Apache AGE on Postgres 16, openCypher canonical, hybrid storage, 19 canonical node labels + 24 edge labels)
- **ADR-002** — Licensing & Open Source Strategy (BSL 1.1 Core + Commercial Premium, DCO contributor model, four-year Change Date)
- **ADR-003** — Write-Path Policy Enforcement Architecture (four-level defense-in-depth: catalog gateway + writer-side transform + post-commit detection + credential vending)
- **ADR-004** — MCP `graph.cypher` Hardening Contract (Phase 5, planned)

ADRs are currently maintained in the private planning repository while the project is pre-MVP; they will be published to `docs.neksur.com/architecture/adrs/` ahead of public design-partner engagement.

## Contact

- **General:** `hello@neksur.com`
- **Security:** `security@neksur.com`
- **Code of Conduct concerns:** `conduct@neksur.com`
- **Commercial / licensing inquiries:** `hello@neksur.com`

---

*Neksur Core — pre-MVP scaffolding initialized 2026-05-12.*
