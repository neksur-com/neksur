# Neksur — Phase 0 Implementation Stack

**Документ:** Phase 0 Implementation Constraint
**Версия:** 0.1
**Дата:** 12 мая 2026
**Статус:** Active constraint — referenced from CLAUDE.md в repo
**Audience:** founding engineers начинающие implementation в Claude Code
**Related:** `neksur-spec-v0.7.md`, `neksur-graph-foundation-adr.md` (ADR-001), `neksur-licensing-adr.md` (ADR-002), `neksur-write-path-enforcement-adr.md` (ADR-003)

> **Назначение этого документа.** Spec v0.7 описывает **полное product vision** (M1-M24+, четыре фазы развития). Этот документ описывает **минимальный достаточный стек для Phase 0 (M1-M4)** — то есть только то, что **реально нужно build** в первые четыре месяца. Если что-то упомянуто в spec, но **отсутствует здесь** — оно **намеренно отложено** до Phase 1+. Цель: предотвратить scope creep и tech sprawl, которые убивают bootstrapped startups.

> **Правило использования.** При implementation в Claude Code задавайте вопрос: «is this in Phase 0 stack? if not, defer.» Если возникает соблазн добавить новую технологию — flag в этом документе с justification, а не add silently.

---

## 1. Phase 0 — что мы строим (single sentence)

**Phase 0 deliverable: customer's Spark/Trino read Iceberg-таблицы через Neksur, и cross-engine row filter / column mask политика применяется на обоих движках — с документированным compliance gap closure.**

Не "MVP всего". Не "все features из spec". Именно **этот** end-to-end flow, демонстрируемый дизайн-партнёру. Всё остальное — Phase 1+.

---

## 2. Stack — what's in, what's NOT in Phase 0

### 2.1 Backend services

| Component | Phase 0? | Language | Justification |
|---|---|---|---|
| **Catalog Gateway** (Iceberg REST proxy + policy validation) | ✅ Yes | **Go** | Core component (L1 enforcement per ADR-003). High-throughput, low-latency, network-heavy. Go is industry standard для такого профиля. |
| **Semantic Engine** (compiler YAML→SQL для engines) | ✅ Yes | **Go** | Same monorepo. Compilation is CPU work — Go достаточно, Rust overkill для Phase 0. |
| **Policy Service** (OPA integration) | ✅ Yes | **Go** + OPA как embedded library | OPA shipped как Go library. Tight integration. |
| **SQL Proxy** (pgwire frontend для read-path) | ✅ Yes | **Go** | Network service, latency-sensitive. |
| **MCP Server** | ✅ Yes | **Go** | Same monorepo. MCP-Go SDK от Anthropic. |
| **REST API** | ✅ Yes | **Go** | Same monorepo. Standard library. |
| **GraphQL API** | ❌ **No, Phase 1+** | — | REST покрывает 95% use cases. GraphQL — premature optimization для Phase 0. |
| **gRPC for internal services** | ❌ **No, Phase 1+** | — | В Phase 0 — monolith. Microservices premature. Внутри Go monorepo — функциональные вызовы. |
| **L3 Detection Worker** (post-commit scanning) | ✅ Yes | **Go** | Async worker, periodic task scheduler. |

**Single backend language: Go.** Один language для всех бэкенд-сервисов, один monorepo, один CI pipeline, один observability setup, один hire profile.

### 2.2 Spark Extension (separate artifact, mandatory language)

| Component | Phase 0? | Language | Justification |
|---|---|---|---|
| **Spark SQL Extension** для write-path L2 | ⚠ **Phase 0.5** (M3-M4) | **Scala** | Mandatory — Spark API is JVM. Customers с серьёзным compliance требованием активируют это в Phase 0 пилотах. До M3 — L1 + L3 only. |
| **Spark Catalog wrapper** (для catalog mediation) | ✅ Yes | **Scala** | Wraps `SparkSessionCatalog` per spec §4. Required для proper Spark integration. |

**JVM language: Scala.** Используется только для Spark integration artifacts. Не для бэкенда, не для UI.

### 2.3 SDK & Client libraries

