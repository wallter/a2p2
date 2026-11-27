#!/usr/bin/env bash
# End-to-end check against REAL harnesses, run headless. Spends model tokens.
#   scripts/e2e/run.sh [claude|codex|agy|all]
#
# Uses a throwaway A2P2_HOME and a throwaway project directory, and passes the
# MCP server to each harness on its command line (or a workspace-local config),
# so your real a2p2 node and your harness configuration are not touched.
#
# For each harness it proves two things:
#   outbound: the model calls the a2p2 `send` tool and the message is stored
#   inbound:  a message sent to the agent is returned by its `inbox` tool
# With more than one harness it also proves a message from one reaches another.
set -uo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
bin="$root/bin/a2p2"
[ -x "$bin" ] || { echo "build first: make build" >&2; exit 2; }
which=${1:-all}
[ "$which" = all ] && which="claude codex agy"

work=$(mktemp -d /tmp/a2p2-e2e.XXXXXX)
export A2P2_HOME="$work/home"
proj="$work/proj"; mkdir -p "$proj"
trap '"$bin" stop >/dev/null 2>&1; rm -rf "$work"' EXIT
"$bin" status >/dev/null || { echo "daemon did not start" >&2; exit 1; }
node=$("$bin" status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["node"][:8])')

pass=0; fail=0
ok()  { echo "  PASS  $1"; pass=$((pass+1)); }
bad() { echo "  FAIL  $1"; fail=$((fail+1)); [ -n "${2:-}" ] && echo "$2" | tail -15 | sed 's/^/        /'; }
have_text() { "$bin" history --as tester -n 200 --json | grep -q "$1"; }

# run_<harness> PROMPT -> prints the model's output; runs in $proj.
# stdin is /dev/null: codex exec otherwise waits for piped stdin to close.
run_claude() {
  local cfg; cfg=$(printf '{"mcpServers":{"a2p2":{"command":"%s","args":["mcp","--harness","claude"],"env":{"A2P2_HOME":"%s"}}}}' "$bin" "$A2P2_HOME")
  (cd "$proj" && claude -p "$1" --mcp-config "$cfg" --strict-mcp-config \
     --allowedTools 'mcp__a2p2__whoami,mcp__a2p2__peers,mcp__a2p2__send,mcp__a2p2__inbox,mcp__a2p2__reply,mcp__a2p2__ack' </dev/null 2>&1)
}
run_codex() {
  (cd "$proj" && codex exec --skip-git-repo-check -s read-only \
     -c "mcp_servers.a2p2.command=\"$bin\"" \
     -c 'mcp_servers.a2p2.args=["mcp","--harness","codex"]' \
     -c "mcp_servers.a2p2.env={A2P2_HOME=\"$A2P2_HOME\"}" \
     -c 'mcp_servers.a2p2.default_tools_approval_mode="approve"' \
     ${A2P2_E2E_CODEX_FLAGS:-} "$1" </dev/null 2>&1)
}
run_agy() {
  mkdir -p "$proj/.agents"
  printf '{"mcpServers":{"a2p2":{"command":"%s","args":["mcp","--harness","agy"],"env":{"A2P2_HOME":"%s"}}}}' "$bin" "$A2P2_HOME" > "$proj/.agents/mcp_config.json"
  (cd "$proj" && agy --settings '{"permissions":{"allow":["mcp(a2p2/*)"]}}' --print-timeout 180s ${A2P2_E2E_AGY_FLAGS:-} -p="$1" </dev/null 2>&1)
}

for h in $which; do
  command -v "$h" >/dev/null || { echo "$h: not installed, skipping"; continue; }
  echo "$h ($("$h" --version 2>/dev/null | head -1))"
  agent="$h-proj@$node"

  nonce="e2e-out-$h-$RANDOM$RANDOM"
  out=$("run_$h" "Use the a2p2 MCP server's send tool to post exactly this text to the default room: $nonce . Then answer with the single word DONE.")
  if have_text "$nonce"; then ok "outbound: model called send"; else bad "outbound: message not stored" "$out"; fi

  if "$bin" status --json | grep -q "\"$agent\""; then ok "registered as $agent"; else bad "agent $agent not registered" "$("$bin" status)"; fi

  word="zebra$RANDOM"
  "$bin" send --as tester --to "$agent" "The code word is $word." >/dev/null
  out=$("run_$h" "Call the a2p2 MCP server's inbox tool and tell me the code word it contains. Answer with just the code word.")
  if echo "$out" | grep -q "$word"; then ok "inbound: model read its inbox"; else bad "inbound: code word not in output" "$out"; fi

  # The unread notice the hook would print for this agent's mailbox.
  "$bin" send --as tester --to "$agent" "hook check" >/dev/null
  notice=$(echo "{\"cwd\":\"$proj\",\"workspacePaths\":[\"$proj\"]}" | CLAUDE_PROJECT_DIR="$proj" "$bin" hook --harness "$h" --event "$([ "$h" = agy ] && echo PreInvocation || echo UserPromptSubmit)")
  if echo "$notice" | grep -q 'unread'; then ok "hook prints an unread notice"; else bad "hook printed nothing for a mailbox with mail" "$notice"; fi
done

set -- $which
if [ $# -ge 2 ] && command -v "$1" >/dev/null && command -v "$2" >/dev/null; then
  echo "cross-harness: $1 -> $2"
  word="otter$RANDOM"
  "run_$1" "Use the a2p2 send tool to send this exact text to the agent $2-proj@$node : The password is $word. Then answer DONE." >/dev/null
  out=$("run_$2" "Call the a2p2 inbox tool. Another agent sent you a password. Answer with just the password.")
  if echo "$out" | grep -q "$word"; then ok "$1's message was read by $2"; else bad "$2 did not report $1's message" "$out"; fi
fi

echo
"$bin" status | sed -n '/agents on this machine/,$p'
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
