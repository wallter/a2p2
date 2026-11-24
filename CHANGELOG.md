# Changelog

Notable changes to a2p2, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); while the major
version is 0, a minor release may change behaviour or formats, and says so
here. How a release is made is in [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Added

- **Windows support, built but not yet run there.** The daemon and the CLI
  no longer refuse to start on Windows 10 (1803 or later) and 11. The
  home is created for your user only and refused if others can open it,
  and every local API call carries a key the daemon writes into the home
  at each start (D47). Releases include `a2p2_windows_amd64.exe`
  and `a2p2_windows_arm64.exe`. On Windows, `a2p2 install` refuses a
  binary path with spaces for the harnesses whose hooks run it in a
  shell, and refuses `agy`. Nothing changes on macOS or Linux.

## [0.1.0] - 2026-11-24

The first release, a preview: read the limits and the status in the
[README](README.md) before relying on it.

### Added

- **Messaging between agents and people without a server.** One daemon per
  machine; private rooms joined by a one-time invite that a person
  approves on both machines; every member keeps a signed copy of the room
  and machines pull from each other over pinned mutual TLS. A third member
  carries messages between two machines that are never online together,
  and a machine that can only make outgoing connections takes part fully
  if it can reach a member that others can also reach.
- **Seven MCP tools for agents** (`whoami`, `peers`, `send`, `inbox`,
  `reply`, `ack`, `ask_human`), with definitions under 1,000 tokens, plus a
  CLI and a local web page for the person.
- **Threads, handled reports, replace and withdraw.** Replies reach
  everyone who has posted in the thread; a message can be reported handled
  once its text was shown; each member's daemon deletes withdrawn text
  when the withdrawal reaches it. Copies already received elsewhere cannot
  be recalled.
- **Per-agent levels.** Each agent follows a room at `all`, `addressed` or
  `none`, can read what a level kept out, and is reached at `addressed` by
  a message that begins with `@everyone`.
- **Several admins per room.** Membership changes are signed entries that a
  person confirms; with three admins a room survives the loss or theft of
  one admin key.
- **Installers** for Claude Code, Codex CLI, Antigravity CLI, OpenCode,
  Hermes Agent and Pi (`a2p2 install <harness>`, with `--dry-run`,
  `uninstall` and `doctor`). Flags may come before or after the harness
  name.
- **Recipes and a skill** (`recipes/`, `skills/a2p2/SKILL.md`): optional
  conventions for delegating, handing off, claiming and reviewing work.
- **A team launcher** (`scripts/team`): start, watch and stop a team of
  Claude Code, Codex CLI and Antigravity CLI sessions on one room, and
  write a report on the run. A layer on top; nothing in the
  daemon knows about it.
- **Fault tests with real daemons in containers** (`make chaos`).

### Known limits

- Designed for rooms of up to 64 machines; text only (about 16 KiB a
  message) that expires after 30 days; no attachments; macOS and Linux
  only (Windows builds but does not run).
- Not yet verified on the current delivery code: interactive sessions,
  hooks inside a real session, and two physical machines on a network.
- The full list is in [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md),
  section 6.

[Unreleased]: https://github.com/wallter/a2p2/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/wallter/a2p2/releases/tag/v0.1.0
