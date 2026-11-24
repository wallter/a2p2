# team: start a team of harnesses on an a2p2 room

`team` starts several AI coding sessions (Claude Code, Codex CLI,
Antigravity CLI, or any command) that coordinate through one a2p2 room,
watches them, stops them and measures the run. It is an optional layer on
top of a2p2 and uses only the `a2p2` CLI and its MCP server; nothing in the
daemon knows about it ([D40](../../docs/DECISIONS.md)). It needs
Python 3 and nothing else.

## What it does

- **`team up`** checks the manifest, claims the state directory (a second
  `up` on it is refused), creates each member's working directory and
  per-run files, starts the daemon for the manifest's `a2p2_home` if
  needed, creates the room if it is missing, registers every label, posts
  a kickoff message as the lead, and starts the supervisor. The a2p2 MCP
  server is given to each harness for that run only, on its command line;
  for Antigravity, in `.agents/mcp_config.json` in the member's directory.
  No harness configuration is edited. `--wait` waits until every member
  has finished and prints the report.
- **The supervisor** starts each member under a small guard process that
  leads the member's process group, and records each launch as it
  happens. It alone signals members, and only process groups whose
  leader it started and has not yet reaped. It ends a member's group when
  the member passes its limit, when `team stop` asks, and when the
  supervisor itself gets SIGTERM, SIGINT or SIGHUP or fails. If the
  supervisor is killed outright, each guard sees its pipe from the
  supervisor close and ends its own group.
- **The watcher**, a thread of the supervisor, reads the room and posts
  ordinary messages from the label `team`:
  - `started`, once the member's tool server has connected (see
    [readiness](#readiness)). It is seen within one poll interval.
  - `done` or `failed`, with the exit status, the wall-clock time and where
    the final answer was saved.
  - `not ready` when a member exits without its tool server ever having
    connected, whatever its exit status.
  - `limit` when a member passes its wall-clock limit.
  - `silent` when a member has posted nothing in the room for
    `silence_seconds`, once per quiet spell.
  - `needs input` when a member sends a question to the person.
  - `stopped` after `team stop`, or when the supervisor was ended.

  It uses exit statuses, the files a harness writes and what the room
  shows. It does not read a harness's screen, and it never acts on what
  members write.
- **`team status`** lists the roster: label, harness, guard pid, state,
  the last room message and the elapsed time.
- **`team stop [LABEL]`** asks the live supervisor, through a request file
  in the state directory that only the supervisor answers, to end one
  member or all of them. If no supervisor answers within three seconds,
  it says the run is not live, marks the roster ended and signals
  nothing. It never signals a pid read from disk.
- **`team logs LABEL [-n N]`** prints a member's output and error logs.
- **`team report`** writes `report.json` and `report.md` in the state
  directory. For each member it gives the wall-clock time, how long the
  tool server took to be seen connected, messages sent, replies that
  reached more than one inbox, questions to the person, refusals by code
  and empty inbox polls (both counted from the harness's log), the exit
  state and whether its last report began `DONE:` or `BLOCKED:`. For the
  run it counts messages from the lead, the watcher and the person, and
  requests waiting for the person. The supervisor writes the report when
  the last member finishes.

## Readiness

a2p2 has no per-launch readiness event, so `started` is inferred from
what `a2p2 status` shows. A member is ready when its label is registered
under the harness's own name (`claude`, `codex`, `agy`, or `mcp_harness`
for a command), with the member's working directory of this run, and
with a last-seen time at or after the launch time, which is taken before
the process starts. The launcher registers each label earlier through the
CLI as `cli`; a CLI call keeps an earlier harness name and directory, so
a label left from another run or directory does not count. A process
that registers the same label with the same harness name from the same
directory would still pass; the launcher cannot tell it apart.

## Limits and what is guaranteed

- Limits (`limit_seconds`, `silence_seconds`, `poll_seconds`,
  `max_budget_usd`) must be finite numbers above zero.
- While the supervisor runs, a member that passes `limit_seconds` gets
  SIGTERM to its process group within a tenth of a second on the
  supervisor's monotonic clock, and SIGKILL five seconds later. Room
  polling and slow or blocked a2p2 calls do not delay this; they only
  delay the room messages.
- Processes that leave the member's process group (for example with
  `setsid`) are not ended by the launcher.
