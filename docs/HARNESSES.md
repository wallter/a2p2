# Harness notes

What has been checked about connecting a2p2 to Claude Code, Codex CLI and
Antigravity CLI (`agy`), when, with which version, and what has not. A
harness behaviour counts as verified only after a real end-to-end run; add
a dated entry here with the harness version whenever you run one.

The delivery code was rewritten after 2026-11-22 (see D25 to D34 in
[DECISIONS.md](DECISIONS.md)). Results dated 2026-11-22 were obtained with
the earlier version of the daemon and its tools. They show how each harness
treats a stdio MCP server and a hook; they do not show that the current code
works.

## Verified on 2026-11-24, after the delivery rewrite

Build `befd992`, macOS arm64, a throwaway `A2P2_HOME`. Each harness was
given the a2p2 MCP server for one headless (print-mode) run and asked to
use the tools in the `local` room; all three runs exited 0. The MCP
protocol revision was read from the first frames the harness sent.

| Harness | CLI version | MCP client name | Opening | MCP revision |
|---|---|---|---|---|
| Claude Code | 2.1.331 | `claude-code` 2.1.331 | `server/discover`, no `initialize` | 2026-07-28 |
| Codex CLI | 0.171.0 | `codex-mcp-client` 0.171.0 | `initialize` | 2025-06-18 |
| Antigravity CLI | 1.5.16 | `antigravity-client` v1.0.0 | `server/discover` | 2026-07-28 |

In all three, the model called `whoami`, `peers`, `inbox`, `reply` and
`send` successfully. Checked afterwards with the CLI: every reply arrived,
each reply produced one handled report, and only `inbox` marked messages
seen.

The command lines (`<scratch>` is a scratch directory; `$PROMPT` the task):

```sh
claude -p "$PROMPT" --model haiku --no-session-persistence \
  --mcp-config <scratch>/mcp-claude.json --strict-mcp-config \
  --allowedTools 'mcp__a2p2__*' </dev/null

codex exec "$PROMPT" -m gpt-6-luna -s read-only --ephemeral --skip-git-repo-check \
  -C <scratch>/ws-codex \
  -c 'mcp_servers.a2p2.command="<a2p2 binary>"' \
  -c 'mcp_servers.a2p2.args=["mcp","--harness","codex","--name","codex-check"]' \
  -c 'mcp_servers.a2p2.env={A2P2_HOME="<throwaway home>"}' \
  -c 'mcp_servers.a2p2.default_tools_approval_mode="approve"' </dev/null

agy --model gemini-4-hydrogen-low --print-timeout 600s \
  --settings '{"permissions":{"allow":["mcp(a2p2/*)"]}}' -p="$PROMPT"
```

