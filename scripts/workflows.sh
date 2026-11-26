#!/bin/sh
# Check the GitHub Actions workflows (make check runs it; DECISIONS,
# D61).
#
#   scripts/workflows.sh [FILE|DIR ...]
#
# With no argument it reads .github/workflows/*.yml and *.yaml; a DIR
# stands for the *.yml and *.yaml files in it. Fails, naming the file and
# line, when:
# - a `uses:` names an action by tag or branch, not by a full 40-hex commit
#   SHA followed by a comment with its release tag (`@<sha> # v1.2.3`);
# - an actions/checkout step does not set `persist-credentials: false`;
# - a file has no top-level `permissions:`, or grants a write scope there
#   (every job would get it);
# - a job that holds any write scope (`write` or `write-all`, quoted or
#   not) checks out code or sets up Go, so the steps that run repository
#   code and the toolchain never hold one;
# - the file uses YAML this line-based check does not follow: a tab in the
#   indentation, a quoted key, a flow mapping (`{...}`; `${{ }}` is fine),
#   an anchor, alias or merge key, a `uses:` anywhere but at the start of
#   a key, or a job that is not at two spaces under `jobs:`.
# The text of a block scalar (`run: |`) is skipped: it is a string, not
# structure. Otherwise the parse expects the layout these files use: two
# spaces per level, jobs at two spaces, steps as `- ` items.
set -eu
cd "$(dirname "$0")/.."
[ "$#" -gt 0 ] || set -- .github/workflows
files=""
n=0
for a; do
  if [ -d "$a" ]; then
    for f in "$a"/*.yml "$a"/*.yaml; do
      [ -e "$f" ] || continue
      files="$files$f
"
      n=$((n + 1))
    done
  else
    [ -e "$a" ] || { echo "workflows: $a: no such file"; echo "workflows: FAILED"; exit 1; }
    files="$files$a
"
    n=$((n + 1))
  fi
done
[ "$n" -gt 0 ] || { echo "workflows: no workflow files in $*"; echo "workflows: FAILED"; exit 1; }
# One file name per line; names with a newline are not expected here.
old_ifs=$IFS
IFS='
'
# shellcheck disable=SC2086
set -- $files
IFS=$old_ifs
awk '
function fail(msg) { printf "workflows: %s:%d: %s\n", FILENAME, FNR, msg; bad = 1 }
function indent(s) { match(s, /^ */); return RLENGTH }
function endjob() {
  if (job != "" && jobwrite && jobcode)
    printf "workflows: %s: job %s holds a write scope and %s; split it so the write job runs no repository code\n", file, job, jobcode
  if (job != "" && jobwrite && jobcode) bad = 1
  job = ""; jobwrite = 0; jobcode = ""
}
function endcheckout() {
  if (co && !copersist) {
    printf "workflows: %s:%d: actions/checkout without persist-credentials: false\n", file, coline
    bad = 1
  }
  co = 0
}
function endfile() {
  if (file == "") return
  endcheckout(); endjob()
  if (!toplevelperm) { printf "workflows: %s: no top-level permissions:\n", file; bad = 1 }
}
FNR == 1 {
  endfile()
  file = FILENAME; injobs = 0; toplevelperm = 0; intop = 0; job = ""; co = 0; blk = -1
}
# The text of a block scalar: every line indented more than its key.
blk >= 0 {
  if ($0 ~ /^[ ]*$/ || indent($0) > blk) next
  blk = -1
}
/^[ ]*#/ || /^[ ]*$/ { next }
/^ *\t/ { fail("a tab in the indentation is not supported"); next }
{
  ind = indent($0)
  line = $0
  sub(/^ */, "", line)
  # The structure of the line without a trailing comment or ${{ }}.
  bare = line
  sub(/[ ]+#.*$/, "", bare)
  gsub(/\$\{\{[^}]*\}\}/, "", bare)
  if (bare ~ /^(- )?["\047][^"\047]*["\047][ ]*:/) fail("a quoted key is not supported: " line)
  if (bare ~ /[{}]/) fail("a flow mapping is not supported: " line)
  if (bare ~ /(^|[ :\[,-])[&*][A-Za-z0-9_-]/ || bare ~ /<<[ ]*:/) fail("anchors, aliases and merge keys are not supported: " line)
  if (bare ~ /uses[ ]*:/ && bare !~ /^(- )?uses:/) fail("uses: must start its line or its - item: " line)
  # A block scalar starts here; its text is skipped from the next line.
  if (bare ~ /:[ ]*[|>][-+0-9]*[ ]*$/) blk = ind + (line ~ /^- / ? 2 : 0)
}
# Top level.
ind == 0 {
  endcheckout(); endjob()
  injobs = (line ~ /^jobs:/)
  intop = 0
  if (line ~ /^permissions:/) {
    toplevelperm = 1; intop = 1
    if (line ~ /write/) fail("top-level permissions grant write")
  }
  next
}
intop && line ~ /write/ { fail("top-level permissions grant write: " line) }
# A job starts at two spaces under jobs:.
injobs && ind == 2 && line ~ /^[A-Za-z0-9_-]+:[ ]*$/ {
  endcheckout(); endjob()
  job = line; sub(/:.*/, "", job)
  next
}
injobs && (ind == 2 || job == "") { fail("expected a job name at two spaces under jobs: " line) }
# The checkout step in progress ends at the next item at its level or above.
co && (ind < coind || (ind == coind && line ~ /^- /)) { endcheckout() }
co && line ~ /^persist-credentials:[ ]*false([ ]|$)/ { copersist = 1 }
job != "" && (line ~ /:[ ]*["\047]?write([ "\047]|$)/ || line ~ /write-all/) { jobwrite = 1 }
{
  u = line
  sub(/^- /, "", u)
  if (u !~ /^uses:/) next
  ref = u
  sub(/^uses:[ ]*/, "", ref)
  if (ref !~ /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.\/-]+@[0-9a-f]+ # v[0-9]+(\.[0-9]+)*$/) {
    fail("not pinned to a commit with its tag in a comment (@<40-hex sha> # vX.Y.Z): " ref)
  } else {
    sha = ref; sub(/^[^@]*@/, "", sha); sub(/ .*/, "", sha)
    if (length(sha) != 40) fail("the commit SHA is not 40 hex characters: " ref)
  }
  name = ref; sub(/@.*/, "", name)
  if (name == "actions/checkout") {
    if (job != "" && jobcode == "") jobcode = "checks out code"
    co = 1; copersist = 0; coline = FNR; coind = ind
  }
  if (name == "actions/setup-go" && job != "" && jobcode == "") jobcode = "sets up Go"
}
END {
  endfile()
  exit bad
}
' "$@" || { echo "workflows: FAILED"; exit 1; }
echo "workflows: ok ($n files)"