- If the supervisor is killed, guards end their groups within about a
  fifth of a second, plus the five-second grace. If a guard is killed too,
  or the machine reboots, nothing is ended by the launcher, and `team
  stop` will not signal anything; check for leftover processes yourself.
- `max_budget_usd` is Claude Code's own limit. Codex and Antigravity have
  only the wall-clock limit here.

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
under the state directory). `git worktree add` also writes metadata into
the source repository's `.git/worktrees`; remove the worktree afterwards
with `git worktree remove`. Without either, the member gets an empty
directory. Unknown keys, and keys that do not apply to the member's
harness, are refused. Per harness:

| Harness | How it is started | Profile keys |
|---|---|---|
| `claude` | `claude -p <prompt> --model M --no-session-persistence --mcp-config <file> --strict-mcp-config --allowedTools 'mcp__a2p2__*' --output-format stream-json --verbose` | `allowed_tools`, `permission_mode`, `max_budget_usd` |
| `codex` | `codex exec <prompt> -m M -s S --ephemeral --skip-git-repo-check -C <dir> -c mcp_servers.a2p2...` with `default_tools_approval_mode="approve"`, `--json -o <final>` | `sandbox` (default `read-only`) |
| `agy` | `agy --settings '{"permissions":{"allow":["mcp(a2p2/*)"]}}' --model M --print-timeout <limit>s -p=<prompt>` from a directory with `.agents/mcp_config.json` | none |
| `command` | `command`, a list of arguments with `{a2p2}`, `{home}`, `{label}`, `{room}`, `{workdir}`, `{final}`, `{prompt_file}` and `{team_dir}` filled in | `mcp_harness`: the `--harness` name its MCP server registers with |

Only profiles that keep the harness's own permission and sandbox
controls are accepted; anything else is refused with the allowed values:

- Claude Code `permission_mode`: `default`, `acceptEdits` or `plan`
  (`bypassPermissions` and others are refused). `allowed_tools` may not
  name `*`, unscoped `Bash` or `Bash(*)`; name scoped tools such as
  `Read` or `Bash(git status:*)`.
- Codex `sandbox`: `read-only` (the default) or `workspace-write`
  (`danger-full-access` is refused).
- Antigravity has no profile key; its MCP tools are allowed for the run.
- A `command` member runs exactly what the manifest says. It is trusted
  input: the person who writes the manifest vouches for it, as for any
  program they run.

The launcher writes a harness's per-run file only where nothing exists
yet: if the member's directory already has `.agents/mcp_config.json`
(for example from a checked-out worktree), or it or `.agents` is a
symlink, `up` is refused and the file is left as it was.

The state directory holds `roster.json`, `supervisor.log`, a `control`
directory for stop requests, the reports, and per member `launch.json`,
`prompt.md`, `out.log`, `err.log` and `final.txt`.

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
in 25 and 15 seconds. [examples/run-2026-11-24-readme](examples/run-2026-11-24-readme/README.md)
is a four-member review of README.md with Codex CLI, Antigravity CLI and
Claude Code, and what its report missed.

## What it does not do

- It does not decide who does what, merge work, review it or retry a
  failed member. Those are the lead's job.
- It does not wake an idle member or pass it messages; members read the
  room with `inbox`.
- It does not approve joins, turn networking on or answer the person's
  questions; those stay with the person.
- It does not survive a reboot, and it is not tested on Linux with real
  harnesses.
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
never connects, only refreshes its label with the CLI, asks the person or
posts text that looks like commands. The tests put stub `claude`, `codex`
and `agy` commands first on the `PATH`, so no model is ever called.
