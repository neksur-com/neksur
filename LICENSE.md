# License — Human-Readable Summary

> **TL;DR.** Neksur Core is source-available under the **Business Source License 1.1** with a four-year **Change Date** to the **Apache License 2.0**. The full license text is in [`LICENSE`](LICENSE). This document is a plain-English summary; if anything here conflicts with the LICENSE file, the LICENSE file wins.

---

## What License Is This?

Neksur Core is licensed under the **Business Source License 1.1** (BSL 1.1), with the following parameters:

| Parameter | Value |
|---|---|
| **Licensor** | Neksur (legal entity to be formalized prior to first paying customer) |
| **Licensed Work** | Neksur Core — the source-available core of the Neksur Open Lakehouse Control Plane for Apache Iceberg |
| **Change Date** | 2030-05-10 (four years from ratification of ADR-002) |
| **Change License** | Apache License, Version 2.0 |
| **Additional Use Grant** | See below |

The BSL 1.1 template is maintained by MariaDB plc and published at https://mariadb.com/bsl11/.

## What Can I Do With It?

**Allowed without restriction:**

- Read, study, and modify the source code.
- Run Neksur Core in your own infrastructure (cloud, on-premises, air-gapped, laptop) — for development, testing, evaluation, internal production, research, education, or anything else.
- Redistribute Neksur Core, modified or unmodified, **provided** the redistribution is not a competing managed service (see the Additional Use Grant below).
- Embed Neksur Core in your own product, **provided** your product is not a competing managed service of the type described in the Additional Use Grant.
- Contribute back via Pull Request under the project's DCO process (see `CONTRIBUTING.md`).
- After **2030-05-10**, do anything Apache License 2.0 allows — including offering Neksur Core as a managed service.

**Not allowed under BSL (until the Change Date):**

- Offer Neksur Core, in whole or in significant part, to third parties as a **managed, hosted, or embedded service** whose primary value proposition is **cross-engine policy enforcement, semantic consistency, or runtime coordination over Apache Iceberg lakehouses or open lakehouse systems**.
- Sublicense Neksur Core under terms that conflict with BSL 1.1.
- Remove the license headers, attribution, or this LICENSE.md file from redistributed copies.

The Additional Use Grant is intentionally narrow: it blocks competing managed services and embedded competing services, but does **not** block any other use. If you are unsure whether your use case is allowed, please reach out at `hello@neksur.com` — we will respond in writing within a reasonable time.

## What Happens on 2030-05-10?

On the Change Date, every commit of Neksur Core released prior to that date **automatically and irrevocably** becomes available under the **Apache License, Version 2.0** — a permissive, OSI-approved open source license.

- The Change Date can be **accelerated** by the Licensor at any time. Once accelerated, it cannot be reversed.
- The Change Date will **not** be delayed for any reason. Any release made under BSL 1.1 is locked to that date.
- Releases made after the Change Date may use a newer Change Date for their own BSL releases, but the existing released code is bound to its original Change Date.

This is a one-way ratchet: the project becomes more open over time, never less.

## Why Not Just Use Apache 2.0 From the Start?

The BSL approach lets us sustain commercial development of premium features (RLS enforcement, multi-engine L2/L3, compliance bundles, ML anomaly detection, advanced write-path enforcement) while keeping the core open and free for internal use. Without the Additional Use Grant, a hyperscaler could package Neksur Core as a managed service and undercut Neksur's commercial offering — eliminating the funding source for ongoing core development.

The four-year Change Date is a credible public commitment: we believe the core will be valuable to the open lakehouse ecosystem long after we stop being its primary commercial steward. Once Neksur has reached commercial maturity (or fails to), the source becomes Apache 2.0 — community owned.

Precedents for this license model: HashiCorp (Terraform, Vault, Consul), Sentry, MariaDB, dbt Fusion, MetricFlow.

## Public Messaging

Per ADR-002 D-002.12, public messaging refers to Neksur Core as **"source-available"**, not "open source." This honors the OSI definition of open source (which excludes BSL) and avoids confusing the community. The core will become open source on the Change Date.

## Commercial Premium

Premium features are in a separate private repository (`neksur-com/neksur-premium`) under a **Neksur Commercial License**. The Commercial License is **closed source** and is not affected by the Change Date. The BSL/Commercial boundary is described in ADR-002 D-002.05 and D-002.06 and refined in ADR-003 D-003.04 for write-path enforcement levels.

For premium feature licensing, contact `hello@neksur.com`.

## Contributing

Contributions are accepted under the project's **Developer Certificate of Origin (DCO) 1.1** process — no separate Contributor License Agreement. See `CONTRIBUTING.md`. By signing off on a commit, you certify that you have the right to submit your contribution under BSL 1.1 (which will become Apache 2.0 on the Change Date along with the rest of the codebase).

## Status of Additional Use Grant Text

> **NOTE.** The Additional Use Grant text in the `LICENSE` file is a **draft** pending review by qualified counsel before the first public release. The current draft expresses the intent described above. If counsel review produces a refined text, the LICENSE file will be updated and a 90-day public notice issued per ADR-002 D-002.02. The intent of the grant — to block competing managed services while permitting all other use — will not change.

## Disclaimer

This LICENSE.md is a summary written in plain language for ease of understanding. It is **not** itself a legal document. The actual license terms governing your use of Neksur Core are exclusively those in the `LICENSE` file. If anything in this LICENSE.md is unclear or appears to conflict with the LICENSE file, please raise it on the issue tracker or via `hello@neksur.com`.

---

*Last updated: 2026-05-12*
