#!/bin/sh
# Write the databases of one tagged release for the compatibility test
# (docs/RELEASING.md; DECISIONS, D61).
#
#   scripts/release-db.sh v0.2.3
#
# Copies the tag's source out of git into a temporary directory (git
# archive; nothing is downloaded but Go modules the tag needs), adds
# internal/node/testdata/releases/gen_test.go.txt there as a test file and
# runs it with that release's own code. It writes
# internal/node/testdata/releases/<tag>/: a.sql, b.sql, c.sql (three
# machines of one room, as SQL text) and manifest.json. Commit them.
# TestDatabasesFromEachReleaseOpenReplayAndKeepWorking then opens each with
# the current code. Starts no daemon and touches no real state.
set -eu
cd "$(dirname "$0")/.."
v=${1:?usage: release-db.sh vX.Y.Z}
case "$v" in
  v[0-9]*) ;;
  *) echo "release-db: '$v' is not a version tag" >&2; exit 2 ;;
esac
case "$v" in *[!A-Za-z0-9._-]*) echo "release-db: '$v' is not a version tag" >&2; exit 2 ;; esac
git rev-parse -q --verify "refs/tags/$v" > /dev/null || { echo "release-db: no tag $v here" >&2; exit 1; }
repo=$(pwd)
rels=$repo/internal/node/testdata/releases
out=$rels/$v
# One run per tag at a time; a second one stops here and changes nothing.
lock=$rels/.$v.lock
mkdir "$lock" 2> /dev/null || { echo "release-db: $lock exists: another run for $v, or one that was cut off (remove it)" >&2; exit 1; }
work=""; new=""; old=""
# The databases in place are replaced only by a complete new set. If the
# run is cut off between the two renames below, the old set is put back.
done_() {
  [ -z "$old" ] || [ -e "$out" ] || mv "$old" "$out"
  [ -z "$old" ] || rm -rf "$old"
  [ -z "$new" ] || rm -rf "$new"
  [ -z "$work" ] || rm -rf "$work"
  rmdir "$lock"
}
trap done_ EXIT
trap 'exit 1' INT TERM HUP
work=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-release-db.XXXXXX")
git archive --format=tar -o "$work/src.tar" "$v"
mkdir "$work/src"
tar -xf "$work/src.tar" -C "$work/src"
cp internal/node/testdata/releases/gen_test.go.txt "$work/src/internal/node/zz_release_db_test.go"
new=$(mktemp -d "$rels/.$v.new.XXXXXX")
(cd "$work/src" && A2P2_RELEASE_DB_OUT="$new" go test -count=1 -run '^TestWriteReleaseDatabases$' ./internal/node)
for f in a.sql b.sql c.sql manifest.json; do
  [ -s "$new/$f" ] || { echo "release-db: the generator wrote no $f; nothing was changed" >&2; exit 1; }
done
chmod 0755 "$new"
if [ -e "$out" ]; then
  old=$rels/.$v.old.$$
  mv "$out" "$old"
fi
mv "$new" "$out"
new=""
echo "release-db: wrote $out"
