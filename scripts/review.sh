#!/usr/bin/env bash
# Independent review of a change by another model family.
#   scripts/review.sh [reviewer] [git-diff-args...]
#   scripts/review.sh                 # sol reviews uncommitted changes
#   scripts/review.sh astra HEAD~1    # astra reviews the last commit
# The reviewer gets the diff and the project rules, not the author's reasoning.
# Codex can also read files under the repo; the diff text is untrusted input
# to it. agy can read the repo too; its limits were seen in single runs (ask.sh).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
who=${1:-sol}; shift || true
diff=$(git diff "${@:-HEAD}")
[ -n "$diff" ] || { echo "nothing to review" >&2; exit 0; }
{
  cat <<'P'
You are reviewing a change to a2p2, a small local-first peer-to-peer messaging
daemon for AI agents. Read AGENTS.md and docs/DESIGN.md first for the rules.
Review ONLY the diff below. Report, most severe first:
1. Bugs: wrong behaviour, races, lost or duplicated messages, unhandled errors.
2. Security: anything that lets an unpaired node, a non-member, or a web page
   read or write; anything that trusts peer-supplied identity; missing limits.
3. Violations of the design or decision log, and needless complexity that could
   be deleted.
4. Missing tests for behaviour the diff adds.
For each finding give file:line, a concrete failing scenario, and the smallest
fix. Do not praise, do not restate the diff, do not suggest style changes. If
you find nothing in a category, say "none". Do not modify files.

DIFF:
P
  printf '%s\n' "$diff"
} | "$root/scripts/ask.sh" "$who" -
