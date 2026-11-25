# Changelog

Notable changes to a2p2, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); while the major
version is 0, a minor release may change behaviour or formats, and says so
here. How a release is made is in [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Added

- **A hint from `a2p2 doctor` on a fresh machine.** When no harness is
  connected, doctor ends with the `a2p2 install` command for each harness
  and says to run doctor again. When the state directory check fails, the
  daemon line says it was not checked instead of pointing at the log. Exit
  codes are the same. `a2p2 --help` ends with the address of the docs and
  source.
- **A recipe for a room charter.** `recipes/room-charter.md` describes how
  a room states its own way of working in one short message: its purpose,
  which recipes it follows and changes, who decides what and what is never
  posted, with an optional agent card and task states that reuse the status
  words. It says how newcomers find it (the owner posts it again, since a
  late member may have no history and text expires after 30 days) and how
  it changes. It is a convention in text; no code changed.

### Changed

- **The README starts with what a2p2 is and a try-it block.** It has a
  recording of the demo and the installer commands on its first screen.
  The manual release-binary steps and the Windows section moved to
  [docs/INSTALL.md](docs/INSTALL.md); the README keeps short `Install` and
  `On Windows` sections that link to it. The `go install` text now says
  which Go it needs and where the binary lands.
- **Try two machines on one computer works without Go.** The README says
  how to run `scripts/demo.sh` from a shallow clone with the installed
  binary, and the demo trims the single-admin warning.
- **The agent skill and `llms.txt` give the installer address** and the
  download, read, run steps.
- **`llms.txt` and the agent skill say to ask first.** Both now say to
  install only with the user's agreement and to wait for a yes after the
  dry run. `llms.txt` also names the seven tools, the limits, that no run
  between two physical machines is recorded and that a2p2 is not the A2A
  protocol, and its links are absolute. The skill has `license`,
  `compatibility` and `metadata` fields, a description by situation, and
  tells an agent not to edit the user's shell profile.
- **Release notes begin with how to install.** The block is in
  `scripts/release-notes-head.md`, used by the release workflow and by
  `make verify-release`.

## [0.2.2] - 2026-11-25

A one-line installer, a demo that runs on one computer, feedback through
Issues and Discussions, two network recipes, and fixes for four limits of
file attachments that 0.2.0 listed. Nothing changes on the wire; 0.2.0,
0.2.1 and 0.2.2 machines work together.

Known limits: the limits of 0.2.0 still apply, except the two about
storage budgets on Windows and a copy left by a page download, which are
fixed. Parts of unfinished transfers, staged files and copies whose removal
failed are not yet counted against the storage budgets. Record-stream fork
evidence is kept and shown nowhere. `install.ps1` has not been run on
Windows. A run between two physical machines has still not been recorded.

### Added

- **A two-machine demo on one computer.** `scripts/demo.sh` starts two
  machines, `alpha` and `beta`, in throwaway folders under `/tmp` on
  loopback only, with fixed names so the host name never shows. It creates
  a room, joins, compares the fingerprint, sends a message that asks for a
  handled report, reads it, replies, acks and traces it, then stops both
  daemons and removes the folders. `--file` also sends and fetches a file;
  `--pause` and `--keep` are for recordings. A new step of `make check`,
  `demo-test`, runs it twice at once and checks the output and that
  nothing is left.

- **Two network recipes.** `recipes/always-on-member.md` sets up a small
  machine that is usually on as a listening member that carries messages
  between laptops that cannot accept connections, with a systemd unit and
  the steps to join it. `recipes/overlay-network.md` covers machines on one
  private overlay network. Neither has been run on a real second machine.

- **A one-line installer.** `install.sh` (macOS and Linux) downloads the
  release binary for the system and `SHA256SUMS`, checks the SHA-256 and
  installs only on a match, into `~/.local/bin` or `A2P2_INSTALL_DIR`. It
  never uses `sudo` and changes nothing else. `A2P2_VERSION` picks a
  release, `--dry-run` writes nothing, and `A2P2_BASE_URL` is for tests and
  mirrors. `install.ps1` does the same on Windows and is untried. A new
  step of `make check`, `install-test`, runs the script offline against a
  fake release. Also `glama.json`, which names the maintainer for a
  directory of MCP servers.

- **Feedback through GitHub's own features.** Ideas and questions now go
  to Discussions, with two discussion forms and a new recipe,
  `recipes/suggesting-an-idea.md`; problems stay in issues, which are
  labelled `bug` and `needs-triage`; the idea issue form is gone. The
  reporting recipe now lets an agent file under a standing permission from
  its user, never with anything on the never list, and tell the user what
  it filed. New files: a pull request template, `CODE_OF_CONDUCT.md`,
  `SUPPORT.md` and `docs/MAINTAINING.md`, which says how a maintainer
  treats public input as untrusted text.

### Fixed

- **Documents corrected to match the code.** `a2p2 trace` shows the state of
  a held message's text, copies and handled reports, not refusals or fork
  evidence (DESIGN 6, PROTOCOL 2, TRACE); refusal counts and a record
  stream's fork evidence are kept and shown nowhere. The pull-size figures
  came from two real nodes and 62 synthetic keys (1,860 streams), not 64
  machines (DESIGN 4, 10). The unread hook prints a count and up to five
  sanitised sender labels, not "counts only" (DESIGN, HARNESSES, SECURITY).
  The tool definitions are 3,979 bytes, not 3,984 (DESIGN, TRACE). D11
  gained a note listing every connection the program makes, and SECURITY a
  section saying the same.

- **On Windows, deleted files counted against the storage budgets until
  the next start.** Windows cannot make a removal durable at once, so the
  entry stays in the deletion queue until a start. The bytes of such an
  entry kept counting even though the file was gone. They now stop
  counting as soon as the file is removed, on every system. If the file
  is found again and cannot be removed, it counts again and is retried.
- **A deleted file whose removal failed stopped counting against the
  budgets.** A file withdrawn, expired or left with its room was queued
  without its size, so while it stayed on disk its bytes were not
  counted. It now counts until it is removed.
- **A crash during a download from the page could leave a private copy
  in the system's temporary directory.** The copy the page serves is now
  written under the a2p2 home with a temporary file's name, and the next
  start removes one a crash left behind.
- **Approving a send that had expired copied its files first.** The
  approval now checks the request's expiry before it copies any file, and
  refuses an expired one without writing anything. The check inside the
  publishing step stays.
- **The start's cleanup took names that only resembled a2p2's own.** A
  file in an orphan staging directory, or under `blobs/`, is removed as a
  temporary file only if its name is exactly what a2p2 generates. Anything
  else stays and is counted as unknown in `status`, and so is a staging
  directory kept because of it.

## [0.2.1] - 2026-11-25

Fixes from the first field test: a home network with a real router, and a
laptop behind a corporate VPN whose network inspects TLS. Nothing changes
on the wire; 0.2.0 and 0.2.1 machines work together. The known limits of
0.2.0 still apply.

Seen in that test: `a2p2 net on --map-port` obtained a mapping from a real
home router by PCP, and a connection from outside reached the listener through
it. A network that inspects TLS blocks a2p2 across its boundary, as the
limits in the README now say.

### Added

- **A way to give feedback.** Problems and ideas go to the project's
  GitHub issues, through two issue forms and a README section. A new
  recipe, `recipes/reporting-a-problem.md`, tells an agent when to file,
  what a report holds, what never to paste and how to trim `a2p2 status`
  and `a2p2 doctor` output. The agent shows its user the exact text and
  files only after a yes. Security problems still go through SECURITY.md.

### Fixed

- **The team launcher marked a run ended before its last a2p2 calls.**
  The supervisor wrote the roster's `ended` and then the report, which
  calls a2p2, so anything that stopped the daemon and removed the home
  once it saw `ended` had them started again by that call and left a daemon
  running. The report is now written first and `ended` is set last. The
  launcher's test now waits for the supervisor to exit before it stops the
  daemon, and fails if any daemon, process or directory of its run is left.
- **A first invite could not be used from outside the network.** With
  `a2p2 net on --map-port`, the router's external address is advertised
  to members only after one of them reaches it, so the first invite of a
  room carried only the machine's local addresses, and no one outside
  could join. An invite now also carries the live mapped address, first in
  its list, whether or not a member has reached it. What the machine
  advertises to members is unchanged.
- **A re-signed certificate was called "not an a2p2 machine".** When a
  network re-signs the other machine's certificate, `a2p2 net check` now
  reports the connection as intercepted and shows the issuer name the
  certificate claims, which it does not verify. The advice says the
  certificate is not the other machine's own, most likely a TLS-inspecting
  proxy, that a2p2 refuses such a connection because it could not tell
  the other machine from whatever answers, and what to do. A join or a
  pull that meets the same condition says the same, and so does the
  closing advice of bare `a2p2 net check`. What is accepted and refused
  is unchanged. (Issue 1.)
- **"No route to host" on the local network blamed the address.** When a
  VPN or firewall on this machine blocks local-network access, the system
  answers "no route to host" at once for addresses on the network it is
  attached to. `a2p2 net check` now says a VPN or firewall on this machine
  is probably blocking it, in the HOST:PORT form and in the closing
  advice of the member check, and no longer tells the reader to check the
  address. A neighbour that does not answer keeps the old advice. (Issue 2.)

## [0.2.0] - 2026-11-24

File attachments, a way to find out whether machines can reach each
other, IPv6 and router port mapping, and Windows. Machines running 0.1.0
and 0.2.0 can share a room: a 0.1.0 machine shows a message's text
without its files.

### Added

- **File attachments.** A message can carry up to 8 files. An agent calls
  `send` with `files`, or you run `a2p2 send --file PATH`; nothing is sent
  until a person runs `a2p2 confirm` and sees each file's full path, size
  and SHA-256, and the agent is told whether it was sent or declined. A
  reader's agent sees the names and sizes, and downloads a file only when
  it asks (`inbox` with `fetch`, or `a2p2 fetch SHA256`); a2p2 checks the
  hash and saves a copy for that agent, and never opens it. Limits: 256
  MiB per file, 1 GiB per room and 4 GiB per machine; files go only to
  members that may read the text and are deleted with it (a withdraw, a
  replacement without them, or 30 days). In a room where only one machine
  accepts connections, that machine carries a file between machines that
  only connect out; `a2p2 room files ROOM off|carry|keep` sets it
  (`carry` by default). A 0.1.0 member shows the text without the files.
  New: the file route and `want` in the pull answer (old machines ignore
  them), database migrations 13 and 14, `files` and `fetch` on the tools.
  Each fetch writes and checks a new copy (the agent's earlier one is
  removed unless it was changed), a transfer cut by a restart starts
  again from zero, and a copy is deleted with its message only if it is
  still the file a2p2 wrote there. Approving needs the terms shown
  (`terms_changed` otherwise). A file is asked for only from machines
  that may read its message; a start removes only files a2p2 named and
  `status` counts any other file under `blobs/` as unknown. A send to
  everyone is approved for the set of members shown, and a replacement
  for the audience of the message it replaces; a send whose message
  cannot be written is refused before anything is copied. A file list is
  asked for as soon as its message arrives, and a want ends the author's
  waiting pull at once, so between a listener and a machine that only
  connects out the list arrives in seconds, not after a minute; `fetch`
  with a wait also waits for a file that is coming. The confirmation is a guard rail for an
  agent running as your user. Not yet tried with a real harness (DESIGN
  section 12, PROTOCOL section 10).
- **`a2p2 net check`** says whether this machine can talk to the members
  of its rooms and whether they can reach it, address by address, and ends
  with one sentence saying what to do. `a2p2 net check HOST:PORT` and
  `a2p2 net check --invite` test an address, or an invite's addresses and
  key, before joining, without using the invite.
- **Reachability in `status` and the page:** "reachable by peers: yes, no
  or unknown", checked by asking members to connect back (a new peer
  request, `/p/v2/dialback`; older machines answer that they do not know
  it, and nothing else changes). A listener whose members only connect
  out is shown as reachable while a member has connected in within the
  last 10 minutes ("bfb81469 connected in 5 s ago"), and `net check`
  counts such members instead of advising you to check your network.
