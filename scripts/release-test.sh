#!/bin/sh
# Test the release checks offline (make release-test, part of make check):
# - scripts/workflows.sh passes on .github/workflows and refuses a tag-pinned
#   action, a checkout that keeps its token, a missing top-level
#   permissions block and a write job that checks out code, and syntax it
#   does not parse;
# - scripts/verify-release.sh, run against a stub gh that serves a fake
#   release from a temporary directory, passes on an honest release and
#   fails when a binary for another system was swapped together with its
#   SHA256SUMS line, or replaced by an attested build of another tag, when
#   SHA256SUMS has an extra, missing, repeated or blank line, or when the
#   release has another asset;
# - the publish job's file check in .github/workflows/release.yml, taken
#   out of the file and run on fake dist/ directories.
# Usage: scripts/release-test.sh
set -eu
cd "$(dirname "$0")/.."
repo=$(pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-release-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
fail=0
ok() { if [ "$2" -eq 0 ]; then :; else echo "FAIL: $1"; fail=1; fi; }
set +e # assertions are collected by ok

# --- workflows.sh -------------------------------------------------------
scripts/workflows.sh > "$work/out" 2>&1; ok "the workflows in the tree pass" $?

sha=11d5960a326750d5838078e36cf38b85af677262
good() { # a workflow in the shape these files use
  cat <<EOF
name: t
on: push
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$sha # v4.4.0
        with:
          persist-credentials: false
      - run: make
  publish:
    needs: build
    permissions:
      contents: write
    steps:
      - uses: actions/download-artifact@$sha # v8.0.2
      - run: gh release create
EOF
}
wf() { # wf NAME: refused unless workflows.sh rejects $work/NAME.yml
  rc=0; scripts/workflows.sh "$work/$1.yml" > "$work/out" 2>&1 || rc=$?
}
good > "$work/good.yml"
wf good; ok "a pinned workflow passes" "$rc"

good | sed "s|actions/checkout@$sha # v4.4.0|actions/checkout@v4|" > "$work/tag.yml"
wf tag; [ "$rc" -ne 0 ] && grep -q "not pinned" "$work/out"; ok "an action named by tag is refused" $?

good | sed "s|actions/checkout@$sha # v4.4.0|actions/checkout@$sha|" > "$work/nocomment.yml"
wf nocomment; [ "$rc" -ne 0 ]; ok "a pin without its tag in a comment is refused" $?

good | sed "s|actions/checkout@$sha # v4.4.0|actions/checkout@11d5960 # v4.4.0|" > "$work/short.yml"
wf short; [ "$rc" -ne 0 ]; ok "a short SHA is refused" $?

good | sed "s|actions/download-artifact@$sha # v8.0.2|actions/download-artifact@main # v8.0.2|" > "$work/branch.yml"
wf branch; [ "$rc" -ne 0 ]; ok "an action named by branch is refused" $?

good | sed '/persist-credentials/d' > "$work/persist.yml"
wf persist; [ "$rc" -ne 0 ] && grep -q "persist-credentials" "$work/out"; ok "a checkout that keeps its token is refused" $?

good | sed '/^permissions:/d; /^  contents: read/d' > "$work/noperm.yml"
wf noperm; [ "$rc" -ne 0 ] && grep -q "no top-level permissions" "$work/out"; ok "a workflow without top-level permissions is refused" $?

good | sed 's/^permissions:$/permissions: write-all/; /^  contents: read/d' > "$work/writeall.yml"
wf writeall; [ "$rc" -ne 0 ]; ok "top-level write-all is refused" $?

good | sed 's/^  contents: read/  contents: write/' > "$work/topwrite.yml"
wf topwrite; [ "$rc" -ne 0 ]; ok "a top-level write scope is refused" $?

good | sed "s|      - uses: actions/download-artifact@$sha # v8.0.2|      - uses: actions/checkout@$sha # v4.4.0\\
        with:\\
          persist-credentials: false|" > "$work/writecode.yml"
wf writecode; [ "$rc" -ne 0 ] && grep -q "holds a write scope and checks out code" "$work/out"; ok "a write job that checks out code is refused" $?

good | sed "s|      - uses: actions/download-artifact@$sha # v8.0.2|      - uses: actions/setup-go@$sha # v5.6.0|" > "$work/writego.yml"
wf writego; [ "$rc" -ne 0 ] && grep -q "sets up Go" "$work/out"; ok "a write job that sets up Go is refused" $?

# A quoted write scope in the job that checks out code.
good | sed "s|^    runs-on: ubuntu-latest|    runs-on: ubuntu-latest\\
    permissions:\\
      contents: 'write'|" > "$work/quoted.yml"
wf quoted; [ "$rc" -ne 0 ] && grep -q "holds a write scope and checks out code" "$work/out"; ok "a quoted write scope is seen" $?

good | sed 's/^  contents: read/  "contents": write/' > "$work/qkey.yml"
wf qkey; [ "$rc" -ne 0 ] && grep -q "quoted key" "$work/out"; ok "a quoted key is refused" $?

good | sed "s|      - uses: actions/checkout@$sha # v4.4.0|      - {uses: actions/checkout@v4}|; /persist-credentials/d; /^        with:/d" > "$work/flow.yml"
wf flow; [ "$rc" -ne 0 ] && grep -q "flow mapping" "$work/out"; ok "a uses in a flow mapping is refused" $?

good | sed 's/^    permissions:$/    permissions: *w/' > "$work/alias.yml"
wf alias; [ "$rc" -ne 0 ] && grep -q "aliases" "$work/out"; ok "an alias is refused" $?

good | sed 's/^  build:/    build:/' > "$work/indent.yml"
wf indent; [ "$rc" -ne 0 ] && grep -q "job name at two spaces" "$work/out"; ok "a job not at two spaces is refused" $?

# The text of a run block is not structure: uses: and { in it are fine.
good | sed "s|      - run: make|      - run: \\|\\
          echo 'uses: actions/checkout@v4' { }\\
          make|" > "$work/block.yml"
wf block; ok "the text of a run block is not read as structure" "$rc"

# A .yaml file in a directory is read too.
mkdir "$work/wfdir"
good > "$work/wfdir/a.yml"
good | sed "s|actions/checkout@$sha # v4.4.0|actions/checkout@v4|" > "$work/wfdir/b.yaml"
rc=0; scripts/workflows.sh "$work/wfdir" > "$work/out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] && grep -q "b.yaml:.*not pinned" "$work/out"; ok "a .yaml workflow is checked" $?

# --- verify-release.sh ----------------------------------------------------
v=v9.9.9
all="a2p2_darwin_amd64 a2p2_darwin_arm64 a2p2_linux_amd64 a2p2_linux_arm64 a2p2_windows_amd64.exe a2p2_windows_arm64.exe"
sum() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum < "$1" | awk '{print $1}'
  else shasum -a 256 < "$1" | awk '{print $1}'; fi
}
# release REL: an honest fake release in $work/REL: every file is a script
# that prints the tag, SHA256SUMS has a line for each, every file is
# "attested" from the tag (a line `<hash> refs/tags/<tag>` in
# REL.attested), and the notes hold the install block.
release() {
  d=$work/$1; rm -rf "$d"; mkdir -p "$d"; : > "$work/$1.attested"
  for b in $all; do
    printf '#!/bin/sh\necho "a2p2 %s"\n# %s\n' "$v" "$b" > "$d/$b"
    printf '%s refs/tags/%s\n' "$(sum "$d/$b")" "$v" >> "$work/$1.attested"
  done
  (cd "$d" && for b in $all; do printf '%s  %s\n' "$(sum "$b")" "$b"; done) > "$d/SHA256SUMS"
  cp scripts/release-notes-head.md "$work/$1.notes"
}
mkdir "$work/ghbin"
cat > "$work/ghbin/gh" <<'EOF'
#!/bin/sh
# A stub gh serving the fake release in $REL.
case "$1 $2" in
  "run list") echo 42 ;;
  "run watch") ;;
  "repo view") echo owner/a2p2 ;;
  "release view")
    case "$*" in
      *"--json assets"*) ls "$REL" ;;
      *"--json body"*) cat "$REL.notes" ;;
      *) exit 2 ;;
    esac ;;
  "release download")
    out=""; pat=""; prev=""
    for a; do [ "$prev" = -D ] && out=$a; [ "$prev" = -p ] && pat=$a; prev=$a; done
    [ -n "$out" ] || exit 2
    if [ -n "$pat" ]; then cp "$REL/$pat" "$out"/; else cp "$REL"/* "$out"/; fi ;;
  "attestation verify")
    case "$*" in *"--repo owner/a2p2 --signer-workflow owner/a2p2/.github/workflows/release.yml"*) ;; *) exit 3 ;; esac
    if command -v sha256sum >/dev/null 2>&1; then h=$(sha256sum < "$3"); else h=$(shasum -a 256 < "$3"); fi
    # Like the real gh: without --source-ref any ref is accepted.
    ref=""; prev=""
    for a; do [ "$prev" = --source-ref ] && ref=$a; prev=$a; done
    if [ -n "$ref" ]; then
      grep -qx "${h%% *} $ref" "$REL.attested" || { echo "no attestation for $3 from $ref" >&2; exit 1; }
    else
      grep -q "^${h%% *} " "$REL.attested" || { echo "no attestation for $3" >&2; exit 1; }
    fi ;;
  *) echo "stub gh: unexpected: $*" >&2; exit 2 ;;
esac
EOF
chmod +x "$work/ghbin/gh"
vr() { # vr REL: run verify-release.sh against $work/REL
  rc=0
  env PATH="$work/ghbin:$PATH" REL="$work/$1" WAIT=0 TMPDIR="$work" \
    "$repo/scripts/verify-release.sh" "$v" > "$work/out" 2>&1 || rc=$?
}

release honest
vr honest; ok "an honest release passes" "$rc"
[ "$(grep -c 'matches SHA256SUMS and its attestation' "$work/out")" = 6 ]; ok "all six binaries are checked" $?

# A binary for a system other than this one is swapped, and its SHA256SUMS
# line with it. Only its missing attestation shows it.
for other in a2p2_windows_arm64.exe a2p2_linux_arm64; do
  case "$(uname -s)/$(uname -m)" in
    Linux/aarch64 | Linux/arm64) [ "$other" != a2p2_linux_arm64 ] || continue ;;
  esac
  release swap
  printf '#!/bin/sh\necho "a2p2 %s"\n# swapped\n' "$v" > "$work/swap/$other"
  (cd "$work/swap" && for b in $all; do printf '%s  %s\n' "$(sum "$b")" "$b"; done) > "$work/swap/SHA256SUMS"
  vr swap
  [ "$rc" -ne 0 ] && grep -q "$other has no valid attestation" "$work/out"; ok "a swapped $other with a matching SHA256SUMS line is refused" $?
done

release extra
printf '%s  a2p2_freebsd_amd64\n' "$(sum "$work/extra/a2p2_linux_amd64")" >> "$work/extra/SHA256SUMS"
vr extra; [ "$rc" -ne 0 ]; ok "an extra SHA256SUMS line is refused" $?

release missing
grep -v a2p2_darwin_amd64 "$work/missing/SHA256SUMS" > "$work/missing.s"; mv "$work/missing.s" "$work/missing/SHA256SUMS"
vr missing; [ "$rc" -ne 0 ]; ok "a missing SHA256SUMS line is refused" $?

release wrong
printf 'x\n' >> "$work/wrong/a2p2_darwin_arm64"
vr wrong; [ "$rc" -ne 0 ] && grep -q "checksum of a2p2_darwin_arm64 does not match" "$work/out"; ok "a binary that does not match its line is refused" $?

# A binary for another system replaced by an attested build of an older
# tag, with its SHA256SUMS line: the workflow matches, the tag does not.
release oldtag
printf '#!/bin/sh\necho "a2p2 v9.9.8"\n# old\n' > "$work/oldtag/a2p2_windows_arm64.exe"
printf '%s refs/tags/v9.9.8\n' "$(sum "$work/oldtag/a2p2_windows_arm64.exe")" >> "$work/oldtag.attested"
(cd "$work/oldtag" && for b in $all; do printf '%s  %s\n' "$(sum "$b")" "$b"; done) > "$work/oldtag/SHA256SUMS"
vr oldtag; [ "$rc" -ne 0 ] && grep -q "a2p2_windows_arm64.exe has no valid attestation" "$work/out"; ok "an attested build of another tag is refused" $?

release blank
printf '\n' >> "$work/blank/SHA256SUMS"
vr blank; [ "$rc" -ne 0 ]; ok "a blank SHA256SUMS line is refused" $?

# Six lines, one of them repeated in place of another binary's.
release dup
grep -v a2p2_darwin_amd64 "$work/dup/SHA256SUMS" > "$work/dup.s"; grep a2p2_linux_amd64 "$work/dup.s" >> "$work/dup.s"; mv "$work/dup.s" "$work/dup/SHA256SUMS"
vr dup; [ "$rc" -ne 0 ] && grep -q "exactly one line for each" "$work/out"; ok "a repeated SHA256SUMS line in place of another is refused" $?

release more
printf 'x\n' > "$work/more/a2p2_freebsd_amd64"
vr more; [ "$rc" -ne 0 ]; ok "a release with another asset is refused" $?

# --- the publish job's file check in release.yml --------------------------
# The run block of the step that checks dist/, as bash runs it there.
awk '
  /name: the files are the six binaries/ { found = 1; next }
  found && /run: \|/ { inrun = 1; match($0, /^ */); base = RLENGTH; next }
  inrun { match($0, /^ */); if ($0 !~ /^ *$/ && RLENGTH <= base) exit; print substr($0, base + 3) }