| Component | Phase 0? | Language | Justification |
|---|---|---|---|
| **Python SDK** (`neksur-spark` package) | ⚠ **Phase 0.5** (M3-M4) | **Python** | Data engineering team adoption. После Spark Extension готова. |
| **JVM SDK** (Java/Scala) | ❌ **No, Phase 1** | — | Defer until Python SDK proven. JVM SDK has Spark Extension as alternative. |
| **Go client library** | ❌ **No, Phase 2+** | — | Только если customer signal. |
| **Rust SDK** | ❌ **No, Phase 3+** | — | Premature. |

### 2.4 Frontend

| Component | Phase 0? | Tech | Justification |
|---|---|---|---|
| **Web UI** | ✅ Yes (minimal) | **TypeScript + React + Tailwind** | Required для design partner demos. Minimal scope: view catalog, view lineage graph, edit policies. |
| **Mobile app** | ❌ **No, ever** | — | We're infrastructure, not consumer. |

### 2.5 Storage layer

| Component | Phase 0? | Tech | Justification |
|---|---|---|---|
| **Primary OLTP + Graph** | ✅ Yes | **PostgreSQL 16 + Apache AGE** | Per ADR-001. Single Postgres = single ops stack. AGE = openCypher без отдельного сервиса. |
| **Full-text search** | ✅ Yes | **PostgreSQL `tsvector` (built-in)** | Postgres FTS достаточно для Phase 0 (catalogs до 100K tables). НЕ ставим OpenSearch/Tantivy. |
| **Vector search для semantic search** | ❌ **No, Phase 1** | — | Postgres `pgvector` extension добавляем в Phase 1 если customer signal. |
| **Cache layer** | ⚠ **Conditional** | **Redis** (only if benchmarks show need) | Default: nothing. Add Redis only если policy fetch latency hurts. Премature optimization is enemy. |
| **OpenSearch / Elasticsearch** | ❌ **No, Phase 3+** | — | Только если customer достигает 1M+ catalog entries. |
| **Tantivy** | ❌ **No, never для main app** | — | Не масштабируется горизонтально. Может быть useful для embedded use cases в Phase 2+. |
| **Object storage (S3/GCS/Azure)** | ✅ Yes | **AWS S3 (Phase 0), abstracted via interface** | Iceberg data lives there. We don't store data в S3 — мы оперируем metadata только. |

### 2.6 Policy & enforcement

| Component | Phase 0? | Tech | Justification |
|---|---|---|---|
| **Policy engine** | ✅ Yes | **Open Policy Agent (OPA) + Rego** | Industry standard. Used by Kubernetes, Envoy, Istio. Embedded as Go library. |
| **Policy DSL для row filters / column masks** | ✅ Yes | **Custom YAML schema** | Simple typed YAML compiled to Rego policies. |
| **ABAC engine** | ❌ **No, Phase 1 (Business tier)** | — | Per spec §11. RBAC только в Phase 0. |
| **Data residency enforcement** | ❌ **No, Phase 1** | — | Geographic policies — Phase 1 feature. |
| **Encryption key management** | ❌ **No, Phase 2+** | — | We integrate с customer's KMS/Vault когда необходимо. Не строим свой. |

### 2.7 Catalog integrations (per ADR-003)

| Catalog | Phase 0? | Priority |
|---|---|---|
| **Apache Polaris** | ✅ Yes | **P1 — primary** |
| **Generic Iceberg REST Catalog** | ✅ Yes | P1 (Polaris-compatible API) |
| **Databricks Unity Catalog REST** | ❌ **No, Phase 1** | P2 — second integration |
| **AWS Glue Iceberg REST** | ❌ **No, Phase 1** | P2 |
| **Snowflake Iceberg Catalog REST** | ❌ **No, Phase 1** | P2 |
| **Apache Nessie** | ❌ **No, Phase 2** | P3 |
| **Hive Metastore (Thrift)** | ❌ **No, Phase 2+** | P3 fallback |
| **Hadoop file-system catalog** | ❌ **Never** | Not supported — customer must migrate |

### 2.8 Engine adapters

| Engine | Phase 0? | Priority |
|---|---|---|
| **Trino** | ✅ Yes | **P1 — read-path первый** |
| **Apache Spark** | ✅ Yes (M3+) | **P1 — write-path для L2 Extension** |
| **Snowflake** (как reader из Iceberg REST) | ❌ **No, Phase 1** | P2 |
| **Apache Flink** | ❌ **No, Phase 1+** | P2 (streaming — separate complexity) |
| **Dremio** | ❌ **No, Phase 2** | P3 |
| **AWS Athena** | ❌ **No, Phase 2** | P3 |
| **PyIceberg native** | ❌ **No, Phase 2+** | P3 (когда customer ask) |

