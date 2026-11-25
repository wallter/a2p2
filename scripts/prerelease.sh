#!/usr/bin/env bash
# The local gate before a tag (make prerelease; docs/RELEASING.md): check,
# lint, vuln, check-linux and one chaos run at the same time, each to its
# own log in .prerelease/. When every step passed and the tree was clean
# and on the same commit from start to end, it writes .prerelease/stamp:
# "<commit> <UTC time>", which `make tag` requires to name HEAD.
set -u
cd "$(dirname "$0")/.."
MAKE=${MAKE:-make}
dir=.prerelease
mkdir -p "$dir"
rm -f "$dir/stamp"
head0=$(git rev-parse HEAD)
clean0=$(git status --porcelain)
[ -z "$clean0" ] || echo "prerelease: the tree is not clean, so no stamp will be written (git status)"
# A release is never made with the web page's script unrun: without node
# the web-js step of check fails here instead of being skipped.
export A2P2_REQUIRE_NODE=1

scripts/gate.sh -l "$dir" \
  check="$MAKE --no-print-directory check" \
  lint="$MAKE --no-print-directory lint" \
  vuln="$MAKE --no-print-directory vuln" \
  check-linux="$MAKE --no-print-directory check-linux" \
  chaos="$MAKE --no-print-directory chaos"
rc=$?
[ "$rc" = 0 ] || exit "$rc"

if [ -n "$clean0" ] || [ -n "$(git status --porcelain)" ] || [ "$(git rev-parse HEAD)" != "$head0" ]; then
  echo "prerelease: passed, but the tree was not clean or HEAD moved: no stamp"
  exit 0
fi
echo "$head0 $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$dir/stamp"
echo "prerelease: stamp written for $head0 ($dir/stamp); next: make tag VERSION=vX.Y.Z"