' .github/workflows/release.yml > "$work/distcheck.sh"
grep -q 'sha256sum --strict -c SHA256SUMS' "$work/distcheck.sh"; ok "the dist/ check is found in release.yml" $?
dist() { # dist NAME: a dist/ as the build job leaves it, in $work/NAME
  d=$work/$1; rm -rf "$d"; mkdir -p "$d"
  for b in $all; do printf 'binary %s\n' "$b" > "$d/$b"; done
  (cd "$d" && for b in $all; do printf '%s  %s\n' "$(sum "$b")" "$b"; done) > "$d/SHA256SUMS"
  echo notes > "$d/notes.md"
}
dc() { # dc NAME: run the check in $work/NAME
  rc=0; (cd "$work/$1" && bash -eo pipefail "$work/distcheck.sh") > "$work/out" 2>&1 || rc=$?
}
dist dgood; dc dgood; ok "the dist/ check passes the files the build makes" "$rc"
dist ddup
grep -v a2p2_darwin_amd64 "$work/ddup/SHA256SUMS" > "$work/ddup.s"; grep a2p2_linux_amd64 "$work/ddup.s" >> "$work/ddup.s"; mv "$work/ddup.s" "$work/ddup/SHA256SUMS"
dc ddup; [ "$rc" -ne 0 ]; ok "the dist/ check refuses a repeated line that leaves a binary unchecked" $?
dist dblank; printf '\n' >> "$work/dblank/SHA256SUMS"
dc dblank; [ "$rc" -ne 0 ]; ok "the dist/ check refuses a blank line" $?
dist dmore; echo x > "$work/dmore/a2p2_freebsd_amd64"
dc dmore; [ "$rc" -ne 0 ]; ok "the dist/ check refuses another file" $?
dist dswap; echo x >> "$work/dswap/a2p2_linux_arm64"
dc dswap; [ "$rc" -ne 0 ]; ok "the dist/ check refuses a binary that does not match" $?

if [ "$fail" -ne 0 ]; then echo "release-test: FAILED"; exit 1; fi
echo "release-test: ok"