### 2.9 Data quality

| Component | Phase 0? | Tech | Justification |
|---|---|---|---|
| **Iceberg-native freshness checks** | ✅ Yes | Custom (через snapshot metadata) | Native, бесплатно — snapshot timestamps уже там. |
| **Schema drift detection** | ✅ Yes | Custom (через schema history) | Native через Iceberg schema evolution. |
| **Soda Core integration** | ❌ **No, Phase 1** | — | Adapter — Phase 1. Customer пока pipe Soda results manually. |
| **Great Expectations integration** | ❌ **No, Phase 2** | — | Choose one (Soda OR GE). Soda проще, идёт первым. |
| **ML anomaly detection** | ❌ **No, Phase 2 (Business tier)** | — | Commercial feature per ADR-002. |
| **Cross-engine reconciliation** | ❌ **No, Phase 2** | — | Advanced multi-engine feature. |

### 2.10 Observability & lineage

| Component | Phase 0? | Tech | Justification |
|---|---|---|---|
| **OpenLineage events consumer** | ✅ Yes | Custom Go consumer | Industry standard input format. |
| **OpenLineage producer** (мы отправляем events) | ⚠ **Phase 0.5** | Custom | Useful для customer downstream tooling. |
| **OpenTelemetry instrumentation** | ✅ Yes | OTel SDK для Go | Internal traces/metrics. Default observability stack. |
| **Logging** | ✅ Yes | Structured logs (zap/slog) | Standard Go. |
| **Metrics export** | ✅ Yes | Prometheus format | Standard для self-hosting. |
| **OpenMetadata sync** | ❌ **No, Phase 1** | — | Bidirectional sync — Phase 1 feature. |

### 2.11 Async & messaging

| Component | Phase 0? | Tech | Justification |
|---|---|---|---|
| **Async job queue** | ✅ Yes | **PostgreSQL-based queue** (e.g., `riverqueue` для Go) | Postgres достаточно для Phase 0. Не вводим Kafka/RabbitMQ. |
| **Apache Kafka** | ❌ **No, Phase 2+** | — | Только если customer scale требует. |
| **Webhooks для catalog events** | ✅ Yes | Standard HTTP | Polaris commit events. |
| **Internal pub/sub** | ❌ **No, Phase 1+** | — | В Phase 0 — direct function calls (monolith). |

### 2.12 Interfaces

| Interface | Phase 0? | Protocol |
|---|---|---|
| **REST API** | ✅ Yes | HTTP/JSON, OpenAPI 3.0 spec |
| **MCP server** | ✅ Yes | Stdio + SSE per MCP spec |
| **pgwire SQL proxy** | ✅ Yes | PostgreSQL wire protocol |
| **GraphQL** | ❌ **No, Phase 2** | — |
| **gRPC** | ❌ **No, Phase 2+ (если когда-нибудь microservices)** | — |
| **Iceberg REST Catalog proxy** | ✅ Yes | Iceberg REST OpenAPI spec |

### 2.13 Authentication & authorization

| Component | Phase 0? | Tech |
|---|---|---|
| **Token-based auth** | ✅ Yes | JWT с standard claims |
| **OIDC integration** | ⚠ Phase 0.5 | Google/Microsoft OIDC для design partners |
| **SAML** | ❌ Phase 1 (Business tier) | — |
| **SCIM provisioning** | ❌ Phase 2 (Enterprise tier) | — |
| **Service accounts** | ✅ Yes | Long-lived tokens для Spark service principals |

### 2.14 Deployment

| Aspect | Phase 0 | Phase 1+ |
|---|---|---|
| **Primary target** | Self-hosted via Docker Compose | Helm chart, managed SaaS |
| **Container** | Docker | + Kubernetes manifests Phase 1 |
| **Orchestration** | Docker Compose | Kubernetes Phase 1 |
| **Cloud** | AWS-first (S3, EKS later) | + GCP, Azure Phase 2 |
| **High availability** | Single instance + Postgres replica | Multi-instance Phase 1 |
| **Air-gapped** | ❌ Not Phase 0 (Enterprise feature) | Phase 2 |

---

## 3. Summary — Phase 0 stack in one screen

