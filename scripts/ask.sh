#!/usr/bin/env bash
# Ask another model family for an independent opinion.
#   scripts/ask.sh <sol|astra|luna|agy> <prompt-file|-> [output-file]
# sol/astra/luna run Codex read-only in this repo (it can read files itself).
# agy runs Antigravity with its default model and no file access: put everything
# it needs in the prompt. agy's quota is small; prefer Codex.
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
if [ "$who" = agy ]; then
  agy --print-timeout "${A2P2_ASK_TIMEOUT:-600s}" -p="$prompt" >"$out"
else
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  if ! printf '%s' "$prompt" | codex exec -m "$model" -s read-only --skip-git-repo-check \
      -C "$root" -o "$tmp" - >/dev/null 2>"$tmp.err"; then
    tail -5 "$tmp.err" >&2; rm -f "$tmp.err"; exit 1
  fi
  rm -f "$tmp.err"
  cat "$tmp" >"$out"
fi
