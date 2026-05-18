# Neksur

**The Data Contract Plane for Open Lakehouses.**

*One contract — every engine, every agent.*

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-blue.svg)](LICENSE)
[![Change Date: 2030-05-10](https://img.shields.io/badge/Change%20Date-2030--05--10-green.svg)](LICENSE)
[![Status: Pre-MVP](https://img.shields.io/badge/Status-Pre--MVP-orange.svg)](#status)

---

## What This Is

Neksur is the **Data Contract Plane** that sits on top of any Apache Iceberg lakehouse and presents one canonical, versioned object — the **Data Contract** — to every consumer of a dataset.

One **Data Contract** per dataset has three **coupled** dimensions:

1. **Meaning** — what the data is. Business identity, metrics, dimensions, semantic rules, OSI representation.
2. **Access** — who can see what. RBAC, ABAC, row filters, column masks, retention, consumer scope.
3. **State** — the physical and temporal form of the data. Snapshot, schema version, partition spec, branch, freshness.

The Contract is enforced identically across every consumer of the dataset: Spark, Trino, Snowflake, Dremio, Flink, and AI agents reading the same governed metadata graph through MCP (Model Context Protocol).

Every Contract change passes through one canonical lifecycle, applied uniformly to all three dimensions: `draft → review → compile → deploy → enforce → audit`.

Three buyer jobs are done against a Contract:

- **Define** — author and version the Contract along all three dimensions.
- **Enforce** — guarantee the Contract holds at write and at read on every engine and agent.
- **Prove** — produce auditable evidence that the Contract held over a period, per dataset and per consumer.

## Why It Exists

Open lakehouses are almost always multi-engine in real production: Spark and Flink on the write side, Trino, Dremio, and Snowflake on the read side, with AI agents on top. Native catalogs (Databricks Unity, Snowflake Horizon, Polaris RBAC) enforce policies only on their own compute. When external engines read the same Iceberg tables through the REST API, **row filters and column masks are not enforced** — a [documented limitation](https://docs.databricks.com/aws/en/data-governance/unity-catalog/filters-and-masks/) of Unity Catalog (April 2026) and a structural property of every platform-native solution.

The Data Contract Plane closes this gap: one Contract, enforced identically across every engine and every agent, with Meaning, Access, and State held in one object and one lifecycle.

## Status

**Pre-MVP / Discovery (as of 2026-05-18).** Architecture is locked in ADRs; implementation is in Phase 0 (Metadata Graph Foundation). This repository will become populated over the course of Phase 0 and Phase 1. Expect significant churn.

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
| Write-path enforcement | Four-guarantee Defense-in-Depth Ladder: Catalog-level enforcement, Write-path enforcement, Continuous compliance scan, Compute isolation | ADR-003 |
| Runtime | Python 3.12 | Phase 0 W0 decision |

Internal engineering modules: `policy-engine`, `semantic-engine`, `catalog-gateway`. These names appear in source code and runbooks; they are not customer-facing positioning.

Iceberg catalog interface is Polaris (reference) with adapter model for Glue, Unity, Snowflake Horizon, and Nessie. Lineage via OpenLineage. Semantic interchange via OSI (Open Semantic Interchange, January 2026 standard). AI interface via MCP.

## License

Neksur Core is licensed under the **Business Source License 1.1** (BSL 1.1). The full license text is in the [`LICENSE`](LICENSE) file. A human-readable summary is in [`LICENSE.md`](LICENSE.md).

**Key terms:**

- **Source-available**, not OSI-defined open source. Anyone can read, modify, and use the code for almost any purpose.
- **Change Date: 2030-05-10** (four years from ratification of ADR-002). On that date, the source becomes licensed under **Apache License, Version 2.0** — fully open source — automatically and irrevocably.
- **Additional Use Grant:** you may use Neksur Core for almost anything, **except** offering it (in whole or in part) as a managed or hosted service whose primary value proposition is enforcement of Data Contracts (Meaning, Access, or State) across multi-engine open lakehouses. This blocks hyperscalers from packaging Neksur Core as a service while it is under BSL; it does **not** block any other use — internal production, modification, redistribution as part of your own (non-competing) product, or research.

## Tiers — the Defense-in-Depth Ladder

Neksur packages the Data Contract Plane in a four-tier **additive** ladder. Each tier answers one buyer question; each tier **includes** every tier below it. You pick the rung that matches your auditors.

- **Core** (BSL 1.1) — One Contract on one engine, with Catalog-level enforcement. *"I want to try the model end-to-end."*
- **Multi-Engine** (Commercial) — The same Contract enforced identically on Spark + Trino + at least one of Snowflake / Dremio / Flink. The wedge tier — the answer to Unity Catalog's documented April 2026 limitation. *"Our production is multi-engine, and the Contract must hold everywhere."*
- **Defense-in-Depth** (Commercial) — Adds Write-path enforcement, Continuous compliance scan (regex), Compute isolation. *"Our auditors require defense in depth on the write path."*
- **Intelligence** (Commercial) — Adds ML-based classification and anomaly detection, semantic anomaly detection over Contracts, AI-agent observability. *"We want detection and proactive governance, not just enforcement."*

Tiers are **additive**, not alternative. Defense-in-Depth includes Multi-Engine; Intelligence includes Defense-in-Depth. The customer picks the top of the stack they need.

For licensing details on Commercial tiers, contact `hello@neksur.com` or see the separate `neksur-com/neksur-premium` repository (private).

### Data Contract Plane vs dbt-style "data contracts"

The phrase "data contract" is used in the dbt ecosystem to describe YAML schema declarations in a repo. A Neksur Data Contract is a different object: it is **runtime-enforced** across every engine and agent that consumes the dataset, and it spans three coupled dimensions (Meaning, Access, State) — not a column-list schema check. dbt-style data contracts can live inside a Neksur Contract's Meaning dimension; the Contract is the larger, multi-dimensional, multi-engine, lifecycle-managed object.

## Contributing

We use the **Developer Certificate of Origin (DCO 1.1)** for contributions — no CLA. Sign your commits with `git commit -s`. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full process.

Code of conduct: [Contributor Covenant 2.1](CODE_OF_CONDUCT.md). Security reports: [`SECURITY.md`](SECURITY.md).

## Repository Map

This is the **Neksur Core** repository (BSL 1.1, source-available). Related repositories under the `neksur-com` organization:

| Repository | Visibility | License | Purpose |
|---|---|---|---|
| `neksur-com/neksur` (this repo) | public | BSL 1.1 → Apache 2.0 in 2030 | Neksur Core: Contract authoring + compile pipeline, semantic engine (Meaning), Catalog-level enforcement (Access), basic catalog gateway, OpenLineage, OSI roundtrip, Multi-Engine read path, MCP basic, AGE schemas, Web UI core, Continuous compliance scan (regex) |
| `neksur-com/neksur-premium` | private | Neksur Commercial License | Commercial tiers above Core — Multi-Engine write path, Defense-in-Depth (Write-path enforcement, Continuous compliance scan, Compute isolation), Intelligence (ML classification + anomaly detection, semantic anomaly detection, AI-agent observability) |
| `neksur-com/docs` | public | Apache 2.0 | Public documentation (docs.neksur.com) |

## Design Decisions

Architecture decisions are captured as ADRs. The locked decisions in force as of 2026-05-18:

- **ADR-001** — Metadata Graph Foundation (Apache AGE on Postgres 16, openCypher canonical, hybrid storage, 19 canonical node labels + 24 edge labels)
- **ADR-002** — Licensing & Open Source Strategy (BSL 1.1 Core + Commercial, DCO contributor model, four-year Change Date)
- **ADR-003** — Write-Path Policy Enforcement Architecture (four-level defense-in-depth: catalog gateway + writer-side transform + post-commit detection + credential vending). External presentation per ADR-011 §3.7.
- **ADR-004** — MCP `graph.cypher` Hardening Contract (Phase 5, planned)
- **ADR-011** — Product Concept & Terminology Unification (Ratified 2026-05-18). The Data Contract Plane for Open Lakehouses.

ADRs are currently maintained in the private planning repository while the project is pre-MVP; they will be published to `docs.neksur.com/architecture/adrs/` ahead of public design-partner engagement.

## Contact

- **General:** `hello@neksur.com`
- **Security:** `security@neksur.com`
- **Code of Conduct concerns:** `conduct@neksur.com`
- **Commercial / licensing inquiries:** `hello@neksur.com`

---

*Neksur Core — pre-MVP scaffolding initialized 2026-05-12.*
