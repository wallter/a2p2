#!/usr/bin/env bash
# Tag a release after the local gate passed on this commit (docs/RELEASING.md).
#
#   scripts/tag.sh v1.2.3        (make tag VERSION=v1.2.3)
#
# Refuses unless: the version is vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-rc.N;
# the working tree is clean; the stamp `make prerelease` writes names HEAD;
# HEAD is on REMOTE/BRANCH (origin/main) after a fetch; CHANGELOG.md has a
# section for the version (scripts/changelog.sh); and the tag exists neither
# here nor on REMOTE. Then it makes the annotated tag and pushes it, which
# starts the release workflow. A tag made by hand skips these checks.
#
# Environment: REMOTE (origin), BRANCH (main), STAMP (.prerelease/stamp).
set -euo pipefail
cd "$(dirname "$0")/.."
v=${1:-}
REMOTE=${REMOTE:-origin}
BRANCH=${BRANCH:-main}
STAMP=${STAMP:-.prerelease/stamp}
no() { echo "tag: refused: $*" >&2; exit 1; }

grep -qE '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$' <<< "$v" ||
  no "VERSION must look like v1.2.3 or v1.2.3-rc.1 (got '${v}')"
[ -z "$(git status --porcelain)" ] || no "the working tree is not clean (git status)"
head=$(git rev-parse HEAD)
[ -f "$STAMP" ] || no "no $STAMP: run make prerelease on this commit first"
read -r shead stime < "$STAMP" || true
[ "${shead:-}" = "$head" ] || no "$STAMP is for ${shead:-nothing}, not HEAD $head: run make prerelease"
git fetch --quiet "$REMOTE" "$BRANCH" || no "cannot fetch $REMOTE $BRANCH"
git merge-base --is-ancestor "$head" FETCH_HEAD || no "HEAD is not on $REMOTE/$BRANCH: push it first"
scripts/changelog.sh "$v" > /dev/null || no "CHANGELOG.md has no section for $v"
! git rev-parse -q --verify "refs/tags/$v" > /dev/null || no "tag $v already exists here"
[ -z "$(git ls-remote --tags "$REMOTE" "refs/tags/$v")" ] || no "tag $v already exists on $REMOTE"

git tag -a "$v" -m "$v" "$head"
git push "$REMOTE" "refs/tags/$v"
echo "tag: $v on $head (gate passed ${stime:-?}) pushed to $REMOTE; next: make verify-release VERSION=$v"