```
┌─────────────────────────────────────────────────────────────┐
│ NEKSUR PHASE 0 IMPLEMENTATION STACK (M1-M4)                 │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│ Backend services:    Go (monorepo, single language)         │
│ Spark integration:   Scala (separate artifact, M3+)         │
│ Python SDK:          Python (separate artifact, M3+)        │
│ Frontend:            TypeScript + React + Tailwind          │
│                                                              │
│ Storage:             PostgreSQL 16 + Apache AGE             │
│                      (graph + relational + FTS, ONE service)│
│                                                              │
│ Policy:              OPA + Rego (embedded as Go library)    │
│                                                              │
│ Standards:           Iceberg REST, OpenLineage, MCP, OPA,   │
│                      pgwire, OpenTelemetry, OpenAPI         │
│                                                              │
│ Catalogs:            Polaris (P1 only)                      │
│ Engines:             Trino (read), Spark (write via L2)     │
│                                                              │
│ Deployment:          Docker Compose, AWS, self-host         │
│                                                              │
│ Languages count:     4 (Go, Scala, Python, TypeScript)      │
│ Persistent stores:   1 (PostgreSQL)                         │
│ External services:   1 (Iceberg-compatible catalog —        │
│                         Polaris in customer's environment)  │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

Сравнение с full spec stack: **25+ компонентов → 9 ключевых компонентов**. Это **Phase 0 reduction**, не permanent. Phase 1, 2, 3 добавят остальное **только при customer signal**.

---

## 4. Explicit anti-stack — что мы НЕ используем в Phase 0

Этот список — **defensive**. Если соблазн добавить что-то отсюда — flag в этом документе с justification и founder approval.

### Не используем (technology choices)

- ❌ **Rust** — premature для бэкенда. Go достаточно. Rust adds hiring friction.
- ❌ **GraphQL** — REST покрывает Phase 0 needs.
- ❌ **gRPC** — monorepo doesn't need RPC. Phase 2+ если microservices.
- ❌ **Kafka / RabbitMQ / SQS** — Postgres queue достаточно для Phase 0 throughput.
- ❌ **OpenSearch / Elasticsearch / Tantivy** — Postgres FTS достаточно.
- ❌ **Redis** (unless benchmarks show need) — default cache в-памяти Go-приложения.
- ❌ **Memgraph / Neo4j / NebulaGraph** — Apache AGE per ADR-001 для Phase 0.
- ❌ **Kubernetes** в Phase 0 — Docker Compose проще для self-host design partner deployments.
- ❌ **Microservices** — monolith с clean modules. Service split — Phase 2+ если scale требует.
- ❌ **Custom build tools** — стандартный Go modules, npm для frontend, sbt для Scala.
- ❌ **GraphQL Federation, Service Mesh, Sidecar patterns** — Phase 3+ если когда-нибудь.

### Не используем (catalogs / engines)

- ❌ Databricks Unity Catalog — Phase 1
- ❌ AWS Glue Iceberg REST — Phase 1
- ❌ Snowflake Iceberg Catalog — Phase 1
- ❌ Apache Nessie — Phase 2
- ❌ Hive Metastore — Phase 2+
- ❌ Apache Flink — Phase 1+
- ❌ Dremio adapter — Phase 2
- ❌ AWS Athena adapter — Phase 2
- ❌ PyIceberg native client — Phase 2+

### Не строим (features)

- ❌ ABAC engine (RBAC only в Phase 0)
- ❌ ML anomaly detection
- ❌ Encryption key management
- ❌ Air-gapped distribution
- ❌ SAML/SCIM
- ❌ Multi-region replication
- ❌ Compliance bundles (SOC 2/HIPAA artifacts)
- ❌ FinOps modules
- ❌ Cross-engine reconciliation
- ❌ Materialized view router
- ❌ Federated queries (multi-catalog views)
- ❌ Data marketplace

---

## 5. Phase 0 milestone breakdown (M1-M4)

### M1 — Foundation (Week 1-4)

**Goal:** Single Go monorepo с running services, PostgreSQL + AGE schema, basic UI shell.

**Deliverables:**
- Go monorepo structure (см. §6)
- PostgreSQL 16 + Apache AGE setup
- Graph schema per ADR-001 §3 (17 node types, 15 edge types)
- Polaris catalog adapter (read-only)
- Basic REST API skeleton
- Basic React UI: project setup, нет content
- Docker Compose для local dev
- CI: GitHub Actions с Go test + lint + Postgres integration tests

**Out of scope для M1:**
- Policy enforcement (M2)
- MCP server (M2)
- SQL proxy (M3)
- Spark Extension (M3)

### M2 — Read-path enforcement (Week 5-8)

**Goal:** Policy definition (YAML) + OPA evaluation + REST endpoint для policy CRUD.

**Deliverables:**
- Policy YAML schema (defining row filters, column masks)
- OPA Rego policy compiler (YAML → Rego)
- Policy CRUD REST API
- MCP server with basic tools (`discover.search`, `model.list_metrics`, `graph.traverse`)
- L1 catalog gateway skeleton (Iceberg REST proxy, без enforcement yet)
- UI: policy editor (basic form-based editor для row filters)

### M3 — Trino read-path + L1 enforcement (Week 9-12)

**Goal:** End-to-end read flow с Trino: query → SQL proxy → policy injection → Trino → results.

**Deliverables:**
- pgwire SQL proxy
- SQL parser + rewriter для row filter / column mask injection
- Trino adapter — connection management, query forwarding
- L1 Catalog Gateway — full validation pipeline (per ADR-003 §3)
- L3 Detection Worker — basic version (snapshot polling, regex PII classifier)
- Audit log subsystem
- UI: lineage graph viewer, audit log explorer

### M4 — Spark write-path + design partner demo (Week 13-16)

**Goal:** Spark write через Neksur, L2 Spark Extension (Approach B) functional, end-to-end demo для design partner.

**Deliverables:**
- Scala Spark SQL Extension (per ADR-003 §4.3 Approach B)
- Catalyst rule injection для write commands
- Python SDK (basic, Approach A)
- Integration tests: Spark write → L2 transformation → L1 catalog validation → success
- Bypass test: Spark write без extension → L1 reject OR L3 detection
- Documentation: getting started, configuration, demo script
- **First design partner deployment**: соответствие критериям spec §6.9 (≥2 engines, compliance pain)

---

## 6. Repo structure (recommended)

Per ADR-002 §6.1 + this stack:

```
github.com/neksur-io/neksur            (BSL Core, public eventually)
├── LICENSE                            (BSL 1.1 с Additional Use Grant)
├── DCO.md
├── CONTRIBUTING.md
├── README.md
├── CLAUDE.md                          (project-specific guidance для AI work)
├── go.mod
├── go.sum
├── docker-compose.yml                 (local dev environment)
├── Makefile                           (build, test, run shortcuts)
│
├── cmd/                               (entry points)
│   ├── neksur-server/                 (main backend binary)
│   │   └── main.go
│   ├── neksur-worker/                 (L3 detection worker)
│   │   └── main.go
│   └── neksur-cli/                    (admin CLI)
│       └── main.go
│
├── internal/                          (Go internal packages — not importable from outside)
│   ├── gateway/                       (L1 Catalog Gateway)
│   │   ├── iceberg_proxy.go
│   │   ├── validation.go
│   │   └── policy_inject.go
│   ├── policy/                        (Policy service + OPA integration)
│   │   ├── compiler.go                (YAML → Rego)
│   │   ├── evaluator.go               (OPA wrapper)
│   │   └── store.go                   (CRUD)
│   ├── sqlproxy/                      (pgwire SQL proxy + rewriter)
│   │   ├── server.go
│   │   ├── parser.go
│   │   └── rewriter.go
│   ├── semantic/                      (Semantic engine — metric compiler)
│   │   ├── ast.go
│   │   ├── compiler.go
│   │   └── dialects/                  (per-engine SQL generation)
│   │       ├── trino.go
│   │       └── spark.go
│   ├── graph/                         (AGE graph operations)
│   │   ├── schema.go                  (DDL для node/edge types)
│   │   ├── queries.go                 (Cypher templates)
│   │   └── ingestion.go               (lineage event → edges)
│   ├── catalog/                       (Catalog adapters)
│   │   ├── adapter.go                 (interface)
│   │   ├── polaris/                   (Polaris adapter)
│   │   └── generic_rest/              (Iceberg REST adapter)
│   ├── engines/                       (Engine adapters)
│   │   ├── adapter.go                 (interface)
│   │   └── trino/                     (Trino adapter)
│   ├── lineage/                       (OpenLineage consumer)
│   │   ├── consumer.go
│   │   └── parser.go
│   ├── detection/                     (L3 detection)
│   │   ├── worker.go
│   │   ├── classifier.go              (regex + sampling)
│   │   └── notifier.go                (alerts)
│   ├── api/                           (REST + MCP servers)
│   │   ├── rest/
│   │   │   ├── server.go
│   │   │   ├── policies.go
│   │   │   ├── catalogs.go
│   │   │   └── ...
│   │   └── mcp/
│   │       ├── server.go
│   │       ├── tools.go
│   │       └── ...
│   ├── audit/                         (Audit log subsystem)
│   │   └── log.go
│   └── observability/                 (OTel, metrics, logging setup)
│       ├── tracing.go
│       └── metrics.go
│
├── pkg/                               (Go public packages — importable)
│   ├── types/                         (shared types)
│   │   ├── policy.go
│   │   ├── catalog.go
│   │   └── ...
│   └── client/                        (Go client library — Phase 1)
│
├── web/                               (Frontend)
│   ├── package.json
│   ├── tsconfig.json
│   ├── vite.config.ts
│   ├── src/
│   │   ├── App.tsx
│   │   ├── pages/
│   │   ├── components/
│   │   └── api/                       (REST + MCP client)
│   └── public/
│
├── scripts/
│   ├── setup-postgres.sh              (Apache AGE + schema setup)
│   ├── seed-data.sh
│   └── run-integration-tests.sh
│
├── deploy/
│   ├── docker/
│   │   └── Dockerfile
│   └── docker-compose/
│       └── docker-compose.dev.yml
│
├── docs/
│   ├── getting-started.md
│   ├── architecture.md
│   ├── policy-yaml-schema.md
│   └── adrs/                          (ADRs from discovery)
│       ├── 001-graph-foundation.md
│       ├── 002-licensing.md
│       └── 003-write-path-enforcement.md
│
├── tests/
│   ├── integration/
│   │   ├── policy_enforcement_test.go
│   │   ├── catalog_gateway_test.go
│   │   └── spark_e2e_test.go
│   └── fixtures/
│
└── .github/
    └── workflows/
        ├── ci.yml                     (test + lint)
        ├── docker.yml                 (image build)
        └── release.yml                (Phase 1+)


