#!/usr/bin/env bash
# scripts/check-adr-011.sh
#
# ADR-011 retired-language gate. Returns 0 if no retired language is found
# anywhere in the docs surface; returns 1 (+ diagnostic listing) otherwise.
#
# Scope (per Phase 03.1 OQ#4):
#   - Markdown (*.md) and YAML (*.yml, *.yaml) files only.
#   - Engineering source (Go, runbooks, license-gen tools) is NOT scanned;
#     internal-only L-codes in those surfaces are legitimate per ADR-011 §3.7.
#
# Exclusions (per CONTEXT.md §CI grep-gate and Anti-Patterns §1, §2, §3, §6, §7, §9):
#   - .git, archive/, base/ (symlink), .planning/ (symlink), node_modules/, vendor/.
#   - adr-003 files (legitimate internal-code home per ADR-011 §3.7),
#     product-concept-adr.md (enumerates the retired terms by design in its
#     mapping table), INGEST-CONFLICTS.md (historical conflict log), this
#     script + its workflow + its pattern file (self-trigger guard).
#
# Per-line escape hatch: append a trailing comment marker
#   # RETIRED-LANGUAGE-OK
# to any single line that legitimately quotes retired vocabulary (changelog
# provenance, archive headers, mapping-table cites, etc.). Use sparingly.
#
# Local invocation (from the repo root):
#   bash scripts/check-adr-011.sh
#
# CI invocation: see .github/workflows/adr-011-gate.yml.

set -euo pipefail

# Pattern stored in a separate file so the script body itself doesn't
# contain literal retired-language tokens (otherwise the gate would
# self-trigger on its own source — see RESEARCH §Pitfall 1).
PATTERN_FILE="$(dirname "$0")/.check-adr-011.pattern"

if [[ ! -f "$PATTERN_FILE" ]]; then
  echo "FATAL: pattern file not found: $PATTERN_FILE" >&2
  exit 2
fi

# Path exclusions (directories).
#
# Most of these are engineering-internal surface where internal-code
# vocabulary is legitimate per ADR-011 §3.7 and §5.1 item 18 (OQ#4
# locked in Phase 03.1 CONTEXT.md §Locked Open Questions). The phase
# scopes the gate to customer-facing markdown + CI YAML; runbooks,
# observability alerts, internal decision logs, and build-matrix
# workflow YAML are engineering-source and out of scope.
EXCLUDE_DIRS=(
  ".git"
  ".claude"        # Claude Code worktree mirror; not canonical source
  "archive"
  "base"           # symlink target of /Users/evgeny/neksur/base
  ".planning"      # symlink target of /Users/evgeny/neksur/.planning
  "node_modules"
  "vendor"
  "runbooks"       # operational runbooks; internal codes legit per OQ#4
  "docs"           # engineering decision logs + phase-0 stack notes
  "observability"  # Prometheus alert rules; internal engineering plumbing
  "ops"            # Prometheus alert YAMLs; same
)

# Path exclusions (filename substrings).
#
# `nightly-cross-engine.yml` and `build-matrix.yml` legitimately reference
# binary tier names (L1 / L2 / L3) in their job matrices and tier-isolation
# tests; these are engineering plumbing, not customer-facing copy. They
# remain auditable in code review but are excluded from the doc gate per
# OQ#4 (locked in Phase 03.1 CONTEXT.md §Locked Open Questions).
EXCLUDE_FILES=(
  "adr-003"                      # ADR-003 is the legitimate internal-code home
  "product-concept-adr.md"       # ADR-011 itself enumerates retired terms
  "INGEST-CONFLICTS.md"          # historical conflict-resolution log
  "check-adr-011.sh"             # this script
  "adr-011-gate.yml"             # this workflow
  ".check-adr-011.pattern"       # pattern file
  "nightly-cross-engine.yml"     # engineering CI: binary tier matrix
  "build-matrix.yml"             # engineering CI: tier-isolation tests
  "neksur-spec-v0.7.md"             # SUPERSEDED predecessor (Phase 03.1 OQ#1): retained in base/ for cross-reference, body unchanged
  "neksur-business-model-v0.5.md"   # SUPERSEDED predecessor (Phase 03.1 OQ#1): retained in base/ for cross-reference, body unchanged
  "neksur-market-analysis-v0.4.md"  # SUPERSEDED predecessor (Phase 03.1 OQ#1): retained in base/ for cross-reference, body unchanged
)

# Build grep exclusion args.
exclude_args=()
for d in "${EXCLUDE_DIRS[@]}"; do
  exclude_args+=("--exclude-dir=$d")
done
for f in "${EXCLUDE_FILES[@]}"; do
  exclude_args+=("--exclude=*$f*")
done

# Scope-limit args: docs + CI YAML only.
include_args=(
  "--include=*.md"
  "--include=*.yml"
  "--include=*.yaml"
)

# Optional target list: defaults to "." (current working directory).
# Callers may pass explicit paths to narrow the scan; useful for the
# planning-workspace manual run on a single file.
if [[ $# -gt 0 ]]; then
  targets=("$@")
else
  targets=(".")
fi

# Run the grep. `--binary-files=without-match` is defensive in case the
# include filters miss something (Pitfall 9). `|| true` keeps `set -e`
# from killing us on the no-match case (grep exits 1 then).
hits=$(grep -rEn --color=never --binary-files=without-match \
       "${include_args[@]}" "${exclude_args[@]}" \
       -f "$PATTERN_FILE" "${targets[@]}" 2>/dev/null \
       | grep -v "RETIRED-LANGUAGE-OK" || true)

if [[ -n "$hits" ]]; then
  printf 'X ADR-011 grep-gate: retired language found\n\n'
  printf '%s\n' "$hits"
  printf '\n'
  printf 'If a match is a legitimate quote of retired vocabulary\n'
  printf '(changelog provenance, archive header, mapping-table cite),\n'
  printf 'append the trailing comment marker to that single line:\n'
  printf '    # RETIRED-LANGUAGE-OK\n'
  exit 1
fi

printf 'OK ADR-011 grep-gate: zero retired-language matches\n'
exit 0
