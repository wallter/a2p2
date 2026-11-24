# team: start a team of harnesses on an a2p2 room

`team` starts several AI coding sessions (Claude Code, Codex CLI,
Antigravity CLI, or any command) that coordinate through one a2p2 room,
watches them, stops them and measures the run. It is an optional layer on
top of a2p2 and uses only the `a2p2` CLI and its MCP server; nothing in the
daemon knows about it ([D-team](../../docs/DECISIONS.md)). It needs
Python 3 and nothing else.

## What it does

- **`team up`** checks the manifest, starts the daemon for the manifest's
  `a2p2_home` if needed, creates the room if it is missing, registers
  every label, posts a kickoff message as the lead, and starts each member
  in its own working directory (or a git worktree) with its own label,
  model, sandbox or permission profile, wall-clock limit and brief. The
  a2p2 MCP server is given to each harness for that run only, on its
  command line; for Antigravity, in `.agents/mcp_config.json` inside a
  directory the launcher created. No harness configuration is edited.
  `--wait` waits until every member has finished and prints the report.
- **The watcher** runs in the background and posts ordinary room messages
  from the label `team`:
  - `started`, once the member's tool server has connected: its label was
    registered again under the harness's name after launch, which only
    that member's MCP server does (the launcher registered the label
    earlier as `cli`). It is seen within one poll interval.
  - `done` or `failed`, with the exit status, the wall-clock time and where
    the final answer was saved, and a note if the tool server never
    connected.
  - `limit` when a member passes its wall-clock limit; its process group
    gets SIGTERM, then SIGKILL five seconds later.
  - `silent` when a member has posted nothing in the room for
    `silence_seconds`, once per quiet spell.
  - `needs input` when a member sends a question to the person.
  - `stopped` after `team stop`.

  It uses exit statuses, the files a harness writes and what the room
  shows. It does not read a harness's screen, and it never acts on what
  members write.
- **`team status`** lists the roster: label, harness, pid, state, the last
  room message and the elapsed time.
