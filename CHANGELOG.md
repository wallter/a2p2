# Changelog

Notable changes to a2p2, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); while the major
version is 0, a minor release may change behaviour or formats, and says so
here. How a release is made is in [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Added

- Release binaries carry a GitHub artifact attestation (build provenance)
  made by the release workflow, checked by `make verify-release`. Check one
  with `gh attestation verify a2p2_<os>_<arch> --repo wallter/a2p2
  --signer-workflow wallter/a2p2/.github/workflows/release.yml
  --source-ref refs/tags/<tag>`. Releases up to 0.2.3 have none, and the
  binaries are still not code-signed.

### Changed

- **Docs: the durability limit on macOS is stated.** `send` survives a crash
  of a2p2 or of the operating system, but on macOS a sudden power loss can
  lose the last few sent messages, because the database's sync does not
  flush the drive's cache there. REQUIREMENTS D1 and section 6 now say so.
  Nothing in the program changed.
- **The MCP tool descriptions are clearer, and the server sends short
  instructions.** The seven descriptions now say that messages reach other
  agents and people, that `whoami` comes first, what `inbox` also does
  (`level`, `fetch`), and when to use `reply`, `ack`, `send` and
  `ask_human`; "mail" became "message". Some parameter descriptions and the
  `whoami` result's note were reworded too. The initialize and discover
  results carry an `instructions` text of 276 bytes (D54). No tool,
  parameter name, schema or annotation changed; the definitions are 3,991
  bytes (was 3,979), under the 4,000 limit.

### Fixed

- **A send no longer slows down as a room's history grows, and an inbox
  much less.** Looking up what refers to a message, its replies, its
  thread and a new record's dependants read every record of the room;
  they now read only the records that match (D56). Measured on one laptop
  with a real daemon (`make bench-local`, one run each; the "before" run
  overlapped other work on the machine): a send's p50 was 4.1 ms at
  10,000 messages and 10.8 ms at 30,000, now 0.26 and 0.27 ms. An agent's
  first inbox page of 50 plus its Seen, with the whole history unread,
  took 2.1 s at 1,000 messages and 75 s at 10,000, now 73 ms and 0.40 s;
  that still grows with the unread backlog. Results and their order are
  unchanged, and the database schema is not touched, so 0.2.0 to 0.2.3
  can still open the database.

### Security

- **One admin can no longer block its own removal by flooding requests.**
  Since 0.1.0, an admin of a room (for example the admin of another owner
  in a shared room) could sign many entries that wait for a second admin.
  They filled the 32-request queue of the other admins, so a removal of
  that admin could not be proposed, and an entry that arrived while the
  queue was full was never raised, even after a restart. Now a machine
  asks its person about at most 32 waiting entries per room, and one
  signing admin gets more than 4 only from places no other admin's 4
  need. The person's own requests have a separate budget, the first
  removal of an admin per signer and target passes both budgets and takes
  no place in them, a request whose entry no longer waits is closed, and
  an entry skipped while a budget was full is raised once there is a
  place and at start.
- **One room's stored log can no longer stop the daemon starting.** Since
  0.1.0, a start failed for every room if one room's stored log did not
  parse or replayed with a different reason text, so a stricter receipt
  rule in a later release, or an entry a hostile member got stored, could
  stop the daemon after an upgrade. Stored entries are now checked for
  form and signature only, the replay compares dispositions only, and a
  batch that drops entries over a bound is decided again without them, so
  delivered entries cannot make a room fail its own replay. A room whose
  log still does not load is set aside: not served or pulled; sends,
  Seen and a change of its files setting refused; nothing deleted; shown
  by `a2p2 status` and `a2p2 doctor` with what to do. It can still be
  read, except streams without a cursor, undecided text stays undecided, and only retention deletes its
  text and files. It may be left and joined again, except the local room,
  for which the fix is the older version. The other rooms work as before.
- **Leaving a room with many files no longer stalls the other rooms.**
  Since 0.2.0 the file deletions of a left room ran while the rooms lock
  was held, about 4 ms per file, so pulls and approvals of every room
  waited. They now run after the lock is released. Other file work still
  waits for them.
- **A room label from another machine can no longer disguise what a person
  approves.** Exposed since 0.1.0: an admin of a room whose creator was
  hostile. The creator could put a carriage return, terminal escapes, a
  right-to-left override or a newline in the room's label, and the label
  went raw into the text of every membership request and its alert, in
  `confirm`, `alerts` and on the page. A seconding request could so hide
  that it makes the creator's second machine an admin. The request text
  now writes each such character as `\u{...}`; the stored label is
  unchanged, so old logs still load. The alert that this machine may have
  been removed from a room, and the join preview in `a2p2 join`, escape
  the label too.
- **The escaping covers characters that draw as nothing.** Zero width
  characters, tag characters and the rest of U+E0000 to U+E0FFF,
  variation selectors, U+FFF0 to U+FFF8, Hangul fillers, the line and
  paragraph separators and every other Unicode control, format or
  default ignorable character are now written `\u{...}` too, in requests
  and on the page. The helper moved to `internal/node/escape.go` with one
  rule for message text, used without line breaks for one line. It keeps
  a joiner or selector inside an emoji sequence or between letters of a
  script that needs joiners, such as Arabic or Devanagari, and the few
  other marks listed below.
- **The page escapes what other machines wrote.** Exposed since 0.2.3,
  the first release whose page loads: a person using the page in a room
  with another owner's machines. Room labels in the sidebar, headings and
  alerts, machine names, agent ids, message text and choices were drawn
  as sent. They are now escaped as the daemon does, and two rooms whose
  labels are drawn alike are shown with their ids. A message that asks
  the person is drawn under its own heading, apart from requests, says
  who asked from which machine and of whom, says it approves nothing,
  and has plain buttons. A choice that its button cannot show exactly as
  it is sent (a hidden character, a line break, a blank other than a
  plain space, a space at either end or two in a row) is not sent; the
  person types the reply. An emoji choice such as "❤️ Yes" is sent.
- **A seconding request says who chose the machine's name.** Since 0.1.0
  `confirm` (and the page, since it loads in 0.2.3) said "the machine
  chose its own name" for every request, also when the other admin wrote
  it. They now say which: the
  request has `name_from` and `recorded_name`, and its text shows the
  name the room records and other members with the name shown. An
  approval from the page is refused if that text changed before the
  decision; reload and decide again.
- **Honest text is shown as written, and the CLI and MCP tools escape
  too.** The first escaping wrote emoji with a selector or joiner and
  Persian or Indic joiners in room labels, file names and approval text
  as `\u{...}`, and in message text right-to-left marks, Thai word
  breaks, the flags of England, Scotland and Wales, Japanese variation
  sequences and a CR before each line end. These now show as written
  where they change no order. A typed `\u{202e}` is shown as
  `\u{5c}u{202e}`, apart from an escaped character. In the text a
  person approves, the room label and names are isolated so
  right-to-left text cannot reorder the words around them; `confirm`
  puts such a label on its own line. The CLI's messages, alerts, room
  list, status, doctor and `net check`, the question alert, and the text
  of MCP tool results now escape what other machines wrote. In
  `net check` that covers members' addresses, which another member can
  set to any text, the probe details that quote them, what members saw,
  their errors and the router's answers. Status, `confirm`, `join`,
  `fetch`, doctor and error lines escape the remote strings they show
  too. Two room labels that differ only in a no-break, ideographic or
  other Unicode space now get their ids on the page and in status.
  `--json` prints the stored strings. `a2p2 confirm` sends the text it showed,
  and the daemon decides nothing if the request changed since (the
  local API's `confirm` takes an optional `text`).
- **One host that reaches a listening machine can no longer lock its
  members out.** Exposed: every machine with networking `on`, since
  0.1.0, to any host that can reach its peer port (the local network,
  and the internet after `net on --map-port` or on a global IPv6
  address). One host could hold all 256 inbound connections, with raw TCP
  connections or with TLS connections under keys it made up, so members
  could not pull from that machine, fetch its files or join through it.
  Keys that share no room could also fill the per-key rate tables, and
  then a member's new key was refused. Now a connection is placed in a
  pool only once its handshake has proved the client's key: 256 for
  members of a room held here (16 for one key), 32 for other keys, and 64
  for connections still in the TLS handshake, which is cut at 5 s. One
  IPv4 address or IPv6 /64 gets at most 8 connections outside the
  members' pool, a /56 16 and a /48 32; a connection with no slot is
  closed at once. The handshake and the membership lookup after it end
  at 5 s. Membership is checked again for each request. A key that
  shares no room gets one request at a time on a connection, every
  answer to it closes the connection, and its rate buckets are its own on
  every route. An attacker with more source groups still can do part of
  this: 4 groups (4 IPv4 addresses, 2 IPv6 /56s or one /48) keep joiners
  out, and 8 groups (8 IPv4 addresses, 4 /56s or 2 /48s) keep members from
  opening new connections, except members whose IPv4 address or IPv6 /64
  completed a member's handshake here in the last 24 hours: they have 16
  handshake slots of their own, at most 4 for one address, which no
  other address or /64 can take. Connections members already have keep
  working (SECURITY.md).
- **One IPv6 /64 can no longer refuse every joiner.** Exposed: admin
  machines with networking `on`, since 0.1.0. Join limits counted each
  full address, so one /64 filled the table of 4,096 sources and every
  other joiner was refused for a minute; and 60 polls a minute naming
  unknown invites, from anyone, refused every joiner that resumed by the
  invite's hash. Now the join route counts per address, and per IPv6 /64
  at 4 times as much: 80 bodies read a minute, counted before a body is
  read, then 10 opens and accepts and 30 polls. Polls naming unknown
  invites are counted per IPv4 address or IPv6 /64 (10 a minute), after
  the lookup, so a joiner resuming by its invite's hash is never refused
  by them. A joiner sends an accept answered 429 or 503 again after the
  stated delay, instead of reporting an unexpected answer. Tables of
  sources forget the one heard from least recently in constant time.
- **Refused peer requests no longer grow `daemon.log` at the default
  level.** Exposed: every listening machine since 0.1.0. Each refused
  request was one Info line, so a flood of refusals grew the log without
  bound. Refusals of keys that share no room, and refused connections,
  are now logged at Debug, with one Info line a minute about refused
  connections; a member's refusals reach Info 5 at once, then one per
  10 s per key. At Debug each refusal is still a line, and the log file
  is still not rotated.
- **`a2p2 install codex` no longer approves every a2p2 tool without
  asking.** Since 0.1.0 it added `default_tools_approval_mode = "approve"`
  to `[mcp_servers.a2p2]` in `~/.codex/config.toml`, so a Codex agent
  could `send`, `reply` or `ask_human` without a prompt when a member of a
  shared room, another owner's included, asked it to post something. Now
  install approves nothing unless you pass `--approve-tools`, and the
  dry run says which you chose. Reinstall to remove the old line;
  `a2p2 doctor` fails a `codex tool approval` line until you do. A headless
  `codex exec` needs `--approve-tools` (D60). The edits and
  doctor follow `CODEX_HOME`, as the Codex CLI does.
- **The team launcher asks before it approves a2p2's tools for members.**
  Since 0.1.0 `scripts/team` approved every a2p2 tool for each Claude Code,
  Codex and Antigravity member. A manifest with such members must now say
  `"approve_a2p2_tools": true` for `up`; `status`, `stop`, `logs` and
  `report` still work for runs started without it. Claude Code members
  get the seven tools by name instead of `mcp__a2p2__*`, and the state
  directory is now 0700.
- **Pi's skill no longer puts message text in double quotes.** Since 0.1.0
  the skill told the model to run `a2p2 send ... "text"` in bash, where
  `$(...)` and backticks in text it copied from a peer would run as you.
  It now sends text through a heredoc with a quoted end word and writes
  the `--as` label out in every command, since an empty `--as` acts as
  the person. The end word has random digits chosen at install. Run
  `a2p2 install pi` again to get the new skill; `a2p2 doctor` fails the
  old one.
- **Install refuses a binary that another user could replace.** A binary
  in a temporary directory, in Go's build cache (`go run`), in a directory
  every user can change, owned by another user, or with a control
  character in its path is refused, whether the path is a symlink or
  leads through one; a group-writable directory gives a
  warning, and `a2p2 doctor` reports the same. Directories install
  creates for a harness's configuration are now 0700. On Windows the
  advice no longer suggests `C:\a2p2`, which every signed-in user can
  change by default; the binary's access list is not checked there yet.
- Codex `config.toml` edits now skip multi-line strings and arrays and
  touch only a2p2's tables, so text that reads like a2p2's table is not
  written into or removed. `--approve-tools` adds no second
  `default_tools_approval_mode` to a table that has one, in any spelling
  TOML allows, and adds nothing, telling you the line to add, when a line
  that could set it is one a2p2 does not read in full.
- Install and uninstall now write a hook or configuration file only if it
  still holds what they read, checked just before the rename; otherwise
  they leave it and say which file changed. Before, only Codex's
  `config.toml` was checked.
- **The release workflow pins its actions and keeps write scopes away
  from the build.** Since v0.1.0 the release job ran actions named by
  moving tags (`actions/checkout@v4`, `actions/setup-go@v5`) while it held
  `contents: write`, and its checkout kept the token for later steps;
  after 0.2.3 it also held the signing scopes for attestations. Anyone who
  could move one of those tags could change a published binary. Now every
  action is pinned to a commit, no checkout keeps its token, and the
  write, signing and attestation scopes sit in a publish job that checks
  out nothing and runs no repository code. That job publishes only the six
  binaries, with exactly one `SHA256SUMS` line each. `make check` refuses
  a workflow that undoes this, and workflow syntax its line-based check
  does not follow. The new form runs for the first time at the next tag.
- **The installers can require the build attestation.** Since v0.2.2
  `install.sh` and `install.ps1` check the binary against a `SHA256SUMS`
  from the same release, which does not stop someone who can replace
  both. With `A2P2_VERIFY_ATTESTATION=1` they also run `gh attestation
  verify` and install nothing without a valid attestation from this
  repository's release workflow for the tag they download (releases after
  0.2.3), so an attested build of an older tag is refused too. For
  `latest` they ask `gh` for the tag once. Without the setting they now
  say the checksum does not show who built the file.
  `make verify-release` now checks every binary of a release against the
  tag, not only the one for the machine that runs it.
- **The installers refuse more directories another user can change.**
  Since v0.2.3 the Windows notes suggested `C:\a2p2`, which every local
  user can write when it is made by hand, so another user could replace
  `a2p2.exe` and run code as you through a hook. `install.ps1` now makes
  a missing directory with an access list for you, SYSTEM and
  Administrators only, and refuses a junction or symbolic link in the
  path, a directory owned by another account, and one another account
  may change (not yet run on Windows). `install.sh` refuses a directory
  that every user may write without the sticky bit, one a group with
  another member than you and root may write, one owned by another user,
  a symbolic link owned by another user, and the same above it, along the
  path as written (trailing slashes taken off) and the real path a link
  leads to. A group of only you and root, such as `admin` for a Homebrew
  prefix on a Mac with one person, is accepted; a macOS group that holds
  accounts it does not list, such as `everyone`, is refused; a group
  whose members it cannot list gets a note, not a refusal. It makes missing
  directories with mode 0755 and checks again after. The exact cases are
  in [SECURITY.md](SECURITY.md#release-binaries).
- A test now opens databases written by each release from v0.2.0 to
  v0.2.3 with the current code, so a rule that would refuse old data
  fails it.
- **An agent with a shell no longer acts as the person by accident.**
  Since 0.1.0 the CLI acted as `human@<machine id>` whenever `--as` was
  left out or empty, with no check for a person. Any agent that ran
  `a2p2` in its shell, or one told to by a room message, could answer
  the person's questions as the person and mark them seen, so they left
  the person's list. Now an empty `--as` is an error. In a shell that
  Claude Code, Codex or Gemini CLI marks as its agent's, or with
  `AI_AGENT` set, the CLI acts as that agent. Elsewhere `send`, `ack`
  and an `inbox` that marks messages seen act as the person only at a
  terminal or with the new `--person` flag. `a2p2 ui`, whose address
  lets the page act as the person, is refused in an agent's shell and
  away from a terminal unless `--person` is given. The daemon log
  records `--person` used away from a terminal or in an agent's shell.
  Scripts that read or sent as the person, or ran `ui`, without a
  terminal must add `--person` or name themselves with `--as`. This is a
  guard rail: any program running as you can still act as you.
- **`room leave` asks first.** Since 0.1.0 it deleted the room's data at
  once with no terminal check. It now asks at a terminal for a typed
  `yes`, shows the room's id and leaves that id, and says when this
  machine is the room's only admin or member. A room it cannot look up
  is not left.
- **`status --json` leaves out the page's address.** Since 0.1.0 it
  printed the address with the page's access token, and agents run it.
  `ui --print` gives the address to a person who asks.
- `net on` now says that every member and invite holder gets the
  addresses it advertises, which can include VPN and office addresses,
  and lists the `advertise` addresses of `config.json` when they are
  set.


## [0.2.3] - 2026-11-25

The local web page loads. It did not in any earlier release: `a2p2 ui`
showed "Could not load page data". Nothing changes on the wire; 0.2.0 to
0.2.3 machines work together.

Known limits: the limits of 0.2.2 still apply. The page is tested by
running its script in Node, not in a browser; three smaller page problems
are open as issue 8. `install.ps1` has not been run on Windows. A run
between two physical machines has still not been recorded.

### Fixed

- **The web page did not load in 0.1.0, 0.2.0, 0.2.1 and 0.2.2.** `a2p2 ui`
  opened a page that showed "Could not load page data" and never drew the
  sidebar, because a name declared later in `renderSide` hid the room-label
  function the same function called first
  (`Cannot access 'roomLabel' before initialization`). The command line
  and the agents were not affected. The inner name is renamed and the page
  behaves as intended. When `node` is installed, `make check` now
  runs the page's script in Node against the real server (step `web-js`)
  and fails on this kind of error. Without `node` the step is printed as
  `skip` and counted in the last line as skipped, and `make prerelease`
  fails. It is not run on Windows. The README had said
  the page was checked in headless Chrome; that claim is replaced by what
  is tested now.

- **A message addressed to one author could be sent to the whole room.**
  The page rebuilt the recipient list from the authors in the loaded
  history; if the chosen author dropped out of it, the choice was lost and
  the message went to everyone. A chosen recipient now stays in the list,
  and a draft whose recipient is gone is refused with a message. This was
  there before 0.2.3 but unreachable while the page did not load.

### Added

- **`a2p2 ui --print`** prints the page's address and nothing else, and opens no browser;
  it is refused together with `--json`. The
  address carries the page's access token, so treat it like a password
  and keep it out of logs.

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
- **Release notes begin with a summary and how to install.** The summary
  is the first paragraph of the version's section; the install block is in
  `scripts/release-notes-head.md`, used by the release workflow and by
  `make verify-release`. A release is titled `a2p2 <tag>`.
- **A way to install from a fixed version.** [docs/INSTALL.md](docs/INSTALL.md)
  shows how to take `install.sh` from a release tag, not from `main`, and
  says the binaries are not signed.

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
