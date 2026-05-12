# Security Policy

## Reporting a Vulnerability

If you believe you have found a security vulnerability in Neksur Core, please **do not** open a public issue or pull request. Instead, report it privately so we can investigate and disclose responsibly.

**Email:** `security@neksur.com`

If you cannot reach the address above, you may also use [GitHub private vulnerability reporting](https://github.com/neksur-com/neksur/security/advisories/new) on this repository.

When reporting, please include:

- A clear description of the vulnerability
- Steps to reproduce (proof-of-concept code, configurations, screenshots)
- Affected version(s) and component(s)
- Impact assessment (data exposure, privilege escalation, denial of service, etc.)
- Suggested mitigation if you have one

## What to Expect

- **Acknowledgement:** within 3 business days of receipt
- **Initial triage:** within 7 business days with severity assessment and disclosure timeline
- **Fix target:** depends on severity — critical issues prioritized; lower-severity issues batched with regular releases
- **Coordinated disclosure:** we will work with you on a coordinated disclosure date once a fix is available

## Scope

This policy covers the `neksur` (Neksur Core) repository — BSL Core source-available components.

**Out of scope** of this repository's policy (separate channels):

- `neksur-com/neksur-premium` (Commercial Premium components) — same email address, but reported separately under your commercial support agreement
- Vulnerabilities in upstream dependencies (Apache AGE, Postgres, Patroni, pgBackRest, etc.) — please report directly to those projects
- Operational misconfiguration on customer infrastructure (e.g., hard-coded AWS keys, unrestricted S3 bucket policies) — these are operational concerns, see the Neksur deployment runbooks

## Safe Harbor

We support good-faith security research. If you follow this policy and act in good faith, we will not pursue legal action for accidental violations of acceptable use that occur during research.

## Pre-MVP Notice

Neksur Core is currently in pre-MVP / discovery phase. The codebase is incomplete and many components are placeholders. Security reports during this phase are especially valuable — please err on the side of reporting.

---

*Last updated: 2026-05-12*
