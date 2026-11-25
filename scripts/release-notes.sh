#!/usr/bin/env bash
# Print the release notes for a version tag: the summary (the first
# paragraph of that version's CHANGELOG.md section, when it has one before
# its first heading), the fixed install block
# (scripts/release-notes-head.md), then the rest of the section
# (scripts/changelog.sh).
#   scripts/release-notes.sh v0.2.2
# The release workflow publishes this output, and scripts/verify-release.sh
# checks that the published notes contain the install block.
set -euo pipefail
tag=${1:?usage: release-notes.sh <tag>}
root=$(cd "$(dirname "$0")/.." && pwd)
body=$("$root/scripts/changelog.sh" "$tag")
case "$body" in
  '#'*) ;;
  *)
    printf '%s\n' "$body" | awk 'NF == 0 { exit } { print }'
    echo
    body=$(printf '%s\n' "$body" | awk 'seen { print } NF == 0 { seen = 1 }')
    ;;
esac
cat "$root/scripts/release-notes-head.md"
[ -z "$body" ] || printf '\n%s\n' "$body"
