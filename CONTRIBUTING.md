# Contributing to Neksur Core

Thank you for your interest in contributing to Neksur Core. This document describes how to propose changes, what to expect from review, and the legal terms under which contributions are accepted.

## Status

Neksur Core is in **pre-MVP / Discovery phase** as of 2026-05-12. The codebase is incomplete; many components are placeholders that will be filled in during Phase 0 and Phase 1 implementation. External contributions are welcome but expect significant churn while foundational architecture lands.

## How to Contribute

### 1. Open an issue first

For anything non-trivial (bug reports notwithstanding), open an issue before starting work. This avoids duplicated effort and lets a maintainer flag whether the change aligns with the architecture decisions captured in our ADRs.

For trivial fixes (typos, broken links, obvious doc errors), feel free to skip directly to a pull request.

### 2. Fork and branch

```
git clone https://github.com/<your-user>/neksur.git
cd neksur
git checkout -b your-descriptive-branch-name
```

### 3. Make your changes

- Follow the existing code style (linting will run in CI once Phase 0 lands the test infrastructure).
- Add or update tests for any behavior change.
- Update documentation in the `docs` repo (`neksur-com/docs`) if applicable.
- Keep commits focused and write clear commit messages.

### 4. Sign your commits with the DCO

Every commit must be signed off under the **Developer Certificate of Origin** (DCO 1.1). The full DCO text is in the `DCO` file in this repository.

Use `git commit -s` to add a `Signed-off-by` trailer automatically:

```
git commit -s -m "fix: correct snapshot pin handling for cross-engine RLS"
```

This adds a trailer like:

```
Signed-off-by: Your Name <your.email@example.com>
```

By signing off, you certify that you have the right to submit the contribution under the project's license. We do **not** require a Contributor License Agreement (CLA) — DCO is the lighter alternative used by Linux, Kubernetes, and many other projects.

If you forget to sign off, the CI will fail. Amend with `git commit --amend -s` (for the most recent commit) or `git rebase --signoff main` (for a range).

### 5. Open a pull request

- Target the `main` branch unless a maintainer asks otherwise.
- Reference the related issue (`Closes #123` or `Refs #123`).
- Fill in the PR template completely.
- Be prepared to iterate — review feedback is the rule, not the exception.

## Review Process

- Maintainers will triage new PRs within 5 business days.
- Substantive PRs typically go through 1-3 review rounds.
- A PR needs approval from at least one maintainer before merge.
- Merges are typically squash-merge to keep history clean; the squash commit retains the DCO sign-off.

## What We Accept

- **Bug fixes** with a reproducible test case.
- **Documentation improvements** — typos, clarifications, examples.
- **Performance improvements** with benchmarks.
- **New features** that align with the architecture in our ADRs (see `base/` in the planning repo if you have access, or `docs.neksur.com` once published).

## What We Will Likely Reject

- **Features that belong in `neksur-premium`** (the Commercial Premium repository, private). The BSL Core / Commercial boundary is documented in ADR-002 and refined by ADR-003 for write-path enforcement. If you're not sure, ask in an issue first.
- **Vendor-specific integrations** that bypass the catalog adapter model. Neksur is catalog-agnostic; vendor-specific code lives behind the adapter interface.
- **Changes that violate a locked decision** (ADR-001, ADR-002, ADR-003, or any ratified ADR). If the locked decision is wrong, open a separate discussion issue to propose an ADR amendment first.
- **Code without tests** for non-trivial behavior changes.
- **Code without DCO sign-off** (no merge until signed).

## License

By contributing, you agree that your contributions are licensed under the same license as the project: the **Business Source License 1.1** with the Additional Use Grant and Change Date specified in the `LICENSE` file. After the Change Date, your contributions automatically become available under the **Apache License, Version 2.0** along with the rest of the project.

Your DCO sign-off is your legal certification that you have the right to submit your contribution under these terms.

## Reporting Security Issues

**Do not** open public issues for security vulnerabilities. See `SECURITY.md` for the private disclosure process.

## Community Standards

Participation in this project is governed by the `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1). Be respectful; we will be too.

## Questions

- General questions: open a Discussion on GitHub once Discussions are enabled.
- Architecture / roadmap questions: open an Issue with the `question` label.
- Commercial inquiries: `hello@neksur.com`

---

*Last updated: 2026-05-12*
