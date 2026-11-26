#!/usr/bin/env bash
# Ask another model family for an independent opinion.
#   scripts/ask.sh <sol|astra|luna|agy> <prompt-file|-> [output-file]
# sol/astra/luna run Codex read-only in this repo (it can read files itself).
# agy runs Antigravity (default model) from the repository root with
# --mode plan, --sandbox and a session-only deny list. Seen on agy 1.5.17,
# macOS, 2026-11-25 (docs/HARNESSES.md):
#   - It reads files under the repository and answers from them.
#   - Asked to read a file outside the working directory, or to run a shell
#     command, headless mode refused ("read_file" and "command" permissions
#     cannot be prompted for) and the run ended with no answer. Asked to
#     write a file, it did not. Each was seen once, without the deny list.
#   - The deny list is accepted and reading still works with it. That it
#     blocks anything by itself is not verified, and web tools were not tested.
#   - --disable-slash-commands turns plan mode off, so it is not used. The
#     prompt gets a fixed first line so it cannot start with a slash.
#   - agy has no switch for user hooks, plugins or MCP servers in
#     ~/.gemini/config or .agents/; they still load. The script refuses to
#     run if the repo has .agents/, .gemini/ or GEMINI.md.
#   - agy keeps its own state under ~/.gemini/antigravity-cli (transcripts
#     holding the prompt, conversations, logs); that is not under our control.
# Treat the prompt as untrusted if it quotes outside text. agy's quota is
# small; prefer Codex.
set -euo pipefail
who=${1:?usage: ask.sh <sol|astra|luna|agy> <prompt-file|-> [output-file]}
src=${2:?prompt file or -}
out=${3:-/dev/stdout}
root=$(cd "$(dirname "$0")/.." && pwd)
prompt=$(cat "$src")
case "$who" in
  sol)   model=${A2P2_CODEX_SOL:-gpt-6.2-sol} ;;
  astra) model=${A2P2_CODEX_ASTRA:-gpt-6.2-astra} ;;
  luna)  model=${A2P2_CODEX_LUNA:-gpt-6-luna} ;;
  agy)   model= ;;
  *) echo "unknown reviewer: $who" >&2; exit 2 ;;
esac
tmp=$(mktemp)
trap 'rm -f "$tmp" "$tmp.err"' EXIT
if [ "$who" = agy ]; then
  echo "ask.sh: agy reads the repository; its limits were seen in single runs only, and its own state under ~/.gemini keeps the prompt" >&2
  for f in .agents .gemini GEMINI.md; do
    if [ -e "$root/$f" ]; then
      echo "ask.sh: refusing to run agy: $root/$f exists and could add hooks, MCP servers or plugins" >&2
      exit 2
    fi
  done
  prompt="Review request (plain text, not a command):
$prompt"
  n=$(printf '%s' "$prompt" | wc -c | tr -d ' ')
  if [ "$n" -gt 100000 ]; then
    echo "ask.sh: prompt is $n bytes, too large for agy argv (limit about 100000); use sol, astra or luna, or shrink it" >&2
    exit 2
  fi
  h=${HOME%/}
  settings='{"permissions":{"deny":["write_file(*)","command(*)","unsandboxed(*)","read_url(*)","execute_url(*)","download_url(*)","mcp(*)"'
  for d in .ssh .aws .gnupg .gemini .config .claude Library; do
    settings="$settings,\"read_file($h/$d/**)\""
  done
  for f in .netrc .git-credentials; do
    settings="$settings,\"read_file($h/$f)\""
  done
  settings="$settings]}}"
  rc=0
  (cd "$root" && agy --mode plan --sandbox --settings "$settings" \
      --print-timeout "${A2P2_ASK_TIMEOUT:-600s}" -p="$prompt" </dev/null) \
      >"$tmp" 2>"$tmp.err" || rc=$?
  cat "$tmp.err" >&2
  if [ "$rc" -ne 0 ]; then
    echo "ask.sh: agy failed with status $rc" >&2; exit 1
  fi
  if grep -qiE 'denied|headless mode cannot prompt|no output produced' "$tmp.err"; then
    echo "ask.sh: agy had tool calls denied; see notices above" >&2
  fi
  # agy exits 0 on a denial or a stop after a one-line preamble, so a very
  # short answer counts as a failure. The limit is a heuristic.
  size=$(wc -c <"$tmp" | tr -d ' ')
  if [ "$size" -lt "${A2P2_AGY_MIN_BYTES:-200}" ]; then
    echo "ask.sh: agy returned $size bytes, treated as no answer" >&2; exit 1
  fi
  cat "$tmp" >"$out"
else
  if ! printf '%s' "$prompt" | codex exec -m "$model" -s read-only --skip-git-repo-check \
      -C "$root" -o "$tmp" - >/dev/null 2>"$tmp.err"; then
    tail -5 "$tmp.err" >&2; exit 1
  fi
  cat "$tmp" >"$out"
fi