github.com/neksur-io/neksur-spark      (BSL Core, public)
├── LICENSE                            (BSL 1.1)
├── build.sbt
├── src/main/scala/com/neksur/spark/
│   ├── NeksurExtension.scala          (Spark SQL Extension entry point)
│   ├── CatalystRule.scala             (write-path rule)
│   ├── PolicyFetcher.scala            (calls Neksur API)
│   └── Transformations.scala          (column masking UDFs)
└── tests/


github.com/neksur-io/neksur-python     (BSL Core, public)
├── LICENSE                            (BSL 1.1)
├── pyproject.toml
├── neksur/
│   ├── __init__.py
│   ├── spark.py                       (NeksurDataFrameWriter)
│   ├── client.py                      (REST client to Neksur API)
│   └── ...
└── tests/


github.com/neksur-io/neksur-premium    (Commercial License, PRIVATE)
├── LICENSE-Premium                    (Neksur Commercial License)
├── go.mod
├── premium/
│   ├── abac/                          (Phase 1 — not Phase 0)
│   ├── multi_engine_l2_l3/            (Phase 1 — not Phase 0)
│   ├── compliance_bundles/            (Phase 1+)
│   └── ml_anomaly_detection/          (Phase 2)
└── ...


github.com/neksur-io/neksur-docs       (Apache 2.0, public)
├── content/
│   ├── getting-started/
│   ├── concepts/
│   ├── reference/
│   └── licensing/
└── ...
```

**Key principles:**
- **Monorepo для backend** (`neksur-io/neksur`) — все Go services в одном repo, одна CI, один release cycle
- **Separate repos для cross-language artifacts** (Spark, Python) — different toolchains, different release cycles
- **Premium repo private** — commercial code isolated per ADR-002
- **Docs repo separate** — Apache 2.0, can be public early, contributions welcomed

---

## 7. CLAUDE.md template для main repo

Положите этот файл в корень `neksur-io/neksur` как guidance для AI assistance:

```markdown
# CLAUDE.md — guidance для AI-assisted development в Neksur

