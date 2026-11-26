#!/bin/sh
# Test install.sh against a fake release in a temporary directory.
# Offline: A2P2_BASE_URL points at a file:// tree (or a loopback server
# started here with python3), uname is a stub on PATH, TMPDIR is a
# controlled directory and every install goes into a temporary directory.
# Needs curl. Runs the script under sh, and under dash when installed.
# Usage: scripts/install-test.sh
set -eu
# A fixed umask, so the directories made here have known modes.
umask 022
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
for t in curl awk mktemp cp chmod mv rm wc tr cat mkdir find id dirname ls readlink; do
  p=$(command -v "$t") || continue
  ln -s "$p" "$work/nohash/$t"
done
# The same with a hash tool but without gh, and without getent, dscl and
# dscacheutil, so install.sh cannot list who is in a group.
mkdir "$work/nogh"
for t in curl awk mktemp cp chmod mv rm wc tr cat mkdir find id dirname ls readlink sha256sum shasum; do
  p=$(command -v "$t") || continue
  ln -s "$p" "$work/nogh/$t"
done

# A stub getent: `getent group GID` prints $FAKE_GROUP:x:GID:$FAKE_MEMBERS
# (comma-separated), and `getent passwd` prints $FAKE_PASSWD, as if those
# were the system's lists.
mkdir "$work/getent"
cat > "$work/getent/getent" <<'EOF'
#!/bin/sh
case "$1" in
  group) printf '%s:x:%s:%s\n' "$FAKE_GROUP" "$2" "${FAKE_MEMBERS:-}" ;;
  passwd) [ -z "${FAKE_PASSWD:-}" ] || printf '%s\n' "$FAKE_PASSWD" ;;
  *) exit 2 ;;
esac
EOF
chmod +x "$work/getent/getent"
# Stub dscacheutil and dscl, as on macOS, for a PATH without getent.
mkdir "$work/dsc"
cat > "$work/dsc/dscacheutil" <<'EOF'
#!/bin/sh
[ "$1 $2 $3 $4" = "-q group -a gid" ] || exit 2
printf 'name: %s\npassword: *\ngid: %s\nusers: %s\n\n' "$FAKE_GROUP" "$5" "${FAKE_MEMBERS:-}"
EOF
cat > "$work/dsc/dscl" <<'EOF'
#!/bin/sh
[ "$*" = ". -list /Users PrimaryGroupID" ] || exit 2
[ -z "${FAKE_PASSWD:-}" ] || printf '%s\n' "$FAKE_PASSWD"
EOF
chmod +x "$work/dsc/dscacheutil" "$work/dsc/dscl"
# A stub dsmemberutil: the account nobody is a member of every group when
# FAKE_IMPLIED is 1, as for the macOS groups everyone and localaccounts.
mkdir "$work/dsm"
cat > "$work/dsm/dsmemberutil" <<'EOF'
#!/bin/sh
[ "$1 $2 $3 $4" = "checkmembership -U nobody -g" ] || exit 2
if [ "${FAKE_IMPLIED:-0}" = 1 ]; then
  echo "user is a member of the group"
else
  echo "user is not a member of the group"
fi
EOF
chmod +x "$work/dsm/dsmemberutil"
me=$(id -un)
mygid=$(id -g)

# A stub gh. `gh release view --repo wallter/a2p2 ...` prints $GH_LATEST,
# the tag of the latest release. `gh attestation verify FILE ...` passes
# only when FILE has the bytes of $GH_GOOD, as if only that file had an
# attestation, made from the ref $GH_REF: with --source-ref it must name
# that ref, as the real gh checks. It records its arguments in $GH_ARGS.
mkdir "$work/ghstub"
cat > "$work/ghstub/gh" <<'EOF2'
#!/bin/sh
printf '%s\n' "$*" > "$GH_ARGS"
if [ "$1 $2" = "release view" ]; then
  case "$*" in *"--repo wallter/a2p2 "*) echo "$GH_LATEST"; exit 0 ;; esac
  exit 2
