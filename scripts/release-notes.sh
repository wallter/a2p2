#!/usr/bin/env bash
# Print the release notes for a version tag: the fixed install block
# (scripts/release-notes-head.md), a blank line, then that version's
# CHANGELOG.md section (scripts/changelog.sh).
#   scripts/release-notes.sh v0.2.2
# The release workflow publishes this output, and scripts/verify-release.sh
# checks that the published notes begin with the same block.
set -euo pipefail
tag=${1:?usage: release-notes.sh <tag>}
root=$(cd "$(dirname "$0")/.." && pwd)
body=$("$root/scripts/changelog.sh" "$tag")
cat "$root/scripts/release-notes-head.md"
printf '\n%s\n' "$body"
