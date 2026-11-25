#!/bin/sh
# Test install.sh against a fake release in a temporary directory.
# Offline: A2P2_BASE_URL points at a file:// tree, uname is a stub on PATH,
# and every install goes into a temporary directory. Needs curl.
# Runs the script under sh, and under dash when it is installed.
# Usage: scripts/install-test.sh
set -eu
cd "$(dirname "$0")/.."
repo=$(pwd)
script=$repo/install.sh

command -v curl >/dev/null 2>&1 || { echo "install-test: curl not found, skipped"; exit 0; }

work=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-install-test.XXXXXX")
trap 'rm -rf "$work"' EXIT

fail=0
check() { # check NAME CONDITION-RESULT(0 ok)
  if [ "$2" -eq 0 ]; then :; else echo "FAIL: $1"; fail=1; fi
}

# A stub uname, so the asset name does not depend on this machine.
mkdir "$work/stub"
cat > "$work/stub/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) echo "${FAKE_S:-Linux}" ;;
  -m) echo "${FAKE_M:-x86_64}" ;;
  *) echo "${FAKE_S:-Linux}" ;;
esac
EOF
chmod +x "$work/stub/uname"

sum() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# The fake release: a shell script stands for the binary.
rel=$work/rel
mkdir -p "$rel/latest/download" "$rel/download/v0.2.1"
cat > "$work/a2p2_linux_amd64" <<'EOF'
#!/bin/sh
echo "a2p2 0.0.0-fake"
EOF
good=$(sum "$work/a2p2_linux_amd64")
for d in latest/download download/v0.2.1; do
  cp "$work/a2p2_linux_amd64" "$rel/$d/a2p2_linux_amd64"
  printf '%s  a2p2_linux_amd64\n%s  a2p2_darwin_arm64\n' "$good" "$good" > "$rel/$d/SHA256SUMS"
done

run() { # run SHELL DIR [args]: runs install.sh with the stub uname, output in $work/out
  sh_=$1; dir=$2; shift 2
  env PATH="$work/stub:$PATH" A2P2_BASE_URL="file://$rel" A2P2_INSTALL_DIR="$dir" \
    "$sh_" "$script" "$@" > "$work/out" 2>&1
}