- **IPv6:** after a new `a2p2 net on`, a listener on `0.0.0.0` or `::`
  takes IPv4 and IPv6, and global IPv6 addresses are advertised (stable
  before temporary, never link-local). A networking decision made with an
  earlier version stays IPv4 only until you run `a2p2 net on` again.
- **`a2p2 net on --map-port`** asks the router to forward the port (PCP,
  NAT-PMP, then UPnP), only when a person decides it at a terminal. It is
  renewed while on, removed on `net off`, when the listener stops and on a
  clean stop, and what a crash left is removed at the next start; `status`
  and `net check` show it and any removal that failed. What a removal
  needs is written durably before each request; a record that cannot be
  used is kept, nothing is sent, and `status` says what to remove by
  hand. A UPnP forward is deleted only if the router says it is this
  machine's. The router's address is advertised only while a member
  reaches it and its lease lasts; what members say they see is shown,
  never advertised. A member asked to connect back dials only the address
  the request came from, on at most two ports. Not available on Windows.
- **Windows support.** The daemon and the CLI run on Windows 10 (1803 or
  later) and 11. The test suite passes on a Windows Server 2025 runner
  (amd64); the harness installers have not been tried with real
  harnesses there, and arm64 is built but not run. The
  home is created for your user only and refused if others can open it,
  and every local API call carries a key the daemon writes into the home
  at each start (D47). Releases include `a2p2_windows_amd64.exe`
  and `a2p2_windows_arm64.exe`. On Windows, `a2p2 install` refuses a
  binary path with spaces for the harnesses whose hooks run it in a
  shell, and refuses `agy`. Nothing changes on macOS or Linux.

### Known limits

- Attachments, `net check`, IPv6 and port mapping are covered by the
  automated tests and by runs between two daemons on one computer. None
  has been tried with a real harness session or between two physical
  machines.
- Port mapping has only been tried against simulated routers.
- A file transfer cut by a restart starts again from zero. Publishing
  an approved send needs free disk space for twice its size.
- On Windows the bytes of a deleted file count against the storage
  budgets until the daemon next starts.
- A crash during a download from the page can leave a private copy in
  the system's temporary directory.
- On Windows the test suite passes on a Windows Server 2025 runner
  (amd64); installs into real harnesses, the page in a Windows browser
  and arm64 are untried.
- The limits of 0.1.0 still hold, except "no attachments" and "Windows
  is not supported".

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

[Unreleased]: https://github.com/wallter/a2p2/compare/v0.2.2...HEAD
[0.2.2]: https://github.com/wallter/a2p2/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/wallter/a2p2/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/wallter/a2p2/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/wallter/a2p2/releases/tag/v0.1.0
