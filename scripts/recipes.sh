#!/usr/bin/env bash
# Check that the recipes and the skill name only tools, tool arguments and
# CLI commands that exist (requirement I3).
#
# Code in the Markdown (inline `spans` and fenced blocks) is checked:
# - `tool {"arg":...}` must name a tool in the golden tools file and only
#   that tool's arguments;
# - a span that is a bare lower-case word must be a tool, a tool argument,
#   a CLI command or a word on the allow-list below;
# - `a2p2 CMD [SUB] --flag` must be a command `a2p2 help` lists, and each
#   flag must appear in that command's help (or be --json or --as).
# Usage: scripts/recipes.sh [FILE.md...]   (default: recipes/ and skills/)
set -euo pipefail
set -f # code is split into words; never expand globs
cd "$(dirname "$0")/.."

tools_json=internal/mcpshim/testdata/tools.json
# Words that are not tools or arguments but are fine as bare code spans:
# room and agent names, result fields, harness names and status words.
allow='local everyone human id parent from created kind local_agents more
agents_seen_writing claude codex agy opencode hermes pi yes ok recent
answer_from from_this_machine person'

if [ "$#" -gt 0 ]; then files=("$@"); else
  files=(); while IFS= read -r f; do files+=("$f"); done < <(find recipes skills -name '*.md' | sort)
fi
[ "${#files[@]}" -gt 0 ] || { echo "recipes: no files to check"; exit 1; }

# tools.json is written by the golden test with two-space indentation:
# argument names sit at eight spaces, tool names at four.
toolargs=$(awk '
  /^        "[a-z_]+": \{/ { gsub(/[" :{]/, ""); args = args " " $0; next }
  /^    "name": "/ { n = $2; gsub(/[",]/, "", n); print n ":" args; args = "" }
' "$tools_json")
tools=$(printf '%s\n' "$toolargs" | cut -d: -f1)
[ "$(printf '%s\n' "$tools" | wc -l | tr -d ' ')" -ge 7 ] || { echo "recipes: could not read the tools from $tools_json"; exit 1; }
allargs=$(printf '%s\n' "$toolargs" | cut -d: -f2 | tr ' ' '\n' | sed '/^$/d' | sort -u)

home=$(mktemp -d)
trap 'rm -rf "$home"' EXIT
if [ -n "${A2P2_BIN:-}" ]; then help=$(A2P2_HOME="$home" "$A2P2_BIN" help)
else help=$(A2P2_HOME="$home" go run ./cmd/a2p2 help); fi

# One line per piece of code: inline spans, and each line of a fenced block.
code() {
  awk -v f="$1" '
    /^ *```/ { fence = !fence; next }
    fence { print f ":" FNR ":" $0; next }
    { s = $0
      while (match(s, /`[^`]+`/)) {
        print f ":" FNR ":" substr(s, RSTART + 1, RLENGTH - 2)
        s = substr(s, RSTART + RLENGTH)
      } }' "$1"
}

# The help text of one command: its line and the indented lines under it.
cmdhelp() {
  printf '%s\n' "$help" | awk -v c="  a2p2 $1" '
    index($0, c) == 1 && (length($0) == length(c) || substr($0, length(c) + 1, 1) ~ /[ |]/) { on = 1; print; next }
    on && /^                                  / { print; next }
    { on = 0 }'
}

fail=0
bad() { echo "recipes: $1"; fail=1; }
for f in "${files[@]}"; do
  # Short enough to read in one go: a recipe under 60 lines, a skill under 120.
  n=$(wc -l < "$f" | tr -d ' ')
  case "$f" in
    */SKILL.md) [ "$n" -lt 120 ] || bad "$f: $n lines; a skill stays under 120" ;;
    recipes/*) [ "$n" -lt 60 ] || bad "$f: $n lines; a recipe stays under 60" ;;
  esac
  while IFS= read -r line; do
    where=${line%%:*}; rest=${line#*:}; where="$where:${rest%%:*}"; c=${rest#*:}
    # Tool calls: name {"arg":...}
    while read -r call; do
      [ -n "$call" ] || continue
      name=${call%% *}
      args=$(printf '%s\n' "$toolargs" | awk -F: -v n="$name" '$1 == n { print $2 }')
      if ! grep -qx -- "$name" <<< "$tools"; then bad "$where: no tool named $name"; continue; fi
      for k in $(printf '%s' "$call" | grep -oE '[{,]"[a-z_]+":' | tr -d '{,":'); do
        case " $args " in *" $k "*) ;; *) bad "$where: tool $name has no argument $k" ;; esac
      done
    done < <(printf '%s\n' "$c" | grep -oE '(^|[^a-z_])[a-z_]+ \{"[^`]*' | sed -E 's/^[^a-z_]//')
    # Bare words.
    # Here-strings and process substitution, not pipes: under pipefail a
    # grep -q that stops reading early fails the printf that feeds it.
    if grep -qxE '[a-z_]+' <<< "$c"; then
      grep -qx -- "$c" < <(printf '%s\n' "$tools" "$allargs" $allow) ||
        grep -qE "a2p2 ([a-z]+ \| )*$c([ |)]|$)" <<< "$help" ||
        bad "$where: \`$c\` is not a tool, an argument, a command or an allowed word"
    fi
    # CLI commands.
    while read -r cmd; do
      [ -n "$cmd" ] || continue
      set -- $cmd; shift
      [ "$#" -gt 0 ] || continue
      key=$1
      case "$1" in room|net) [ "$#" -gt 1 ] && key="$1 $2" ;; esac
      h=$(cmdhelp "$key")
      # Otherwise any help line that names it: "stop | daemon | version",
      # "(undo with: a2p2 uninstall NAME)".
      [ -n "$h" ] || [ "$key" != "$1" ] || h=$(printf '%s\n' "$help" | grep -E "a2p2 ([a-z]+ \| )*$1([ |)]|$)" || true)
      if [ -z "$h" ]; then bad "$where: a2p2 help lists no command 'a2p2 $key'"; continue; fi
      for fl in $(printf '%s' "$cmd" | grep -oE -- '--[a-z-]+' || true); do
        case "$fl" in --json|--as) continue ;; esac
        grep -q -- "$fl" <<< "$h" || bad "$where: 'a2p2 $key' has no flag $fl"
      done
    done < <(printf '%s\n' "$c" | grep -oE 'a2p2 [a-z][^;&|`]*' | sed -E 's/ a2p2 .*//')
  done < <(code "$f")
done
[ "$fail" = 0 ] || exit 1
echo "recipes: ${#files[@]} files; tools, arguments and commands all exist"