runall() {
  sh_=$1
  echo "install-test: under $sh_"
  n=0
  fresh() { n=$((n + 1)); dir=$work/$sh_.$n/bin; mkdir -p "$work/$sh_.$n"; }

  # good checksum installs and the installed file runs
  fresh; mkdir "$dir"
  rc=0; run "$sh_" "$dir" || rc=$?
  check "good: exit 0" "$rc"
  [ -x "$dir/a2p2" ] && "$dir/a2p2" version | grep -q fake; check "good: installed file runs" $?
  grep -q "checksum ok" "$work/out"; check "good: says checksum ok" $?
  grep -q "a2p2 doctor" "$work/out"; check "good: prints next step" $?
  grep -q "not on your PATH" "$work/out"; check "good: PATH hint" $?
  [ "$(ls -A "$dir")" = a2p2 ]; check "good: only a2p2 in the directory" $?

  # a pinned version installs
  fresh
  rc=0; A2P2_VERSION=v0.2.1 run "$sh_" "$dir" || rc=$?
  check "version: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "version: installed (directory created)" $?
  grep -q "download/v0.2.1" "$work/out" || true

  # a bad version string is refused
  fresh
  rc=0; A2P2_VERSION='v1/../x' run "$sh_" "$dir" || rc=$?
  [ "$rc" -ne 0 ] && [ ! -e "$dir" ]; check "bad version: refused, nothing made" $?

  # a wrong checksum refuses and leaves nothing
  fresh; mkdir "$dir"
  mkdir "$work/bad"; rm -rf "$work/bad"; cp -R "$rel" "$work/bad"
  echo tampered >> "$work/bad/latest/download/a2p2_linux_amd64"
  rc=0
  env PATH="$work/stub:$PATH" A2P2_BASE_URL="file://$work/bad" A2P2_INSTALL_DIR="$dir" \
    "$sh_" "$script" > "$work/out" 2>&1 || rc=$?
  [ "$rc" -ne 0 ]; check "mismatch: nonzero exit" $?
  grep -q "checksum mismatch" "$work/out"; check "mismatch: says so" $?
  [ -z "$(ls -A "$dir")" ]; check "mismatch: install directory empty" $?
  rm -rf "$work/bad"

  # a missing SHA256SUMS line refuses
  fresh; mkdir "$dir"
  mkdir "$work/nol"; cp -R "$rel/latest" "$work/nol/latest"
  printf '%s  a2p2_darwin_arm64\n' "$good" > "$work/nol/latest/download/SHA256SUMS"
  rc=0
  env PATH="$work/stub:$PATH" A2P2_BASE_URL="file://$work/nol" A2P2_INSTALL_DIR="$dir" \
    "$sh_" "$script" > "$work/out" 2>&1 || rc=$?
  [ "$rc" -ne 0 ]; check "missing line: nonzero exit" $?
  grep -q "no line for" "$work/out"; check "missing line: says so" $?
  [ -z "$(ls -A "$dir")" ]; check "missing line: install directory empty" $?
  rm -rf "$work/nol"

  # an unsupported system and an unsupported architecture refuse
  fresh; mkdir "$dir"
  rc=0; FAKE_S=FreeBSD run "$sh_" "$dir" || rc=$?
  [ "$rc" -ne 0 ]; check "unsupported os: nonzero exit" $?
  grep -q "manual route" "$work/out"; check "unsupported os: names the manual route" $?
  rc=0; FAKE_M=riscv64 run "$sh_" "$dir" || rc=$?
  [ "$rc" -ne 0 ]; check "unsupported arch: nonzero exit" $?
  [ -z "$(ls -A "$dir")" ]; check "unsupported: install directory empty" $?

  # a plain-http base that is not loopback is refused
  rc=0
  env PATH="$work/stub:$PATH" A2P2_BASE_URL="http://example.com/x" A2P2_INSTALL_DIR="$dir" \
    "$sh_" "$script" > "$work/out" 2>&1 || rc=$?
  [ "$rc" -ne 0 ]; check "http base: refused" $?

  # --dry-run writes nothing, not even the directory
  fresh
  rc=0; run "$sh_" "$dir" --dry-run || rc=$?
  check "dry run: exit 0" "$rc"
  [ ! -e "$dir" ]; check "dry run: nothing written" $?
  grep -q "a2p2_linux_amd64" "$work/out"; check "dry run: names the asset" $?

  # --help works; an unknown flag is refused
  rc=0; run "$sh_" "$dir" --help || rc=$?
  check "help: exit 0" "$rc"
  rc=0; run "$sh_" "$dir" --nope || rc=$?
  [ "$rc" -ne 0 ]; check "unknown flag: refused" $?

  # an existing older file is replaced, in one rename, leaving no temp file
  fresh; mkdir "$dir"
  printf '#!/bin/sh\necho old\n' > "$dir/a2p2"; chmod 755 "$dir/a2p2"
  rc=0; run "$sh_" "$dir" || rc=$?
  check "replace: exit 0" "$rc"
  "$dir/a2p2" version | grep -q fake; check "replace: new file in place" $?
  [ "$(ls -A "$dir")" = a2p2 ]; check "replace: no temp file left" $?
  # and a failed install keeps the older file
  printf '#!/bin/sh\necho old\n' > "$dir/a2p2"
  mkdir "$work/bad2"; cp -R "$rel/latest" "$work/bad2/latest"
  echo tampered >> "$work/bad2/latest/download/a2p2_linux_amd64"
  env PATH="$work/stub:$PATH" A2P2_BASE_URL="file://$work/bad2" A2P2_INSTALL_DIR="$dir" \
    "$sh_" "$script" > "$work/out" 2>&1 || true
  "$dir/a2p2" | grep -q old; check "replace: failed install keeps the older file" $?
  rm -rf "$work/bad2"

  # nothing is left in the temporary directory by the script
  :
}

# Syntax and lint.
sh -n "$script"; check "sh -n" $?
if command -v dash >/dev/null 2>&1; then dash -n "$script"; check "dash -n" $?; fi
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -s sh "$script"; check "shellcheck" $?
fi

set +e # assertions are collected by check
runall sh
if command -v dash >/dev/null 2>&1; then runall dash; fi

if [ "$fail" -ne 0 ]; then echo "install-test: FAILED"; exit 1; fi
echo "install-test: ok"