- **`team stop [LABEL]`** ends one member, or all of them.
- **`team logs LABEL [-n N]`** prints a member's output and error logs.
- **`team report`** writes `report.json` and `report.md` in the state
  directory. For each member it gives the wall-clock time, how long the
  tool server took to connect, messages sent, replies that reached more
  than one inbox, questions to the person, refusals by code and empty
  inbox polls (both counted from the harness's log), the exit state and
  whether its last report began `DONE:` or `BLOCKED:`. For the run it
  counts messages from the lead, the watcher and the person, and requests
  waiting for the person. The watcher writes the report when the last
  member finishes.

Every member's brief starts with [prelude.md](prelude.md): call `whoami`,
read the kickoff, reply with a plan, check `inbox` between steps, send
questions to the lead, and finish with a `DONE:` or `BLOCKED:` reply. Set
`prelude` in the manifest to use another file; `{label}`, `{room}`,
`{lead}` and `{kickoff}` are filled in.

## Manifest

One JSON file. JSON because every Python 3 reads it without a library
and a2p2 already speaks it; put long briefs in files with `brief_file`.
Relative paths are relative to the manifest.

```json
{
  "room": "crew",
  "a2p2_home": "/tmp/a2p2-team-example/home",
  "a2p2": "a2p2",
  "state_dir": "/tmp/a2p2-team-example/state",
  "max_members": 2,
  "silence_seconds": 120,
  "kickoff": "Each member replies with a one-line plan, then a DONE: report.",
  "members": [
    {"label": "luna", "harness": "codex", "model": "gpt-6-luna",
     "sandbox": "read-only", "limit_seconds": 300,
     "brief": "Reply to the kickoff with a plan, then with DONE:."},
    {"label": "haiku", "harness": "claude", "model": "haiku",
     "max_budget_usd": 0.5, "limit_seconds": 300,
     "brief": "Reply to the kickoff with a plan, then with DONE:."}
  ]
}
```

Run-level keys: `room` (created if missing), `a2p2_home` (required, so the
node is always named; use a throwaway one to try it), `a2p2` (the binary,
default `a2p2` on the `PATH`), `state_dir` (default `.team/<manifest
name>` next to the manifest), `max_members` (default 4, at most 16),
`lead` (the kickoff's author label, default `lead`), `watcher_label`
(default `team`), `kickoff`, `prelude`, `poll_seconds` (default 5) and
`silence_seconds` (default 300).

Member keys: `label`, `harness` (`claude`, `codex`, `agy` or `command`),
`model`, `brief` or `brief_file`, `limit_seconds` (default 900),
`silence_seconds`, and one of `workdir` (an existing directory) or
`worktree` (a repository: the launcher runs `git worktree add --detach`
under the state directory; remove it afterwards with `git worktree
remove`). Without either, the member gets an empty directory. Per harness:

| Harness | How it is started | Profile keys |
|---|---|---|
| `claude` | `claude -p <prompt> --model M --no-session-persistence --mcp-config <file> --strict-mcp-config --allowedTools 'mcp__a2p2__*' --output-format stream-json --verbose` | `allowed_tools`, `permission_mode`, `max_budget_usd` |
| `codex` | `codex exec <prompt> -m M -s S --ephemeral --skip-git-repo-check -C <dir> -c mcp_servers.a2p2...` with `default_tools_approval_mode="approve"`, `--json -o <final>` | `sandbox` (default `read-only`) |
| `agy` | `agy --settings '{"permissions":{"allow":["mcp(a2p2/*)"]}}' --model M --print-timeout <limit>s -p=<prompt>` from a directory with `.agents/mcp_config.json` | none |
| `command` | `command`, a list of arguments with `{a2p2}`, `{home}`, `{label}`, `{room}`, `{workdir}`, `{final}`, `{prompt_file}` and `{team_dir}` filled in | `mcp_harness`: the `--harness` name its MCP server registers with |

The state directory holds `roster.json`, `watcher.log`, the reports, and
per member `prompt.md`, `out.log`, `err.log` and `final.txt`.

## Example

```sh
make build
d=$(mktemp -d /tmp/a2p2x.XXXXXX) && chmod 700 "$d"
sed "s#/tmp/a2p2-team-example#$d#; s#\"a2p2\": \"a2p2\"#\"a2p2\": \"$PWD/bin/a2p2\"#" \
  scripts/team/examples/two-harness.json > "$d/team.json"
scripts/team/team -m "$d/team.json" up --wait
A2P2_HOME="$d/home" bin/a2p2 history --room crew
A2P2_HOME="$d/home" bin/a2p2 stop
```

[examples/run-2026-11-24](examples/run-2026-11-24/report.md) is the report
of that run with Codex CLI 0.171.0 (gpt-6-luna) and Claude Code 2.1.331
(haiku): both connected, posted a plan and a `DONE:` report, and exited 0
in 25 and 15 seconds.

## What it does not do

- It does not decide who does what, merge work, review it or retry a
  failed member. Those are the lead's job.
- It does not wake an idle member or pass it messages; members read the
  room with `inbox`.
- It does not approve joins, turn networking on or answer the person's
  questions; those stay with the person.
- It does not keep running after a reboot, and it is not tested on Linux
  with real harnesses.
- It does not count tokens or cost; harness logs differ and are kept for
  that.

## Evidence

There is no evidence yet that a formation of agents does better than one
strong agent given the same compute. `team report` exists so that a run
can be compared with a single-agent run on the same task before anyone
relies on formations.

## Tests

`make team-test` (part of `make check`) runs [test_team.py](test_team.py)
against [fake_harness.py](fake_harness.py), which starts `a2p2 mcp` and
calls its tools like a harness would, then exits 0, exits non-zero, hangs,
never connects, asks the person or posts text that looks like commands.