fi
[ "$1 $2" = "attestation verify" ] || exit 2
cmp -s "$3" "$GH_GOOD" || { echo "no attestation found for $3" >&2; exit 1; }
prev=""
for a; do
  if [ "$prev" = --source-ref ] && [ "$a" != "$GH_REF" ]; then
    echo "the attestation of $3 is from $GH_REF, not $a" >&2; exit 1
  fi
  prev=$a
done
exit 0
EOF2
chmod +x "$work/ghstub/gh"
export GH_ARGS="$work/gh.args"

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
# v0.3.0 is the latest release with an attestation (gh names it), and
# v0.3.1 holds the same attested bytes, made from v0.3.0, under a newer
# tag: the downgrade someone who can edit releases could make.
for t in v0.3.0 v0.3.1; do
  mkdir -p "$rel/download/$t"
  cp "$work/b-latest" "$rel/download/$t/a2p2_linux_amd64"
  printf '%s  a2p2_linux_amd64\n' "$h_latest" > "$rel/download/$t/SHA256SUMS"
done
export GH_LATEST=v0.3.0 GH_REF=refs/tags/v0.3.0

# A mkdir that makes the directory, then lets every user write it, as if
# another user changed it between its creation and the install.
real_mkdir=$(command -v mkdir)
mkdir "$work/mkstub"
printf '#!/bin/sh\n"%s" "$@" || exit\nfor last; do :; done\nchmod 0777 "$last"\n' "$real_mkdir" > "$work/mkstub/mkdir"
chmod +x "$work/mkstub/mkdir"

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

  cur="install directory another user can change"
  fresh; mkdir -p "$dir"; chmod 0777 "$dir"
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "every user may write it: nonzero exit" $?
  grep -q "can be written by every user" "$work/out"; check "every user may write it: says so" $?
  empty "$dir"; check "every user may write it: nothing installed" $?
  fresh; mkdir -p "$work/$sh_.$n/open"; chmod 0777 "$work/$sh_.$n/open"; dir=$work/$sh_.$n/open/sub/bin
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "below a directory every user may write: nonzero exit" $?
  [ ! -e "$work/$sh_.$n/open/sub" ]; check "below a directory every user may write: nothing made" $?
  fresh; mkdir -p "$dir"; chmod 1777 "$dir"
  run "$sh_" "$dir" - -
  check "a sticky directory every user may write: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "a sticky directory every user may write: installed" $?
  chmod 0755 "$work/$sh_.$n/open" 2>/dev/null || true
  # A link of this user's into a directory below one every user may write:
  # the real parents of the link's target are checked too.
  fresh; t=$work/$sh_.$n
  mkdir -p "$t/ww/sub/bin" "$t/home"; chmod 0777 "$t/ww"; ln -s "$t/ww/sub" "$t/home/link"
  dir=$t/home/link/bin
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "a link into a directory every user may write: nonzero exit" $?
  grep -q "/ww can be written by every user" "$work/out"; check "a link into a directory every user may write: names the real directory" $?
  empty "$t/ww/sub/bin"; check "a link into a directory every user may write: nothing installed" $?
  dir=$t/home/link/new/bin
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "a missing directory below such a link: nonzero exit" $?
  [ ! -e "$t/ww/sub/new" ]; check "a missing directory below such a link: nothing made" $?
  chmod 0755 "$t/ww"
  # A link to a link that sits below a directory every user may write: the
  # real directory at the end is safe, the second link can be swapped.
  fresh; t=$work/$sh_.$n
  mkdir -p "$t/ww/mid" "$t/real/bin" "$t/home"; chmod 0777 "$t/ww"
  ln -s "$t/real" "$t/ww/mid/l2"; ln -s "$t/ww/mid/l2" "$t/home/l1"
  dir=$t/home/l1/bin
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ] && grep -q "/ww can be written by every user" "$work/out"; check "a link to a link below a directory every user may write: refused" $?
  empty "$t/real/bin"; check "a link to a link below a directory every user may write: nothing installed" $?
  chmod 0755 "$t/ww"
  # The same paths written with a trailing slash: "link/" names the
  # directory the link leads to, so the link and the real parents must
  # still be checked.
  fresh; t=$work/$sh_.$n
  mkdir -p "$t/ww/sub" "$t/home" "$t/mid"; chmod 0777 "$t/ww"
  ln -s "$t/ww/sub" "$t/home/link"
  run "$sh_" "$t/home/link/" - -
  [ "$rc" -ne 0 ] && grep -q "/ww can be written by every user" "$work/out"; check "trailing slash on a link into a directory every user may write: refused" $?
  empty "$t/ww/sub"; check "trailing slash on a link into a directory every user may write: nothing installed" $?
  run "$sh_" "$t/home/link//" - -
  [ "$rc" -ne 0 ] && grep -q "/ww can be written by every user" "$work/out"; check "two trailing slashes on such a link: refused" $?
  # A link in a parent whose target ends in a slash and is itself a link.
  ln -s "$t/ww/sub/" "$t/mid/l2"; ln -s "$t/mid/l2/" "$t/home/l1"; mkdir "$t/ww/sub/bin"
  run "$sh_" "$t/home/l1/bin" - -
  [ "$rc" -ne 0 ] && grep -q "/ww can be written by every user" "$work/out"; check "a link in a parent to a link, both with a trailing slash: refused" $?
  empty "$t/ww/sub/bin"; check "a link in a parent to a link, both with a trailing slash: nothing installed" $?
  chmod 0755 "$t/ww"
  # The honest cases with a trailing slash install.
  fresh; d0=$dir; mkdir -p "$d0"
  run "$sh_" "$d0/" - -
  check "an install directory with a trailing slash: exit 0" "$rc"
  [ -x "$d0/a2p2" ]; check "an install directory with a trailing slash: installed" $?
  grep -q "^installed $d0/a2p2\$" "$work/out"; check "an install directory with a trailing slash: named without it" $?
  fresh; t=$work/$sh_.$n
  mkdir -p "$t/real" "$t/home"; ln -s "$t/real/" "$t/home/ok"
  run "$sh_" "$t/home/ok/" - -
  check "a link to a directory only you can change, with trailing slashes: exit 0" "$rc"
  [ -x "$t/real/a2p2" ]; check "a link to a directory only you can change, with trailing slashes: installed there" $?
  # A home directory that is a symbolic link, and the default directory
  # under it ($HOME/.local/bin, made by install.sh).
  fresh; t=$work/$sh_.$n
  mkdir -p "$t/realhome"; ln -s "$t/realhome" "$t/homelink"
  old_home=$HOME; HOME=$t/homelink
  if [ "$HOME" = "$t/homelink" ]; then
    run "$sh_" "" - -
    check "a symlinked home, the default directory: exit 0" "$rc"
    [ -x "$t/realhome/.local/bin/a2p2" ]; check "a symlinked home, the default directory: installed" $?
  else
    check "a symlinked home: HOME could not be set" 1
  fi
  HOME=$old_home

  # A directory a group may write (mode bits). Refused when the group has
  # a member other than this user and root; installed, with a note when
  # the group is not this user's own group of the same name, when it has
  # none; installed with a note when its members cannot be listed.
  # getent and the macOS tools are stubs, so the group's members are known.
  ge="$work/getent:$work/stub:$PATH"
  fresh; mkdir -p "$dir"; chmod 0775 "$dir"
  export FAKE_GROUP=devs FAKE_MEMBERS="$me,mallory" FAKE_PASSWD=""
  run "$sh_" "$dir" - "$ge"
  [ "$rc" -ne 0 ] && grep -q "can be written by the group devs, which has other members (mallory)" "$work/out"; check "a group with another member may write it: refused" $?
  empty "$dir"; check "a group with another member may write it: nothing installed" $?
  export FAKE_MEMBERS="" FAKE_PASSWD="eve:x:1234:$(ls -ldn "$dir" | awk '{print $4}'):eve:/home/eve:/bin/sh"
  run "$sh_" "$dir" - "$ge"
  [ "$rc" -ne 0 ] && grep -q "which has other members (eve)" "$work/out"; check "a group that is another user's primary group may write it: refused" $?
  empty "$dir"; check "a group that is another user's primary group may write it: nothing installed" $?
  export FAKE_GROUP=admin FAKE_MEMBERS="root,$me" FAKE_PASSWD="$me:x:1:$mygid:me:/h:/bin/sh"
  run "$sh_" "$dir" - "$ge"
  check "a group of only you and root may write it: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "a group of only you and root may write it: installed" $?
  [ "$(grep -c "note: $dir can be written by the group admin; the system's user lists show only you and root in it" "$work/out")" = 1 ]; check "a group of only you and root may write it: one note" $?
  # This user's own group of the same name (on Linux, the usual private
  # group; umask 002 makes ~/.local/bin 0775): installed without a note.
  fresh; mkdir -p "$dir"; chmod 0775 "$dir"
  if [ "$(ls -ldn "$dir" | awk '{print $4}')" = "$mygid" ]; then
    export FAKE_GROUP=$me FAKE_MEMBERS="" FAKE_PASSWD="$me:x:1:$mygid:me:/h:/bin/sh"
    run "$sh_" "$dir" - "$ge"
    check "your own group of your name may write it: exit 0" "$rc"
    ! grep -q "install.sh: note:" "$work/out"; check "your own group of your name may write it: no note" $?
  fi
  # A group-writable directory above it, as a Homebrew prefix on macOS
  # (group admin may write it).
  fresh; t=$work/$sh_.$n
  mkdir -p "$t/prefix/bin"; chmod 0775 "$t/prefix" "$t/prefix/bin"
  export FAKE_GROUP=admin FAKE_MEMBERS="root,$me" FAKE_PASSWD=""
  run "$sh_" "$t/prefix/bin" - "$ge"
  check "a prefix that a group of only you and root may write: exit 0" "$rc"
  [ -x "$t/prefix/bin/a2p2" ]; check "a prefix that a group of only you and root may write: installed" $?
  rm -f "$t/prefix/bin/a2p2"; chmod 0755 "$t/prefix/bin"
  export FAKE_MEMBERS="root,$me,other"
  run "$sh_" "$t/prefix/bin" - "$ge"
  [ "$rc" -ne 0 ] && grep -q "/prefix can be written by the group admin, which has other members (other)" "$work/out"; check "a prefix that a group with another member may write: refused" $?
  # The macOS route: dscacheutil and dscl, no getent.
  fresh; mkdir -p "$dir"; chmod 0775 "$dir"
  export FAKE_GROUP=staff FAKE_MEMBERS="root" FAKE_PASSWD="$me $mygid
eve $(ls -ldn "$dir" | awk '{print $4}')"
  run "$sh_" "$dir" - "$work/dsc:$work/stub:$work/nogh"
  [ "$rc" -ne 0 ] && grep -q "which has other members (eve)" "$work/out"; check "dscl lists another user in the group: refused" $?
  export FAKE_PASSWD="$me $mygid"
  run "$sh_" "$dir" - "$work/dsc:$work/stub:$work/nogh"
  check "dscl lists only you in the group: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "dscl lists only you in the group: installed" $?
  # A group whose members macOS implies rather than lists (everyone,
  # localaccounts): the lists show only you, but dsmemberutil says the
  # account nobody is in it. Refused, without the "only you and root" note.
  fresh; mkdir -p "$dir"; chmod 0775 "$dir"
  export FAKE_GROUP=everyone FAKE_MEMBERS="" FAKE_PASSWD="$me $mygid" FAKE_IMPLIED=1
  run "$sh_" "$dir" - "$work/dsm:$work/dsc:$work/stub:$work/nogh"
  [ "$rc" -ne 0 ] && grep -q "can be written by the group everyone, which holds accounts the system's lists do not name (the account nobody is a member)" "$work/out"; check "a group of implied members may write it: refused" $?
  empty "$dir"; check "a group of implied members may write it: nothing installed" $?
  ! grep -q "only you and root" "$work/out"; check "a group of implied members may write it: no only-you note" $?
  export FAKE_IMPLIED=0
  run "$sh_" "$dir" - "$work/dsm:$work/dsc:$work/stub:$work/nogh"
  check "nobody is not in the group: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "nobody is not in the group: installed" $?
  unset FAKE_GROUP FAKE_MEMBERS FAKE_PASSWD FAKE_IMPLIED
  # The real macOS groups everyone (12) and localaccounts (61), which any
  # user may chgrp to: refused with the system's own tools, and refused by
  # gid when dsmemberutil does not report nobody as a member.
  if command -v dsmemberutil >/dev/null 2>&1; then
    for g in everyone localaccounts; do
      fresh; mkdir -p "$dir"
      chgrp "$g" "$dir" 2>/dev/null || continue
      chmod 0775 "$dir"
      run "$sh_" "$dir" - -
      [ "$rc" -ne 0 ] && grep -q "can be written by the group $g, which holds accounts" "$work/out"; check "the macOS group $g may write it: refused" $?
      empty "$dir"; check "the macOS group $g may write it: nothing installed" $?
      ! grep -q "only you and root" "$work/out"; check "the macOS group $g may write it: no only-you note" $?
      export FAKE_IMPLIED=0
      run "$sh_" "$dir" - "$work/dsm:$work/stub:$PATH"
      [ "$rc" -ne 0 ] && grep -q "can be written by the group $g, which holds accounts the system's lists do not name (every local or network account is in it)" "$work/out"; check "the macOS group $g by its gid: refused" $?
      unset FAKE_IMPLIED
    done
  fi
  # No tool lists the group: a note, and the install goes on.
  fresh; mkdir -p "$dir"; chmod 0775 "$dir"
  run "$sh_" "$dir" - "$work/stub:$work/nogh"
  check "a group whose members cannot be listed: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "a group whose members cannot be listed: installed" $?
  if [ "$me" != "$(id -gn)" ]; then
    grep -q "cannot list who is in it" "$work/out"; check "a group whose members cannot be listed: says so" $?
  fi
  # With the system's own lists: installed, or refused naming other members.
  fresh; mkdir -p "$dir"; chmod 0775 "$dir"
  run "$sh_" "$dir" - -
  [ "$rc" -eq 0 ] || grep -q "which has other members" "$work/out"; check "a group-writable directory, the system's lists: installed or refused for a named reason" $?
  # The directories install.sh makes: mode 0755 whatever the umask, and
  # checked again after they are made.
  fresh
  old_umask=$(umask); umask 000
  run "$sh_" "$dir" - -
  umask "$old_umask"
  check "made under umask 000: exit 0" "$rc"
  [ -z "$(find "$work/$sh_.$n" -perm -0002)" ]; check "made under umask 000: no directory every user may write" $?
  fresh
  run "$sh_" "$dir" - "$work/mkstub:$work/stub:$PATH"
  [ "$rc" -ne 0 ]; check "a made directory changed before the install: nonzero exit" $?
  grep -q "can be written by every user" "$work/out"; check "a made directory changed before the install: says so" $?
  [ ! -e "$dir/a2p2" ]; check "a made directory changed before the install: nothing installed" $?
  chmod 0755 "$work/$sh_.$n/bin" 2>/dev/null || true

  cur="attestation"
  export GH_GOOD="$work/b-latest"
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - -
  check "not asked for: exit 0" "$rc"
  grep -q "not who built it" "$work/out"; check "not asked for: says the checksum does not show who built it" $?
  grep -q "A2P2_VERIFY_ATTESTATION=1" "$work/out"; check "not asked for: names the setting" $?
  export A2P2_VERIFY_ATTESTATION=1
  fresh; mkdir "$dir"; rm -f "$GH_ARGS"
  run "$sh_" "$dir" - "$work/ghstub:$work/stub:$PATH"
  check "attested: exit 0" "$rc"
  [ -x "$dir/a2p2" ]; check "attested: installed" $?
  grep -q "attestation ok" "$work/out"; check "attested: says so" $?
  grep -q "downloading a2p2_linux_amd64 (v0.3.0)" "$work/out"; check "attested: latest is resolved once, to the tag gh names" $?
  grep -q -- "--repo wallter/a2p2 --signer-workflow wallter/a2p2/.github/workflows/release.yml --source-ref refs/tags/v0.3.0 --deny-self-hosted-runners" "$GH_ARGS"; check "attested: pinned to the repository, release.yml, the tag and a GitHub runner" $?
  # An attested build of an older tag put in place under a newer one, with
  # its SHA256SUMS line: the checksum and the workflow match, the tag not.
  export A2P2_VERSION=v0.3.1
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - "$work/ghstub:$work/stub:$PATH"
  unset A2P2_VERSION
  [ "$rc" -ne 0 ]; check "an attested build of another tag: nonzero exit" $?
  grep -q "no valid attestation" "$work/out"; check "an attested build of another tag: says so" $?
  empty "$dir"; check "an attested build of another tag: nothing installed" $?
  # gh names a latest release from before attestations.
  export GH_LATEST=v0.2.3
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - "$work/ghstub:$work/stub:$PATH"
  export GH_LATEST=v0.3.0
  [ "$rc" -ne 0 ] && grep -q "has no attestation" "$work/out"; check "a latest release before attestations: refused" $?
  empty "$dir"; check "a latest release before attestations: nothing installed" $?
  # A release whose binary and SHA256SUMS were both replaced: the checksum
  # matches, only the attestation catches it.
  rm -rf "$work/swap"; mkdir -p "$work/swap/latest/download"
  printf '#!/bin/sh\necho "a2p2 swapped"\n' > "$work/swap/latest/download/a2p2_linux_amd64"
  printf '%s  a2p2_linux_amd64\n' "$(sum "$work/swap/latest/download/a2p2_linux_amd64")" > "$work/swap/latest/download/SHA256SUMS"
  mkdir -p "$work/swap/download"; cp -R "$work/swap/latest/download" "$work/swap/download/v0.3.0"
  fresh; mkdir "$dir"
  run "$sh_" "$dir" "file://$work/swap" "$work/ghstub:$work/stub:$PATH"
  [ "$rc" -ne 0 ]; check "swapped binary and SHA256SUMS: nonzero exit" $?
  grep -q "checksum ok" "$work/out"; check "swapped binary and SHA256SUMS: the checksum alone passes" $?
  grep -q "no valid attestation" "$work/out"; check "swapped binary and SHA256SUMS: says so" $?
  empty "$dir"; check "swapped binary and SHA256SUMS: nothing installed" $?
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - "$work/stub:$work/nogh"
  [ "$rc" -ne 0 ]; check "no gh: nonzero exit" $?
  grep -q "needs the GitHub CLI" "$work/out"; check "no gh: says so" $?
  empty "$dir"; check "no gh: nothing installed" $?
  export A2P2_VERSION=v0.2.1
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - "$work/ghstub:$work/stub:$PATH"
  unset A2P2_VERSION
  [ "$rc" -ne 0 ]; check "a release before attestations: nonzero exit" $?
  grep -q "has no attestation" "$work/out"; check "a release before attestations: says so" $?
  empty "$dir"; check "a release before attestations: nothing installed" $?
  export A2P2_VERIFY_ATTESTATION=yes
  fresh; mkdir "$dir"
  run "$sh_" "$dir" - -
  [ "$rc" -ne 0 ]; check "a bad setting: nonzero exit" $?
  empty "$dir"; check "a bad setting: nothing installed" $?
  unset A2P2_VERIFY_ATTESTATION
  # Without the setting, a swapped release installs: the documented limit.
  fresh; mkdir "$dir"
  run "$sh_" "$dir" "file://$work/swap" -
  check "swapped release without the setting: installs (the documented limit)" "$rc"
  rm -rf "$work/swap"

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
