#!/bin/sh
# Test install.sh against a fake release in a temporary directory.
# Offline: A2P2_BASE_URL points at a file:// tree (or a loopback server
# started here with python3), uname is a stub on PATH, TMPDIR is a
# controlled directory and every install goes into a temporary directory.
# Needs curl. Runs the script under sh, and under dash when installed.
# Usage: scripts/install-test.sh
set -eu
cd "$(dirname "$0")/.."
repo=$(pwd)
script=$repo/install.sh

command -v curl >/dev/null 2>&1 || { echo "install-test: curl not found, skipped"; exit 0; }

work=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-install-test.XXXXXX")
srvpid=""
cleanup() {
  if [ -n "$srvpid" ]; then kill "$srvpid" 2>/dev/null; wait "$srvpid" 2>/dev/null; fi
  rm -rf "$work"
}
trap cleanup EXIT

fail=0
check() { # check NAME STATUS(0 ok)
  if [ "$2" -eq 0 ]; then :; else echo "FAIL: $1 [$cur]"; fail=1; fi
}
cur=""

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

# A cp that writes other bytes, to prove the staged copy is hashed again.
mkdir "$work/cpstub"
cat > "$work/cpstub/cp" <<'EOF'
#!/bin/sh
for last; do :; done
printf 'other bytes' > "$last"
EOF
chmod +x "$work/cpstub/cp"

# A PATH with the tools install.sh needs but no hash tool.
mkdir "$work/hstub"
mkdir "$work/nohash"
for t in curl awk mktemp cp chmod mv rm wc tr cat mkdir; do
  p=$(command -v "$t") || continue
  ln -s "$p" "$work/nohash/$t"
done

sum() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum < "$1" | awk '{print $1}'
  else shasum -a 256 < "$1" | awk '{print $1}'; fi
}

# The fake release. Shell scripts stand for the binary, and the latest and
# the pinned release print different text, so a run shows which it fetched.
rel=$work/rel
mkdir -p "$rel/latest/download" "$rel/download/v0.2.1"
printf '#!/bin/sh\necho "a2p2 fake-latest"\n' > "$work/b-latest"
printf '#!/bin/sh\necho "a2p2 fake-v0.2.1"\n' > "$work/b-pinned"
h_latest=$(sum "$work/b-latest")
h_pinned=$(sum "$work/b-pinned")
cp "$work/b-latest" "$rel/latest/download/a2p2_linux_amd64"
printf '%s  a2p2_linux_amd64\n%s  a2p2_darwin_arm64\n' "$h_latest" "$h_latest" > "$rel/latest/download/SHA256SUMS"
cp "$work/b-pinned" "$rel/download/v0.2.1/a2p2_linux_amd64"
printf '%s  a2p2_linux_amd64\n%s  a2p2_darwin_arm64\n' "$h_pinned" "$h_pinned" > "$rel/download/v0.2.1/SHA256SUMS"

mkdir "$work/tmp"

# A loopback server for the redirect case: /real/ serves the release, and
# /redir/ answers every request with a redirect to /real/.
httpport=""
if command -v python3 >/dev/null 2>&1; then
  mkdir "$work/www"
  ln -s "$rel" "$work/www/real"
  cat > "$work/srv.py" <<'EOF'
