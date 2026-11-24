#!/usr/bin/env bash
# Check a published release from this machine (docs/RELEASING.md).
#
#   scripts/verify-release.sh v1.2.3        (make verify-release VERSION=v1.2.3)
#
# Waits for the release workflow run of the tag, then: the release has
# exactly the six binaries and SHA256SUMS; the binary for this machine and
# SHA256SUMS download into a temporary directory; the checksum matches; the
# binary's `version` reports the tag. Prints the asset list and the first
# lines of the notes. Exits non-zero on any mismatch. Needs gh, signed in.
#
# Environment: REPO (default: the repository of the current directory),
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
gh release download ${REPO:+--repo "$REPO"} "$v" -p "$asset" -p SHA256SUMS -D "$dir"
line=$(grep -E "^[0-9a-f]{64}  $asset\$" "$dir/SHA256SUMS" || true)
[ -n "$line" ] || no "SHA256SUMS has no line for $asset"
if command -v sha256sum > /dev/null; then sum=$(sha256sum "$dir/$asset"); else sum=$(shasum -a 256 "$dir/$asset"); fi
[ "${sum%% *}" = "${line%% *}" ] || no "the checksum of $asset does not match SHA256SUMS"
echo "verify-release: $asset matches SHA256SUMS"
chmod +x "$dir/$asset"
out=$("$dir/$asset" version) || no "$asset version failed"
[ "$out" = "a2p2 $v" ] || no "$asset reports '$out', not 'a2p2 $v'"
echo "verify-release: $asset version: $out"
echo "verify-release: notes of $v (first lines):"
gh release view ${REPO:+--repo "$REPO"} "$v" --json body --jq .body | head -12 | sed 's/^/  /'
echo "verify-release: $v ok"
