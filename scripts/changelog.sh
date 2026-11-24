#!/usr/bin/env bash
# Print the section of CHANGELOG.md for a version tag.
#   scripts/changelog.sh v0.1.0        the "## [0.1.0]" section
#   scripts/changelog.sh v0.1.0-rc.1   that section if it exists, else "## [Unreleased]"
# Exits non-zero if there is no such section or it is empty.
set -euo pipefail
tag=${1:?usage: changelog.sh <tag>}
root=$(cd "$(dirname "$0")/.." && pwd)
ver=${tag#v}
section() {
  awk -v want="$1" '
    /^## \[/ { on = (index($0, "## [" want "]") == 1); next }
    on { print }
  ' "$root/CHANGELOG.md" | sed -e '/./,$!d' | awk '{ a[NR] = $0 } END { n = NR; while (n > 0 && a[n] == "") n--; for (i = 1; i <= n; i++) print a[i] }'
}
out=$(section "$ver")
if [ -z "$out" ]; then
  case "$ver" in
    *-*) out=$(section "${ver%%-*}"); [ -n "$out" ] || out=$(section "Unreleased") ;;
  esac
fi
[ -n "$out" ] || { echo "CHANGELOG.md has no section for $tag" >&2; exit 1; }
printf '%s\n' "$out"
