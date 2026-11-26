#!/usr/bin/env bash
# Check a published release from this machine (docs/RELEASING.md).
#
#   scripts/verify-release.sh v1.2.3        (make verify-release VERSION=v1.2.3)
#
# Waits for the release workflow run of the tag, then: the release has
# exactly the six binaries and SHA256SUMS; each of those seven is
# downloaded by name into a temporary directory, which then holds those
# seven and nothing else; SHA256SUMS is exactly six lines `<sha256>  <name>`,
# one for each binary (no blank, repeated or other line); every binary
# matches its line and has a GitHub artifact attestation (`gh attestation
# verify`) made by this repository's release.yml from this tag
# (--source-ref refs/tags/<tag>) on a GitHub-hosted runner; only then is
# the binary for this machine run, and its `version` must report the tag. The
# attestation is required: releases up to v0.2.3 have none, and this script
# is only run for new tags. Prints the asset list and the first lines of the
# notes. Exits non-zero on any mismatch. Needs gh, signed in.
#
# Environment: REPO (default: the repository of the current directory, from
# `gh repo view`),
# WAIT (seconds to wait for the run to appear, 300).
set -euo pipefail
v=${1:?usage: verify-release.sh vX.Y.Z}
WAIT=${WAIT:-300}
no() { echo "verify-release: FAIL: $*" >&2; exit 1; }
command -v gh > /dev/null || no "gh is not installed"

# The run the tag push started.
id=""; waited=0
while :; do
  id=$(gh run list ${REPO:+--repo "$REPO"} --workflow release.yml --branch "$v" --event push --limit 1 \
    --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null || true)
  [ -n "$id" ] && break
  [ "$waited" -lt "$WAIT" ] || no "no release workflow run for $v after ${WAIT}s"
  sleep 5; waited=$((waited + 5))
done
echo "verify-release: waiting for run $id"
gh run watch ${REPO:+--repo "$REPO"} "$id" --exit-status --interval 10 > /dev/null || no "release workflow run $id did not succeed (gh run view $id --log-failed)"

want="SHA256SUMS
a2p2_darwin_amd64
a2p2_darwin_arm64
a2p2_linux_amd64
a2p2_linux_arm64
a2p2_windows_amd64.exe
a2p2_windows_arm64.exe"
got=$(gh release view ${REPO:+--repo "$REPO"} "$v" --json assets --jq '.assets[].name' | LC_ALL=C sort)
echo "verify-release: assets of $v:"; printf '  %s\n' $got
[ "$got" = "$want" ] || no "the assets are not exactly the six binaries and SHA256SUMS"

case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; MINGW*|MSYS*|CYGWIN*) os=windows ;; *) no "unknown system $(uname -s)" ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) no "unknown machine $(uname -m)" ;; esac
ext=""; [ "$os" = windows ] && ext=.exe
asset=a2p2_${os}_${arch}${ext}

dir=$(mktemp -d "${TMPDIR:-/tmp}/a2p2rel.XXXXXX")
trap 'rm -rf "$dir"' EXIT
# Each expected file by name, so an asset added after the listing is never
# fetched; then the directory must hold exactly those files.
for f in $want; do
  gh release download ${REPO:+--repo "$REPO"} "$v" -p "$f" -D "$dir" || no "cannot download $f"
done
[ "$(ls -A "$dir" | LC_ALL=C sort)" = "$want" ] || no "the downloaded files are not exactly the six binaries and SHA256SUMS"
repo=${REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)} || no "cannot work out the repository"
bins=$(printf '%s\n' "$want" | grep -v '^SHA256SUMS$')
# SHA256SUMS, one to one: every line is `<64 hex>  <name>` (so no blank
# line), and the names, sorted, are the six binaries, each once.
[ "$(grep -Evc '^[0-9a-f]{64}  a2p2_[a-z0-9_]+(\.exe)?$' "$dir/SHA256SUMS")" = 0 ] || no "SHA256SUMS has a line that is not '<sha256>  <binary>'"
[ "$(sed 's/^[0-9a-f]*  //' "$dir/SHA256SUMS" | LC_ALL=C sort)" = "$bins" ] || no "SHA256SUMS does not have exactly one line for each of the six binaries"
# Every binary, not only this machine's: a user on another system installs
# one of the others. A swapped file with a matching SHA256SUMS line has no
# attestation, and an attested build of another tag has none from this tag.
for b in $bins; do
  line=$(grep -E "^[0-9a-f]{64}  $b\$" "$dir/SHA256SUMS")
  if command -v sha256sum > /dev/null; then sum=$(sha256sum "$dir/$b"); else sum=$(shasum -a 256 "$dir/$b"); fi
  [ "${sum%% *}" = "${line%% *}" ] || no "the checksum of $b does not match SHA256SUMS"
  gh attestation verify "$dir/$b" --repo "$repo" --signer-workflow "$repo/.github/workflows/release.yml" \
    --source-ref "refs/tags/$v" --deny-self-hosted-runners > /dev/null \
    || no "$b has no valid attestation from $repo/.github/workflows/release.yml for $v"
  echo "verify-release: $b matches SHA256SUMS and its attestation (built by $repo release.yml from $v)"
done
chmod +x "$dir/$asset"
out=$("$dir/$asset" version) || no "$asset version failed"
[ "$out" = "a2p2 $v" ] || no "$asset reports '$out', not 'a2p2 $v'"
echo "verify-release: $asset version: $out"
notes=$(gh release view ${REPO:+--repo "$REPO"} "$v" --json body --jq .body | tr -d '\r')
head=$(cat "$(dirname "$0")/release-notes-head.md")
case "$notes" in
  *"$head"*) ;;
  *) no "the notes of $v do not contain scripts/release-notes-head.md" ;;
esac
echo "verify-release: the notes contain the install block"
echo "verify-release: notes of $v (first lines):"
printf '%s\n' "$notes" | head -20 | sed 's/^/  /'
echo "verify-release: $v ok"