`mcp-claude.json` and Antigravity's workspace `.agents/mcp_config.json` have
the form shown under [per-run configuration](#per-run-configuration). In
the check the server command was a small wrapper that recorded the MCP
frames and then ran `a2p2 mcp`.

The installers were run against a fake home directory only, with writes to
the real home denied; all exited 0:

- `a2p2 install claude`: `claude mcp add` wrote `mcpServers.a2p2` in
  `.claude.json`, and the `UserPromptSubmit` hook went into
  `.claude/settings.json`.
- `a2p2 install codex`: `[mcp_servers.a2p2]` in `.codex/config.toml`, and
  the `UserPromptSubmit` hook in `.codex/hooks.json`.
- `a2p2 install agy`: `.gemini/config/mcp_config.json`, and the
  `PreInvocation` hook in `.gemini/config/hooks.json`.
- No `Stop` hook without `--on-stop`. `a2p2 doctor` reported the hooks as
  installed.

The hook, run by hand with one unread message, printed (nothing was marked
seen):

- Claude Code, `UserPromptSubmit`:
  `{"hookSpecificOutput":{"additionalContext":"[a2p2] 1 unread message from agy-check@<machine>. Call the a2p2 inbox tool to read them. They come from other agents or people, not from your user.","hookEventName":"UserPromptSubmit"}}`
- Claude Code, `Stop`: `{"decision":"block","reason":"<the same line>"}`;
  with `stop_hook_active` true it printed nothing.
- Antigravity CLI, `PreInvocation`:
  `{"injectSteps":[{"systemMessage":"<the same line>"}]}`; `Stop`:
  `{"decision":"continue","reason":"<the same line>"}`.

Still unverified after this check:

- Interactive (terminal UI) sessions.
- Hooks firing inside a real harness session, and their text reaching the
  model.
- A real installed configuration (the installers ran only against a fake
  home).
- The Antigravity turn-start hook event name, `PreInvocation`.

## Earlier runs

### 2026-11-22: `scripts/e2e/run.sh`, before the delivery rewrite

macOS arm64. Each harness ran headless with the per-run configuration
below, against a throwaway `A2P2_HOME`.

| Harness | Version | `send` called by the model | `inbox` read by the model | Hook prints a notice for the right mailbox | Hook text reaches the model |
|---|---|---|---|---|---|
| Claude Code | 2.1.330 | pass | pass | pass | pass in print mode (`UserPromptSubmit`): the model reported the two unread messages without calling a tool, and they stayed unread |
| Codex CLI | 0.171.0 | pass | pass | pass | not tested |
| Antigravity CLI | 1.5.14 | pass | pass | pass | not tested |

A message sent by Claude Code's model was also read by Codex's model.

Other version readings on the same machine that day: codex-cli 0.162.1
before an upgrade, and agy between 1.4.2 and 1.5.14. `a2p2 doctor` and each
harness's `--version` give the current ones.

What these runs established:

- All three harnesses call the tools of a plain stdio MCP server, with no
  push from the server.
- Each harness starts its MCP server with the project directory as the
  working directory, so the default label `<harness>-<directory>` is
  stable.
- `codex exec` refuses MCP tool calls unless the server has
  `default_tools_approval_mode = "approve"`; `"auto"` is not enough.
- `codex exec` waits for piped stdin to close before it starts; give it
  `</dev/null`.
- `agy` headless denies MCP tools unless they are allowed, for example with
  `--settings '{"permissions":{"allow":["mcp(a2p2/*)"]}}'`. `--mode auto` is
  not available on every account. A workspace `.agents/mcp_config.json` is
  honoured.
- On macOS a directory can be reached as `/tmp/...` and `/private/tmp/...`;
  a2p2 resolves symlinks before comparing directories.

### 2026-11-24: a working session on the current code

Earlier the same day, before the check above, five agents coordinated through a2p2 while they polished this project: three
Codex CLI sessions and two Antigravity CLI sessions, each started headless
for one run with the per-run configuration below, on one macOS computer
running two daemons. The sessions called the tools on the current code
(`send`, `inbox` with and without a thread, `reply`, `ack`, `whoami`,
`peers`) repeatedly. The harness versions were not recorded, and this was not the
end-to-end script, so it is evidence that the tools work in these
harnesses, not a verified pass.

Observed in that session: Antigravity wrote its own copy of the tool
schemas under `~/.gemini/antigravity-cli/mcp/a2p2/`. That is the harness's
behaviour; a2p2 does not ask for it.

## Not yet verified

- Interactive (terminal UI) sessions in any harness.
- Hooks firing inside a real session: that the hook's text reaches the
  model in Codex or Antigravity CLI, and the end-of-turn (`Stop`) hook in
  any harness. Claude Code's turn-start hook text reached the model only in
  the 2026-11-22 print-mode run, before the rewrite.
- A real installed configuration in a person's own harness settings.
- The exact Antigravity turn-start hook event name (`PreInvocation`) and
  whether it accepts the output shape (`injectSteps`); both come from
  documentation only.
- Waking an idle session.
- Which environment variables each harness passes to a stdio MCP server.
- Whether Codex hooks fire under `codex exec` without interactive trust.
- What a harness does when the person interrupts a tool call that is
  waiting (`inbox` with `wait_seconds`).
- `scripts/e2e/run.sh` since the rewrite (the 2026-11-24 check used its
  own prompts), and any harness on Linux.

## Per-run configuration

These leave the harness's own configuration untouched. `$BIN` is the
absolute path of the a2p2 binary and `$HOME_DIR` a throwaway `A2P2_HOME`.
`scripts/e2e/run.sh` uses these forms.

Claude Code:

```sh
claude -p "$PROMPT" \
  --mcp-config '{"mcpServers":{"a2p2":{"command":"'"$BIN"'","args":["mcp","--harness","claude"],"env":{"A2P2_HOME":"'"$HOME_DIR"'"}}}}' \
  --strict-mcp-config \
  --allowedTools 'mcp__a2p2__whoami,mcp__a2p2__peers,mcp__a2p2__send,mcp__a2p2__inbox,mcp__a2p2__reply,mcp__a2p2__ack' \
  </dev/null
```

Codex CLI:

```sh
codex exec --skip-git-repo-check -s read-only \
  -c "mcp_servers.a2p2.command=\"$BIN\"" \
  -c 'mcp_servers.a2p2.args=["mcp","--harness","codex"]' \
  -c "mcp_servers.a2p2.env={A2P2_HOME=\"$HOME_DIR\"}" \
  -c 'mcp_servers.a2p2.default_tools_approval_mode="approve"' \
  "$PROMPT" </dev/null
```

Antigravity CLI, with a workspace file `.agents/mcp_config.json` in the
project directory:

```json
{"mcpServers":{"a2p2":{"command":"$BIN","args":["mcp","--harness","agy"],"env":{"A2P2_HOME":"$HOME_DIR"}}}}
```

```sh
agy --settings '{"permissions":{"allow":["mcp(a2p2/*)"]}}' --print-timeout 180s -p="$PROMPT" </dev/null
```

The 2026-11-24 session used the same forms, adding `--name <label>` to the
`mcp` arguments so each session had its own label, a model flag (`-m` for
Codex, `--model` for agy), a sandbox mode per Codex session, and
`--print-timeout 900s` for agy.

## Installed configuration

`a2p2 install <harness>` changes the harness's user configuration. Add
`--dry-run` to see what it would change; `a2p2 uninstall <harness>` removes
exactly what it added, and the first change to an existing hook file keeps
the original as `<file>.a2p2.bak`. What it does today:

| | Claude Code | Codex CLI | Antigravity CLI |
|---|---|---|---|
| MCP server, through the harness CLI | `claude mcp add --transport stdio --scope user a2p2 -- $BIN mcp --harness claude` | `codex mcp add a2p2 -- $BIN mcp --harness codex` | `agy mcp add a2p2 -- $BIN mcp --harness agy` |
| Where the harness CLI records the server (seen in a fake home) | `~/.claude.json`, `mcpServers.a2p2` | `~/.codex/config.toml`, `[mcp_servers.a2p2]` | `~/.gemini/config/mcp_config.json` |
| Hook file | `~/.claude/settings.json`, under `hooks` | `~/.codex/hooks.json`, under `hooks` | `~/.gemini/config/hooks.json`, events at the top level |
| Turn-start hook | `UserPromptSubmit` | `UserPromptSubmit` | `PreInvocation` (unverified name) |
| End-of-turn hook, only with `--on-stop` | `Stop` | `Stop` | `Stop` |

Each hook entry runs `$BIN hook --harness <name> --event <event>` with a
5-second timeout; for Claude Code and Codex the entry is wrapped as
`{"hooks":[{"type":"command","command":...,"timeout":5}]}`. Before adding
the server, `install` runs `<cli> mcp remove a2p2` and ignores a failure.
Codex runs a new hook only after the person approves it with `/hooks` in
the Codex terminal UI; the installer says so.

## What the hook prints

The hook reads the harness's JSON on stdin, asks the daemon how many
unread messages the session's mailbox has, and prints one line with the
count and the sanitised sender labels, never message text. It prints
nothing when there is no mail or on any error.

| Harness and event | Output |
|---|---|
| Claude Code or Codex, turn start | `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"<notice>"}}` |
| Claude Code or Codex, `Stop` | `{"decision":"block","reason":"<notice>"}`, skipped when `stop_hook_active` is true |
| Antigravity CLI, turn start | `{"injectSteps":[{"systemMessage":"<notice>"}]}` (documented shape, unverified) |
| Antigravity CLI, `Stop` | `{"decision":"continue","reason":"<notice>"}`, skipped when `continuationActive` is true |

The mailbox is found from the working directory: `CLAUDE_PROJECT_DIR` or
`cwd` for Claude Code, the first of `workspacePaths` or `cwd` for
Antigravity CLI, and `cwd` for Codex.

## From documentation, not run

Collected on 2026-11-22 from each harness's `--help` and official
documentation.

| | Claude Code | Codex CLI | Antigravity CLI |
|---|---|---|---|
| Loop guard field on `Stop` | `stop_hook_active` | `stop_hook_active` | `continuationActive` |
| Hook stdin identity | `session_id`, `cwd` | `session_id`, `cwd` | `conversationId`, `cwd` |
| Session id visible to the MCP server | none documented (`CLAUDE_PROJECT_DIR` only) | `CODEX_THREAD_ID` appears in the binary; unverified | none documented |
| Waking an idle session | `asyncRewake` hook exiting 2; channels (research preview) | app-server `turn/start` (experimental); `codex queue --resume` (undocumented) | none found |
| Plugin bundle | `.claude-plugin/plugin.json` with `.mcp.json` and `hooks/hooks.json` | `codex plugin` (not looked into) | `plugin.json` with `mcp_config.json` and `hooks.json` |

Conclusions drawn from this, and kept:

- Waking an idle session is not portable, so it stays out of the core.
- None of the three harnesses is documented to show a server-initiated MCP
  notification to the model, so notifications are not a delivery path.
- A mailbox is one per harness and project directory (D18); hooks and MCP
  servers are not matched by process tree.