import http.server, sys
class H(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith('/redir/'):
            self.send_response(302)
            self.send_header('Location', '/real/' + self.path[len('/redir/'):])
            self.end_headers()
            return
        super().do_GET()
    def log_message(self, *a):
        pass
srv = http.server.ThreadingHTTPServer(('127.0.0.1', 0), H)
open(sys.argv[1], 'w').write(str(srv.server_address[1]))
srv.serve_forever()
EOF
  (cd "$work/www" && exec python3 -I "$work/srv.py" "$work/port") > /dev/null 2>&1 &
  srvpid=$!
  i=0
  while [ ! -s "$work/port" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
  [ -s "$work/port" ] && httpport=$(cat "$work/port")
fi

# run SHELL DIR BASE PATH [args]: install.sh with the stub uname and a
# controlled TMPDIR; "-" for BASE or PATH means the default. Output in
# $work/out, status in $rc. After every run the temporary directory must be
# empty and no staging file may be left in the install directory.
rc=0
run() {
  sh_=$1; dir=$2; b=$3; p=$4; shift 4
  [ "$b" != - ] || b="file://$rel"
  [ "$p" != - ] || p="$work/stub:$PATH"
  shp=$(PATH="$p" command -v "$sh_" 2>/dev/null || command -v "$sh_")
  rc=0
  env PATH="$p" A2P2_BASE_URL="$b" A2P2_INSTALL_DIR="$dir" TMPDIR="$work/tmp" \
    "$shp" "$script" "$@" > "$work/out" 2>&1 || rc=$?
  [ -z "$(ls -A "$work/tmp")" ]; check "TMPDIR empty after the run" $?
  if [ -d "$dir" ]; then
    [ -z "$(find "$dir" -name '.a2p2.new.*' 2>/dev/null)" ]; check "no staging file left" $?
  fi
  ls -A "$work/tmp" > /dev/null
  return 0
}

empty() { [ -z "$(ls -A "$1")" ]; }

runall() {
  sh_=$1
  echo "install-test: under $sh_"
  n=0
  fresh() { n=$((n + 1)); mkdir "$work/$sh_.$n"; dir=$work/$sh_.$n/bin; }

  cur="good latest"
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - -
  check "exit 0" "$rc"
  [ -x "$dir/a2p2" ] && "$dir/a2p2" version | grep -q fake-latest; check "installed the latest fixture" $?
  grep -q "checksum ok" "$work/out"; check "says checksum ok" $?
  grep -q "a2p2 doctor" "$work/out"; check "prints next step" $?
  grep -q 'export PATH="' "$work/out"; check "PATH hint with export line" $?
  [ "$(ls -A "$dir")" = a2p2 ]; check "only a2p2 in the directory" $?

  cur="pinned version"
  fresh
  export A2P2_VERSION=v0.2.1
  run "$sh_" "$dir" - -
  check "exit 0" "$rc"
  "$dir/a2p2" version | grep -q fake-v0.2.1; check "installed the pinned fixture" $?
  run "$sh_" "$dir" - - --dry-run
  grep -q "file://$rel/download/v0.2.1/a2p2_linux_amd64" "$work/out"; check "dry run names the pinned URL" $?
  unset A2P2_VERSION
  run "$sh_" "$dir" - - --dry-run
  grep -q "file://$rel/latest/download/a2p2_linux_amd64" "$work/out"; check "dry run names the latest URL" $?

  cur="bad version"
  fresh
  export A2P2_VERSION='v1/../x'
  run "$sh_" "$dir" - -
  unset A2P2_VERSION
  [ "$rc" -ne 0 ] && [ ! -e "$dir" ]; check "refused, nothing made" $?

  cur="wrong checksum"
  fresh; mkdir "$dir"
  rm -rf "$work/bad"; cp -R "$rel" "$work/bad"
  echo tampered >> "$work/bad/latest/download/a2p2_linux_amd64"
  run "$sh_" "$dir" "file://$work/bad" -
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  grep -q "checksum mismatch" "$work/out"; check "says so" $?
  empty "$dir"; check "install directory empty" $?
  fresh
  run "$sh_" "$dir" "file://$work/bad" -
  [ ! -e "$dir" ]; check "a missing directory is not created" $?
  rm -rf "$work/bad"

  cur="missing line"
  fresh; mkdir "$dir"
  rm -rf "$work/nol"; mkdir "$work/nol"; cp -R "$rel/latest" "$work/nol/latest"
  printf '%s  a2p2_darwin_arm64\n' "$h_latest" > "$work/nol/latest/download/SHA256SUMS"
  run "$sh_" "$dir" "file://$work/nol" -
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  grep -q "no line for" "$work/out"; check "says so" $?
  empty "$dir"; check "install directory empty" $?

  cur="duplicate lines"
  fresh; mkdir "$dir"
  rm -rf "$work/dup"; mkdir "$work/dup"; cp -R "$rel/latest" "$work/dup/latest"
  printf '%s  a2p2_linux_amd64\n%s  a2p2_linux_amd64\n' "$h_latest" "$h_latest" > "$work/dup/latest/download/SHA256SUMS"
  run "$sh_" "$dir" "file://$work/dup" -
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  grep -q "more than one line" "$work/out"; check "says so" $?
  empty "$dir"; check "install directory empty" $?

  cur="malformed hash"
  fresh; mkdir "$dir"
  printf 'abc123  a2p2_linux_amd64\n' > "$work/dup/latest/download/SHA256SUMS"
  run "$sh_" "$dir" "file://$work/dup" -
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  empty "$dir"; check "install directory empty" $?
  rm -rf "$work/dup" "$work/nol"

  cur="hash tool misbehaves"
  fresh; mkdir "$dir"
  # prints the right hash but fails
  printf '#!/bin/sh\necho "%s  -"\nexit 1\n' "$h_latest" > "$work/hstub/sha256sum"
  chmod +x "$work/hstub/sha256sum"
  run "$sh_" "$dir" - "$work/hstub:$work/stub:$PATH"
  [ "$rc" -ne 0 ]; check "a failing hash tool: nonzero exit" $?
  empty "$dir"; check "a failing hash tool: nothing installed" $?
  # prints something that is not a hash, and SHA256SUMS says the same
  printf '#!/bin/sh\necho "notahash  -"\n' > "$work/hstub/sha256sum"
  rm -rf "$work/odd"; mkdir "$work/odd"; cp -R "$rel/latest" "$work/odd/latest"
  printf 'notahash  a2p2_linux_amd64\n' > "$work/odd/latest/download/SHA256SUMS"
  run "$sh_" "$dir" "file://$work/odd" "$work/hstub:$work/stub:$PATH"
  [ "$rc" -ne 0 ]; check "a non-hash: nonzero exit" $?
  empty "$dir"; check "a non-hash: nothing installed" $?
  rm -rf "$work/odd"

  cur="no hash tool"
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - "$work/stub:$work/nohash"
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  grep -q "needs sha256sum or shasum" "$work/out"; check "says so" $?
  empty "$dir"; check "install directory empty" $?

  cur="unsupported system"
  fresh; mkdir "$dir"
  export FAKE_S=FreeBSD
  run "$sh_" "$dir" - -
  unset FAKE_S
  [ "$rc" -ne 0 ]; check "os: nonzero exit" $?
  grep -q "manual route" "$work/out"; check "os: names the manual route" $?
  export FAKE_M=riscv64
  run "$sh_" "$dir" - -
  unset FAKE_M
  [ "$rc" -ne 0 ]; check "arch: nonzero exit" $?
  empty "$dir"; check "install directory empty" $?

  cur="refused base URLs"
  fresh; mkdir "$dir"
  for b in "http://example.com/x" "http://localhost:1@example.com/x" "http://127.0.0.1@example.com/" \
           "https://example.com/x?y=1" "https://example.com/x#y" "https://example.com/a b" \
           "http://localhost.example.com/x" "ftp://example.com/x"; do
    run "$sh_" "$dir" "$b" -
    [ "$rc" -ne 0 ]; check "refused: $b" $?
    grep -q "A2P2_BASE_URL must" "$work/out"; check "refused by the URL check, not by a failed download: $b" $?
  done
  empty "$dir"; check "install directory empty" $?

  cur="dry run"
  fresh
  run "$sh_" "$dir" - - --dry-run
  check "exit 0" "$rc"
  [ ! -e "$dir" ]; check "nothing written" $?
  grep -q "a2p2_linux_amd64" "$work/out"; check "names the asset" $?

  cur="help and flags"
  fresh
  run "$sh_" "$dir" - - --help
  check "help exit 0" "$rc"
  run "$sh_" "$dir" - - --nope
  [ "$rc" -ne 0 ]; check "unknown flag refused" $?

  cur="replace older file"
  fresh; mkdir "$dir"
  printf '#!/bin/sh\necho old\n' > "$dir/a2p2"; chmod 755 "$dir/a2p2"
  run "$sh_" "$dir" - -
  check "exit 0" "$rc"
  "$dir/a2p2" version | grep -q fake-latest; check "new file in place" $?
  [ "$(ls -A "$dir")" = a2p2 ]; check "no temp file left" $?
  printf '#!/bin/sh\necho old\n' > "$dir/a2p2"
  rm -rf "$work/bad"; cp -R "$rel" "$work/bad"
  echo tampered >> "$work/bad/latest/download/a2p2_linux_amd64"
  run "$sh_" "$dir" "file://$work/bad" -
  "$dir/a2p2" | grep -q old; check "a failed install keeps the older file" $?
  rm -rf "$work/bad"

  cur="staged copy differs"
  fresh; mkdir "$dir"
  printf '#!/bin/sh\necho old\n' > "$dir/a2p2"; chmod 755 "$dir/a2p2"
  run "$sh_" "$dir" - "$work/cpstub:$work/stub:$PATH"
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  grep -q "does not match" "$work/out"; check "says so" $?
  "$dir/a2p2" | grep -q old; check "the older file is kept" $?

  cur="target is a directory"
  fresh; mkdir "$dir" "$dir/a2p2"
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "nonzero exit" $?
  grep -q "is a directory" "$work/out"; check "says so" $?
  empty "$dir/a2p2"; check "nothing moved into it" $?
  fresh; mkdir "$dir" "$work/$sh_.$n/elsewhere"
  ln -s "$work/$sh_.$n/elsewhere" "$dir/a2p2"
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "a link to a directory: nonzero exit" $?
  empty "$work/$sh_.$n/elsewhere"; check "a link to a directory: nothing moved into it" $?

  cur="target is a symlink"
  fresh; mkdir "$dir"
  printf '#!/bin/sh\necho linked-old\n' > "$work/$sh_.$n/real"; chmod 755 "$work/$sh_.$n/real"
  ln -s "$work/$sh_.$n/real" "$dir/a2p2"
  run "$sh_" "$dir" - -
  check "exit 0" "$rc"
  [ ! -L "$dir/a2p2" ]; check "the link itself is replaced" $?
  grep -q "linked-old" "$work/$sh_.$n/real"; check "the old target is untouched" $?
  grep -q "symbolic link" "$work/out"; check "says a link was replaced" $?

  cur="directory with a space"
  n=$((n + 1)); mkdir "$work/$sh_.$n"; dir="$work/$sh_.$n/sp ace/bin"
  run "$sh_" "$dir" - -
  check "exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "installed" $?
  ! grep -q 'export PATH' "$work/out"; check "no export line" $?
  grep -q "Add this directory to your PATH" "$work/out"; check "says to add the directory" $?

  cur="redirects"
  if [ -n "$httpport" ]; then
    fresh; mkdir "$dir"
    run "$sh_" "$dir" "http://127.0.0.1:$httpport/real" -
    check "loopback http without a redirect: exit 0" "$rc"
    [ -x "$dir/a2p2" ]; check "loopback http installs" $?
    fresh; mkdir "$dir"
    run "$sh_" "$dir" "http://127.0.0.1:$httpport/redir" -
    [ "$rc" -ne 0 ]; check "a redirect is not followed: nonzero exit" $?
    empty "$dir"; check "a redirect is not followed: nothing installed" $?
  else
    echo "install-test: python3 not found, redirect case skipped"
  fi

  cur="truncated script"
  # A copy cut anywhere inside the last 40 bytes, short of the closing brace
  # group, must not parse or must do nothing. (Cutting only the final newline
  # leaves the whole script.)
  len=$(wc -c < "$script" | tr -d ' ')
  cut=$((len - 40))
  while [ "$cut" -le $((len - 2)) ]; do
    fresh; mkdir "$dir"
    head -c "$cut" "$script" > "$work/trunc.sh"
    shp=$(command -v "$sh_")
    env PATH="$work/stub:$PATH" A2P2_BASE_URL="file://$rel" A2P2_INSTALL_DIR="$dir" TMPDIR="$work/tmp" \
      "$shp" "$work/trunc.sh" --dry-run > "$work/out" 2>&1 || true
    # Without the closing brace the script must not have run main at all.
    ! grep -q "dry run\|downloading\|installed" "$work/out"; check "cut at $cut of $len ran something" $?
    empty "$dir"; check "cut at $cut of $len installed something" $?
    cut=$((cut + 1))
  done
  fresh; mkdir "$dir"
  head -c "$((len - 2))" "$script" > "$work/trunc.sh"
  env PATH="$work/stub:$PATH" A2P2_BASE_URL="file://$rel" A2P2_INSTALL_DIR="$dir" TMPDIR="$work/tmp" \
    "$(command -v "$sh_")" "$work/trunc.sh" > "$work/out" 2>&1 || true
  empty "$dir"; check "a cut script installs nothing" $?
  empty "$work/tmp"; check "a cut script leaves nothing in TMPDIR" $?
}

# Syntax and lint.
cur="syntax"
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