## Project context

Neksur — Open Lakehouse Governance Plane. Cross-engine policy enforcement
для Apache Iceberg lakehouses. Per `docs/adrs/`.

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
- **Spark code:** Scala (in separate repo `neksur-spark`).
- **Python SDK:** Python (in separate repo `neksur-python`).
- **Frontend:** TypeScript + React + Tailwind.

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
- **Testing:** table-driven tests, integration tests с real Postgres + AGE,
  no mocks для critical paths.
- **License headers:** BSL 1.1 в каждом .go file, см. `scripts/license-header.txt`.

## Critical rules

1. **Never commit secrets.** Use `.env.example`, real .env в `.gitignore`.
2. **BSL license** в каждом file header. Premium code goes к `neksur-premium`
   (separate repo, private).
3. **Test before commit** — `make test` passes.
4. **One PR, one logical change** — readable git history matters.
5. **Spec changes need ADR amendment** — не silently diverge от spec/ADR.

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

- Re-read relevant ADR (`docs/adrs/`)
- Check spec section
- Default: simpler is better для bootstrap stage
```

Этот файл — **single source of truth** для AI assistance work. Обновляйте по мере evolution стека.

---

## 8. Что меняется при переходе к Phase 1+

Этот document — **only для Phase 0**. По мере прогресса к Phase 1, мы revise:

**Phase 1 trigger:** ≥2 design partners в production, basic value prop validated.

**Phase 1 additions (per spec §12):**
- Databricks Unity Catalog adapter
- AWS Glue Iceberg REST adapter
- Snowflake Iceberg adapter
- Apache Flink integration (streaming write path)
- ABAC engine
- Soda Core data quality adapter
- pgvector для semantic search
- OpenMetadata bidirectional sync

**Phase 1 stack additions (carefully):**
- Add: Helm chart для Kubernetes deployment
- Add: Redis IF benchmarks show need
- Add: pgvector extension
- Maybe: GraphQL API (если customer pull)

**Phase 2 (M11-M16) — multi-engine coordinator deep work + commercial features**

**Phase 3+ — post-PMF expansion**

Каждая фаза получит свой "Phase N Implementation Stack" документ с тем же подходом: explicit scope, explicit anti-stack, repo structure updates, milestone breakdown.

---

## 9. Open questions для resolution в первые дни Claude Code

Эти вопросы намеренно оставлены open — нужны practical answers через actual implementation:

1. **AGE performance под realistic load.** Synthetic benchmarks в ADR-001 нужно validate с real data. Сделать в M1 — load test 10M edges, measure P95 на 3-hop traversal.

2. **OPA vs Cedar.** OPA выбран как industry standard, но Amazon Cedar (released 2023) — newer, possibly cleaner для policy authoring. Default — OPA, но evaluate Cedar в M2 (1 day spike).

3. **Frontend framework — Vite vs Next.js.** Phase 0 — Vite (SPA, simpler). Phase 1 если нужно SSR — switch к Next.js. Решить в M1.

4. **Go web framework.** Standard `net/http` достаточно для Phase 0, или нужен chi/echo/gin? Default — standard library + chi router (minimal, well-maintained). Decide в M1.

5. **AGE distribution choice.** Apache AGE binaries vs Docker container vs build from source. Decide M1 setup.

6. **Container registry.** GitHub Container Registry default. Customer-facing distribution — Phase 1.

7. **Test data fixtures.** Generate synthetic Iceberg tables в test environment? Or use real-world examples (TPC-DS adapted)? Decide M1.

8. **CI duration budget.** Target <10 minutes для full test suite. Если превышает — что cut?

---

## 10. Honest meta-note

Этот документ существует, потому что spec v0.7 описывает **vision на 16 месяцев**, а Claude Code работа делается **здесь и сейчас**. Без этого constraint document риск **scope creep гарантирован**.

В implementation:
- **При каждом PR review** — спросить: «это в Phase 0 scope?»
- **При каждом hire** — спросить: «он/она для Phase 0 или Phase 1+?»
- **При каждом customer conversation** — спросить: «они Phase 0 design partner или Phase 1+ prospect?»
- **При каждой архитектурной дискуссии** — спросить: «we're solving Phase 0 problem или premature optimization?»

**4 месяца на 4 milestones, jasn focused work, end-to-end demo для design partner к концу M4.** Это и есть defensible Phase 0.

После M4 — review, decide whether to commit Phase 1, или extend Phase 0 based on learnings.

---

*Документ updated по мере evolution. Если что-то disagree с reality — ADR amendment, не silent drift.*
