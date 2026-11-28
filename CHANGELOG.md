# Changelog

Notable changes to a2p2, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); while the major
version is 0, a minor release may change behaviour or formats, and says so
here. How a release is made is in [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Changed

- **Under Claude Code, an unread `inbox` page is no larger in bytes than
  the harness's output token limit, less 1,000, but for one oversized
  entry.** Claude Code saves a
  tool result above that limit (25,000 tokens, or `MAX_MCP_OUTPUT_TOKENS`)
  to a file, which the model may never read, after the page was marked
  seen. No inbox result was measured at more tokens than bytes. So when
  the MCP server was installed for Claude Code (`--harness claude`), a
  `max_bytes` above 24,000, or above the lower limit set, is read at it
  with a `note`, and the default page of 18,000 shrinks to it when the
  limit is lower. A message too large for that size comes cut, as at the
  32,000-byte ceiling, and one whose fields alone pass it comes alone
  without its text; none is held back. The page is still marked seen
  before the harness shows it. Other harnesses, and a daemon from before
  this release (which can still return a page the harness sets aside),
  read as before. Only unread pages are capped; an error now repeats at
  most 200 bytes of any input. A read that sets a level and waits now
  returns an entry that arrived whole, leaving the list of rooms set out
  when it would not fit. No tool definition changes (D74).

### Fixed

- **`a2p2 status` and `a2p2 doctor` say when the running daemon is another
  version than the binary.** After installing a new binary with the old
  daemon still running, `status` showed only the daemon's version. It now
  adds one line naming both versions and `a2p2 stop`. `doctor` fails the
  check "daemon version" when the daemon is older, and warns when it is
  newer or the two cannot be ordered. `send` to several addressees that
  have not registered now prints its stderr sentence once, naming up to
  three and then "and N more", instead of once for each. docs/INSTALL.md
  says to `mv` a new binary into place, not copy over the old one.
- **A message sent to an agent before it registers now reaches it.**
  From 0.1.0 to 0.3.0 a label's first use started its reading at the end
  of every room, so a brief a lead sent to `w1@<machine>` before the
  worker's session existed was never unread for it, while `send` printed
  an id as if it would be read (0.3.0 lists this as a known limit). Now a
  message addressed to an agent is unread when the agent first registers
  if it is among the newest 10,000 records this machine holds at that
  moment, from this machine or another; room-wide history from before
  stays read, as before (`recent` reads it, and an older brief).
  Registration does bounded work, about 40 ms with 100,000 records held.
  A brief replaced since is shown with its newest text, one withdrawn is
  not shown, one whose newest text expired is shown as expired. A brief
  sent before this machine was admitted to the room without history is
  not unread. `send` (and the MCP send, reply and ask_human results) now
  names addressees that have not registered on this machine yet; the CLI
  prints that on stderr. A label registered while the head of this
  machine's admission frontier was not held yet no longer starts below
  that frontier (from 0.1.0 to 0.3.0). An agent registered before this version
  keeps its place. The database gains a column after 0.3.0 (migration
  15): once this version has opened a home, 0.3.0 and earlier refuse it
  ("upgrade a2p2"). To be able to go back, copy the home before upgrading
  ([installing](docs/INSTALL.md#going-back-to-an-older-version)).
- **`a2p2 doctor` no longer calls an outbound-only machine off.** Its
  network line said "off: this machine only" whenever the machine
  listened on no address. It now says "outbound only" when that is what
  runs, as `a2p2 status` does.
- **`a2p2 doctor` names a harness it could not check.** A harness that
  a2p2 installed into was left out of `doctor` without a word when its
  program was not on PATH. It now gets one line saying so.
- **`a2p2 reply` points at the command that exists.** The agent tool is
  called `reply`, so people type it; the CLI now answers that unknown
  command with `a2p2 send --reply-to ID TEXT` instead of only "run a2p2
  help".

## [0.3.0] - 2026-11-27

A security release with breaking changes. It closes ways for other
machines in a shared room, hosts that reach the peer port, devices on
the local network and programs on the same computer to misuse a2p2 (D57
to D69, D71), fixes a delivery delay every earlier release had for a
machine that sends more often than about once a minute, and pages the
MCP `inbox` by size (D70, D72). Nothing changes on the wire or in the
database schema: 0.2.0 to 0.2.3 machines share rooms with this version,
and 0.2.3 opens a database this version wrote. Scripts that use the CLI
as the person, `a2p2 ui`, port mapping, Codex installs, the team
launcher and Pi's skill need action after the upgrade: read "Upgrading
from 0.2.3" below, then run `a2p2 stop` so that the new daemon starts,
and `a2p2 doctor`.

Known limits: the limits of 0.2.3 still apply. These protections hold
only on machines that run this version; a 0.2.3 machine in the same room
still dials what members give it and does not escape remote text. The CLI's
person rule, the file approval and the installers' checks are guard
rails: any program running as your user can still act as you. A member
can still make this machine try a TLS handshake with any host on a
private network or the internet at port 443 or 1024 and above, within
the dial budgets, and an attacker with several source addresses can
still keep joiners or new member connections out of a listener
(SECURITY.md). Lookalike letters in room labels (`lоcal` with a Cyrillic
`о`) are not detected. The changes agents see were tried in one
end-to-end run each in Claude Code, Codex CLI and Antigravity CLI, with
a build before the page sizes of D72; no run has shown a model calling
`inbox` again on `more`. A harness can set a tool result aside in a
file, or cut it, after a2p2 has recorded the page as seen; the model
then never reads messages that count as read. The byte budget makes
this rarer but cannot rule it out (D72). A message addressed to an agent
label before that label first registers on its machine is not shown to
it: a lead should brief workers after they appear in `peers`. The
Windows parts of the installer and staging checks have not been run on
Windows, and the binary's access list is not checked there. A sudden power loss on macOS can lose the last few sent
messages. A run between two physical machines has still not been
recorded.

### Upgrading from 0.2.3

- **Run `a2p2 stop` after you install.** The daemon of the old version
  keeps running until then. With it running, the new MCP server reads the
  inbox as 0.2.3 did and its `note` says to run `a2p2 stop`, and
  `a2p2 ui` opens and prints nothing and says the same.
- **Scripts that use the CLI as the person.** Without `--as`, `send`,
  `ack` and an `inbox` that marks messages seen now act as the person
  only when stdin or stderr is a terminal, and are refused otherwise
  (`inbox --peek` still works). In a shell that Claude Code, Codex or
  Gemini CLI marks as its agent's, or with `AI_AGENT` set, the CLI acts
  as that agent and says so on stderr. An empty `--as` is an error. What
  to do: a script names itself with `--as LABEL`, or adds `--person` if it
  must act as the person; `status` and `doctor` count `--person` used away
  from a terminal or in an agent's shell.
- **`a2p2 ui`.** The address it opens or prints (`--print`) now holds a
  code that works once, within 30 seconds; a saved or bookmarked address
  no longer works. `ui` is refused in an agent's shell and away from a
  terminal unless `--person` is given, and takes no `--json` or `--as`.
  Over SSH it opens nothing and prints the address and the port to
  forward. `status --json` no longer prints the page's address: use
  `a2p2 ui --print`.
- **Port mapping is off after the upgrade.** A `--map-port` decision made
  by 0.2.3 or earlier is not applied: nothing is mapped, what that version
  left on the router is removed, and `status` and `net on` say so. Run
  `a2p2 net on --map-port` again at a terminal to map.
- **Addresses no longer dialled.** An address another machine gave is
  dialled only if it is a LAN, VPN, overlay or public address at port 443
  or 1024 and above, never loopback or link-local. An address 0.2.3
  stored that this rule refuses is kept and not dialled, even one where
  the member's key answered, unless an invite you accepted gave it, and it
  goes once it has failed for a day and 5 tries. `a2p2 status`, `a2p2
  doctor` and `a2p2 net check` show it as "held and not dialled, on
  another machine's word" with why. A listener on a port below 1024 other
  than 443, or on a plain link-local address, is now learned only from an
  invite. This stops this machine dialling a member on the same computer,
  on a direct cable or on such a port, learned from its own requests or
  another member's `peers`, and matters only if this machine does not
  listen or the member cannot reach it: otherwise the member's own
  requests still carry messages both ways. What to do: ask that member's
  person for an invite and run `a2p2 join` with it (a member may join
  again); its address is then dialled as given. Two machines on one
  computer still work through the invite's address: the one that joined
  dials the inviter, and a third reaches the others through the machine
  it joined.
- **A home whose path holds `#`, `?` or `%`.** 0.2.3 and earlier opened
  the database at a wrong path: for `/x/a#b`, the file `/x/a` beside the
  home. If that file is an a2p2 database and the home holds none, the
  daemon refuses to start and names both paths and the `-wal` and `-shm`
  files. What to do: if it is this home's, stop a2p2, move it with its
  `-wal` and `-shm` files to `a2p2.db`, `a2p2.db-wal` and `a2p2.db-shm`
  in the home (which must not exist yet), and start again; the node
  starts with its data. If it is not, move it away from that path.
- **`inbox` comes in pages.** The MCP `inbox` returns at most 18,000
  bytes unless `max_bytes` (512 to 32,000) says otherwise, and `more`
  says how many are still unread, so a large backlog takes more calls (1
  KB messages about fifteen to a call instead of fifty). A `max_bytes`
  above 32,000 is read as 32,000 and the result's `note` says so. An
  agent that stops after one call leaves the rest unread, not lost.
  `a2p2 inbox` prints how many more are unread when its limit left some
  out.
- **Codex and the team launcher no longer approve a2p2's tools.** `a2p2
  install codex` approves nothing unless you pass `--approve-tools`, and
  `a2p2 doctor` fails its `codex tool approval` line until you reinstall,
  which removes the line 0.1.0 to 0.2.3 wrote. A headless `codex exec`
  needs `--approve-tools`. A team launcher manifest with Claude Code, Codex
  or Antigravity members needs `"approve_a2p2_tools": true` for `up`.
- **Pi's skill.** `a2p2 doctor` fails the old one: run `a2p2 install pi`
  again.
- **Install refuses some binaries and homes.** `a2p2 install` refuses a
  binary in a temporary directory, in Go's build cache (`go run`) or in a
  directory another user can change. a2p2 refuses a home under a
  directory another user owns or can write (one only a group you are in
  can write gives a warning). What to do: move the binary
  to a directory of yours (`~/.local/bin`) and install again; move the
  home or fix the directory's owner or mode. The team launcher refuses a
  `state_dir` or `a2p2_home` another user could change.
- **A room set aside.** A room whose stored log does not load in this
  version is no longer fatal: the daemon starts, and `a2p2 status` and
  `a2p2 doctor` show the room as set aside, with what to do. It is not
  served or pulled, and sends, Seen and a change of its files setting are
  refused; its messages can still be read and nothing is deleted. What to
  do: run the version that stored it, or leave the room (`a2p2 room
  leave` asks first) and join it again. For the local room, only the
  older version helps.
- **Rooms that can no longer be joined.** A room labelled `local`, or
  whose label holds control, direction or invisible characters, cannot be
  joined by this version, and rooms cannot be renamed: make it again
  under another label. A join whose preview label differs from the
  room's signed create entry is refused. A join an older version
  recorded under another label is refused, and the room stays. A room
  this machine already holds is joined again only through an admin
  machine that is a member here; if this machine's admission comes after
  1,024 entries (or 4 MiB) of the log that this machine lacks, it pulls
  from a member first, or leaves and joins again.
- **`room leave` needs a terminal.** It asks for a typed `yes` and is
  refused away from a terminal, so a script can no longer leave a room.
- **Machines on 0.2.0 to 0.2.3 in the same room.** They keep working
  with this version: nothing changed on the wire, they see fewer
  requests from it, and it answers theirs as before. They do not get
  this release's protections: they still dial what members give them,
  do not escape remote text, and give their agents none of the new
  marks. Addresses they advertise that the rule refuses are dropped, not
  answered with an error. The team launcher with an older `a2p2` still
  matches the person by short id. Joins between versions are argued from
  the unchanged formats, not run in a test. A 0.2.3 binary opens a
  database this version wrote; after a downgrade, 0.2.3 maps the port
  again under a decision this version recorded.

### Security

Other machines in a shared room:

- **Members no longer choose what this machine dials (D71).** Since
  0.1.0, any member could give addresses for itself or other members
  (loopback ports, link-local and cloud metadata addresses, well-known
  ports, and names resolving to these), and this machine dialled them
  with a TLS handshake for pulls and, since 0.2.0, file transfers and
  `a2p2 net check`; a dial-back went to any port from 1, and to loopback
  when the request came over loopback. Now one rule holds on every path:
  an address on another machine's word is kept and dialled only at port
  443 or 1024 and above, never loopback, link-local, unspecified,
  multicast, broadcast, zoned or an IPv6 form that carries a refused IPv4
  address. A name is looked up once and only the addresses that pass are
  dialled. Addresses from an invite are dialled as given once it is
  accepted (a preview no longer marks them); `a2p2 net check HOST:PORT`
  dials what is typed. Each member gets a share of the unverified dial
  budget, and a name pays for every address it gives, even one that
  answered before. A stored "verified" mark never overrides the rule.
- **A key that is not a member leaves nothing behind on a listener
  (D71).** Since 0.1.0 a pull from any key was recorded in memory and
  waited for the lock all rooms share, so any host reaching the peer port
  could grow the daemon's memory with fresh keys and slow members' pulls.
  Such a request, and one for a room not held here, is now refused before
  the lock and nothing of it is kept. One that carries log entries is
  checked at most 2 at a time, within a budget of 4 a second for the
  machine and one a second for each source address or IPv6 /64. What is
  kept of a peer that is no longer a member goes within 10 minutes, and so
  does a member's stream hint whose report is a day old.
- **A join can no longer write into a room held here from a machine that
  is not a member (D64).** Since 0.1.0 a removed admin, or any machine
  holding a copy of the room, could send a member an invite; once its
  person approved, the answer's entries were applied and the
  `not_caught_up` gate opened, even with no heads. Now, for a room held
  here, the preview must name the create entry and label held here; each
  batch counts only if the admin machine is a member here before or after
  it, also for a join resumed after a restart; and the gate opens only
  for an answer from a member whose heads are exactly the heads of the log
  here. A page that breaks the page limits or does not move on ends the
  join, and so does an answer past 256 pages or 64 MiB, or pages for 15
  minutes, or for 2 minutes without an entry this machine lacks. These
  count over every run of the join until the daemon restarts, and the
  next run goes on from the last page read. Entries kept while the admin
  machine is not yet a member are checked again once a quarter page more
  is kept.
- **A pull's answer follows the same membership rule.** Since 0.1.0 its
  log entries were written before the peer's membership was checked. Now
  they count only if the peer is a member here before or after them;
  otherwise nothing of the answer is stored, addresses and file wants
  included. A page that removes the peer with more of its log to come is
  kept and the rest asked for, in a request that only reads (no
  addresses, peers, log entries, summary, detail or offer). A request
  queued when its peer is removed is not sent.
- **A joined room keeps the label its person approved, and is never
  `local` (D64).** Since 0.1.0 the preview's label was not compared with
  the signed create entry, and a room from another machine could be
  labelled `local`. Now a create entry with another label is refused; a
  new room's label may not read as `local` or hold control, direction or
  invisible characters (a joiner the display keeps, as in an emoji or
  between Persian letters, passes); and a stored room labelled `local`
  that is not the local room is shown as its label, `#` and the first 8
  hex of its id, and never resolves as `local`.
- **An agent can tell this machine's person from a `human` elsewhere
  (D65).** Since 0.1.0 any member machine could post as `human@<its id>`,
  results left out the author's machine key, and `ask_human` said only
  "from human"; a short id is 32 bits. Now every message an agent reads
  says `from_this_machine` (by full key) and, for `human` in any case,
  `person`; `ask_human` names the person's id by full key; a machine whose
  short id another known machine shares is written in full and marked
  `id_shared` in tool results, the local API and status; no two rooms are
  shown alike, a room name that could mean two rooms is refused, and
  `room create` refuses `local`; in `peers`, a machine name two machines
  use, or one that reads as another's id, carries the id. Registering as
  `Human`, and `a2p2 mcp --harness human`, are refused. The page's notices
  and trace, the CLI's `trace`, question alerts and the hook carry the same
  marks; the hook's notice used to say no sender was the user even when
  this machine's person was one. A replaced notice says `edited` and names
  the message's own label, as the thread does; when another agent of the
  author's machine replaced it, both name that agent in `changed_by`.
  `unread` first lines and MCP `peek` lines carry `changed_by` and
  `edited` too, MCP gives `changed_by_person`, and the CLI, the page and
  question alerts mark a `human` editor. `trace` gives the author and
  writes a handler with a shared short id in full. The hook and `unread`'s
  senders name the agent that wrote the text shown: a local agent that
  replaced the person's message used to be summed up as the person. Any
  agent on a machine can still replace or withdraw every other label's
  messages there, the person's included; the marks show who did.
- **Replaced text is marked, and an answer is tied to the text shown
  (D62).** Since 0.1.0 an author could replace a message after a reader
  answered, with nothing to tell, and a handled report was accepted for a
  version replaced since the read. Now local API entries and MCP messages
  carry `edited`, the CLI prints `[edited]` and the page marks it; a
  replacement whose text expired keeps the mark and shows as expired. A
  handled report, or a reply by an addressee, is refused as `not_shown`
  until a read shows the newest text held, and while newer text is
  announced but not here. The page sends the version it drew
  (`reply_version`), and its background requests keep no receipt. A
  signed reply or report still names only the message.
- **A peer's report can no longer add stream rows without limit (D62).**
  Since 0.1.0 made-up epochs in an unsigned report each became a stream
  row for the life of the room. Now at most 32 such rows per author are
  kept on one peer's word, also when taken over from another peer, and
  they go when it stops naming them. A peer that raises another's hint and
  drops it leaves that peer's last report in place.
- **One admin can no longer block its own removal by flooding requests
  (D57).** Since 0.1.0 entries waiting for a second admin could fill the
  other admins' 32-request queue, and an entry that arrived while it was
  full was never raised. Now a machine asks its person about at most 32
  waiting entries per room, one signing admin gets more than 4 only from
  places no other admin's 4 need, the person's own requests have their
  own budget, the first removal of an admin per signer and target passes
  both budgets, a request whose entry no longer waits is closed, and a
  skipped entry is raised once there is a place and at start.
- **One room's stored log can no longer stop the daemon (D57).** Since
  0.1.0 a start failed for every room if one room's log did not parse or
  replayed with a different reason text, so a stricter rule in a later
  release, or an entry a hostile member got stored, could stop the daemon
  after an upgrade. Stored entries are now checked for form and signature
  only, the replay compares dispositions only, and a batch that drops
  entries over a bound is decided again without them. A room whose log
  still does not load is set aside (see Upgrading); streams without a
  cursor are held back from reading, undecided text stays undecided, and
  only retention deletes its text and files.
- **Remote text is escaped wherever it is shown (D58).** Since 0.1.0 a
  room's creator could put a carriage return, terminal escapes, a
  right-to-left override or a newline in the label, which went raw into
  every membership request and alert, `confirm`, `alerts` and the page, so
  a seconding request could hide that it makes the creator's second
  machine an admin. Now remote strings are written as `\u{...}` when they
  hold controls, format or default-ignorable characters (zero width and
  tag characters, U+E0000 to U+E0FFF, variation selectors, U+FFF0 to
  U+FFF8, Hangul fillers, line and paragraph separators), in requests,
  alerts, the join preview, the page, the CLI (messages, alerts, room
  list, status, doctor, `confirm`, `join`, `fetch`, error lines, and in
  `net check` members' addresses, which another member can set to any
  text, the probe details that quote them, what members saw, their errors
  and the router's answers), the question alert, and MCP tool results.
  Honest text shows as written where it changes no order:
  joiners and selectors inside emoji and in scripts that need them,
  right-to-left marks, Thai word breaks, the flags of England, Scotland
  and Wales, Japanese variation sequences and a CR before a line end. A
  typed `\u{202e}` is shown as `\u{5c}u{202e}`. In approval text, room
  labels and names are isolated so right-to-left text cannot reorder the
  words around them, and `confirm` puts such a label on its own line. Two
  labels that draw alike, or differ only in a Unicode space, are shown
  with their ids. `--json` prints the stored strings. `a2p2 confirm` sends
  the text it showed, and the daemon decides nothing if the request
  changed since (the local API's `confirm` takes an optional `text`).
- **The page escapes what other machines wrote (D58).** Exposed since
  0.2.3, the first release whose page loads. Room labels, machine names,
  agent ids, message text and choices are escaped as the daemon does. A
  message that asks the person is drawn under its own heading, says who
  asked from which machine and of whom, says it approves nothing, and has
  plain buttons. A choice its
  button cannot show exactly as sent (a hidden character, a line break, a
  blank other than a plain space, a space at either end or two in a row)
  is not sent; the person types the reply. An emoji choice such as "❤️
  Yes" is sent.
- **A seconding request says who chose the machine's name (D58).**
  Since 0.1.0 `confirm` said "the machine chose its own name" also when the other
  admin wrote it. The request now has `name_from` and `recorded_name`, its
  text shows the name the room records and other members with the name
  shown. An approval from the page is refused if that text changed before
  the decision; reload and decide again.
- **A warning before removing the only member reached (D71).** Proposing
  or confirming the removal of the only member this machine exchanged with
  in the last hour says so in `a2p2 confirm`, on the page and in the local
  API, with the name escaped and isolated.
- **A mute ends (D71).** Since 0.1.0 a peer that answered `not_member`
  twice was left alone until either side's log changed, so a listener that
  left and joined again could stop a laptop pulling until a restart. The
  mute now lasts 5 minutes, doubling up to an hour; a laptop removed
  from a room asks its former listener once an hour.
- **A message's author can no longer hold another member's file budget
  (D63).** Since 0.2.0 the author of a message with files could ask a
  `carry` member for its own files and never upload, reserving their size
  for a day, or for the message's life with one byte each. A request from
  the author now starts no carry job; one author's unfinished carry jobs
  reserve at most a quarter of the room budget, each at its full size;
  and a part an upload left goes when its want lapses or the author edits
  the message, and one a 0.2.3 binary left under a lapsed want goes at the
  next sweep.

Hosts that reach the peer port:

- **One host can no longer lock members out of a listener (D59).**
  Exposed since 0.1.0 on every machine with networking `on`, to any host
  that reaches its peer port (the local network, and the internet after
  `net on --map-port` or on a global IPv6 address). One host could hold
  all 256 inbound connections, raw or under made-up keys, and keys that
  share no room could fill the per-key rate tables. Now a connection gets
  a pool only once its handshake has proved the client's key: 256 for
  members of a room held here (16 for one key), 32 for other keys, and 64
  for connections still in the TLS handshake, cut at 5 s. One IPv4
  address or IPv6 /64 gets at most 8 connections outside the members'
  pool, a /56 16 and a /48 32. The handshake and the membership lookup
  after it end at 5 s, and membership is checked again for each request.
  A key that shares no room gets one request at a time, every answer
  closes its connection, and its rate buckets are its own. An attacker
  with more source groups can still do part of this: 4 groups (4 IPv4
  addresses, 2 IPv6 /56s or one /48) keep joiners out, and 8 groups (8
  IPv4 addresses, 4 /56s or 2 /48s) keep members from opening new
  connections, except members whose IPv4 address or IPv6 /64 completed a
  member's handshake here in the last 24 hours: they have 16 handshake
  slots of their own, at most 4 for one address. Connections members
  already have keep working.
- **One IPv6 /64 can no longer refuse every joiner (D59).** Exposed since
  0.1.0 on admin machines with networking `on`: one /64 filled the table of
  4,096 sources, and 60 polls a minute naming unknown invites refused
  every joiner that resumed by hash. Now the join route counts per address
  and per IPv6 /64 at 4 times as much: 80 bodies read a minute, counted
  before a body is read, then 10 opens and accepts and 30 polls. Polls
  naming unknown invites are counted per IPv4 address or /64 (10 a
  minute) after the lookup, so a joiner resuming by its invite's hash is
  never refused by them. A joiner retries an accept answered 429 or 503
  after the stated delay. Source tables forget the least recently heard
  in constant time.
- **Strangers are not recorded as connected in (D59).** Since 0.2.0 requests
  from keys that share no room counted, and enough of them pushed members
  out of what `a2p2 status` reports.
- **Refused requests no longer grow `daemon.log` (D67).** Since 0.1.0
  each refused request was one Info line. Refusals of keys that share no
  room, and refused connections, are now logged at Debug, with one Info
  line a minute about refused connections; a member's refusals reach Info
  5 at once, then one per 10 s per key. The log is now rotated: once over
  10 MiB, checked every 10 seconds, its last 10 MiB move to
  `daemon.log.1`, so about 20 MiB is kept plus what is written between
  checks; if that copy fails, the log is emptied anyway.

A device on the local network:

- **Port mapping trusts less of what answers as the router (D68).**
  Exposed since 0.2.0 after `a2p2 net on --map-port`. Another device could
  make a2p2 skip PCP with one datagram, grant a lifetime of 136 years so
  a2p2 never renewed, report a multicast address so that no later start
  mapped or removed anything, or report a private address that a2p2 put
  first in invites. Now a NAT-PMP version answer counts only if no PCP
  answer comes in any try, and the PCP request stays recorded to be
  removed; a lifetime over the 2 hours asked counts as 2 hours; an
  external address that is not public IPv4 is not taken and a2p2 tries to
  remove what the router made; no UPnP port past 65535 is asked for; a run
  adds at most 8 mappings to remove; and only a gateway with a private
  address, not a loopback one, is asked (what an earlier version recorded
  on another is still removed). The mapped address is no longer given out
  once networking stops using it, even if the router did not confirm the
  removal, nor after a renewal answered with something unusable, until a
  usable grant. Once a lease has run out, a silent router is asked every 5
  minutes, not every second. Port mapping stays off unless a person asks
  at a terminal, each time; a decision now carries the role word
  `map_port_v2` next to `map_port`, and a removal that fails at `net off`
  or a stop is logged, shown in `status` from the next run, and retried.
  The `net on --map-port` text says a router may keep a mapping past 2
  hours after a crash.

Agents and other users on the same computer:

- **An agent with a shell no longer acts as the person by accident
  (D66).** Since 0.1.0 the CLI acted as `human@<machine id>` whenever
  `--as` was left out or empty, so an agent running `a2p2`, or one told to
  by a room message, could answer the person's questions as the person
  and mark them seen. The new rule is in Upgrading. The daemon log
  records `--person` used away from a terminal or in an agent's shell,
  and `status` and `doctor` show how many times since the daemon started,
  with the last time. This is a guard rail: any program running as you
  can still act as you.
- **The page's address no longer carries a long-lived secret (D67).**
  Since 0.1.0 `a2p2 ui` opened an address holding the page's token for the
  daemon's whole run, and on Linux `xdg-open` and the browser keep it in
  command lines other users can read; with it they could read every room,
  send as you and approve requests. Now the address holds a code that
  works once and for 30 seconds, and the page trades it for a session
  kept in the tab. A code used twice ends the session it gave, however
  late the second use. `a2p2 ui --print` still prints the address alone
  on standard output, and says on standard error that it works once. On
  Linux a code is refused to a connection that
  the kernel's socket table, matched on both addresses and ports, shows is
  another user's; where that table cannot say (macOS, Windows, a cut-down
  `/proc`), the code alone decides. A tab left open after its daemon
  stopped no longer shows what another listener on its port answers,
  forgets its session when it signs out, and sends a message or decision
  only right after a state from its own daemon for its own session. A
  daemon whose page port was taken says so in its log. `status --json`,
  which agents run, no longer prints the page's address (D66).
- **A home under a directory other users can change is refused (D67).**
  On macOS and Linux, a user who could change a directory above the home
  could put another home with their own socket at its path, which a
  running MCP session would dial. a2p2 now refuses a home under a
  directory another user owns, one others can write without the sticky
  bit, or one a group you are not in can write, also through links, and
  names it. A directory a group you are in can write (on macOS, `staff`)
  gives a warning in the log, `status` and `doctor`. A directory no other
  user can reach (under a 0700 directory of yours, as `~/Documents` is on
  macOS) is neither refused nor warned about.
- **An agent can no longer attach a2p2's own key or database (D63).**
  Since 0.2.0 an agent steered by room text could stage the machine key,
  the local key or `a2p2.db`, and one careless approval sent it to every
  member that may read the room. Staging now refuses every file under the
  home except agents' copies in `files/`, and any file that is the same
  file as one in the home, a hard link included. A request staged before
  the upgrade from the machine key is refused at approval.
- **A path to attach can no longer make the machine connect out before
  anyone approves (D63).** Since 0.2.0, on Windows a path such as
  `\??\UNC\host\share\x` was opened, which can send your Windows
  credentials to that host; on Unix a path under an automount point such
  as `/net` could make the machine connect to the host it names. Staging
  now accepts on Windows only a drive letter and plain names, and on Unix
  refuses `/dev`, `/proc`, `/sys`, `/net` and the macOS automount points.
  Each part of the path is looked up in the directory opened before it,
  and every link is checked before it is followed; on macOS and Linux each
  part is opened without following a link and kept only if it is the
  entry checked, and on Windows a swapped part is refused after the open.
  A drive letter mapped to a share still works. A file under `/net`,
  `/home` on macOS or another refused point must be copied elsewhere to
  be attached. The Windows part has not been run on Windows.
- **Staging tells an agent less about files it cannot read (D63).** Since
  0.2.0, once 32 requests waited, an agent could learn whether two files
  had the same bytes, one bit per call; and a send naming a readable file
  beside one that could never be staged opened, sized and copied the file
  before it was refused. A full queue is now refused before any path is
  touched, the same bytes listed twice are listed once, the whole send is
  checked before any path is opened, and a refusal after opening has the
  same words and steps. A request says when a file has several names on
  disk.
- **`a2p2 install codex` no longer approves every a2p2 tool (D60).**
  Since 0.1.0 it added `default_tools_approval_mode = "approve"` to
  `[mcp_servers.a2p2]`, so a Codex agent could `send`, `reply` or
  `ask_human` without a prompt when a member of a shared room asked it to.
  Install now approves nothing without `--approve-tools`, and the dry run
  says which you chose. The edits and doctor follow `CODEX_HOME`. Edits to
  `config.toml` skip multi-line strings and arrays and touch only a2p2's
  tables; `--approve-tools` adds no second `default_tools_approval_mode`
  in any spelling TOML allows, and adds nothing, naming the line to add,
  when a line that could set it is one a2p2 does not read in full.
- **Install and uninstall write a file only if it still holds what they
  read (D60),** checked just before the rename; otherwise they leave it
  and say which file changed. Before, only Codex's `config.toml` was
  checked.
- **Pi's skill no longer puts message text in double quotes (D60).** Since
  0.1.0 it told the model to run `a2p2 send ... "text"`, where `$(...)`
  and backticks in text copied from a peer would run as you. It now sends
  text through a heredoc whose quoted end word has random hex digits
  chosen at install, and writes the `--as` label out in every command.
- **Install refuses a binary that another user could replace (D60).** A
  binary in a temporary directory, in Go's build cache (`go run`), in a
  directory every user can change or owned by another user (where another
  user can reach it), or with a control character in its path is refused,
  also through a symlink; a group-writable directory gives a warning, and
  `a2p2 doctor` reports the same. Directories install creates for a
  harness's configuration are 0700. On Windows the advice no longer
  suggests `C:\a2p2`; the binary's access list is not checked there yet.
- **The team launcher asks before it approves a2p2's tools (D69).** Since
  0.1.0 `scripts/team` approved every a2p2 tool for each Claude Code, Codex
  and Antigravity member. A manifest must now say `"approve_a2p2_tools":
  true` for `up`; `status`, `stop`, `logs` and `report` still work for runs
  started without it. Claude Code members get the seven tools by name
  instead of `mcp__a2p2__*`.
- **The team launcher refuses a state directory another user could swap
  (D69).** Since 0.1.0 its supervisor ran the command lines saved in a
  `state_dir` another user could replace. `up` now refuses a `state_dir`
  or `a2p2_home` whose path another user could change, and an existing
  state directory group or others may write (a group that is only you does
  not count); it checks the home again after making it and makes missing
  directories 0700 whatever the umask. `status`, `stop`, `logs`, `report`
  and the supervisor refuse such a directory too, and the supervisor runs
  from the manifest `up` checked, saved in the state directory.
- **The team launcher's `allowed_tools` check catches more ways to allow
  every command (D69).** It let through `Read,Bash`, `Read Bash` and rules
  such as `Bash(sh:*)`, `Bash(python3:*)` or `Bash(git *)`. These are
  refused now, also in other case or with a control character, and so is
  a model or prompt that starts with `-`; a kickoff that starts with `-`
  is posted as text. `dangerous` is refused in a tool's name only, so
  `Read(./notes/dangerous-ideas.md)` is accepted again.
- **The team launcher tells members and the person apart by full key
  (D69).** Its watcher, `team status` and `team report` matched by the
  8-character short id and counted a message to any `human@` as a
  question to the person. They now use `from_node` and the exact id
  `a2p2 status` gives the person. `team status` and `team logs` write C1
  controls (such as CSI, U+009B) and bidi overrides that a member wrote or
  a harness logged as `\u{...}` by the daemon's rules, so a Persian
  non-joiner or an emoji with a joiner still shows as written; `team logs
  --raw` prints the bytes, and `team logs -n` reads only the end of each
  log.

Releases:

- **The release workflow pins its actions and keeps write scopes away
  from the build (D61).** Since 0.1.0 the release job ran actions named by
  moving tags (`actions/checkout@v4`, `actions/setup-go@v5`) while it held
  `contents: write`, and its checkout kept the token, so anyone who could
  move one of those tags could change a published binary. Now every
  action is pinned to a commit, no checkout keeps its
  token, and the write, signing and attestation scopes sit in a publish
  job that checks out nothing and runs no repository code; it publishes
  only the six binaries, with exactly one `SHA256SUMS` line each. `make
  check` refuses a workflow that undoes this, and workflow syntax its
  line-based check does not follow. This release is the first made this
  way.
- **The installers can require the build attestation (D61).** Since 0.2.2
  `install.sh` and `install.ps1` check the binary against `SHA256SUMS`
  from the same release, which does not stop someone who can replace
  both. With `A2P2_VERIFY_ATTESTATION=1` they also run `gh attestation
  verify` and install nothing without a valid attestation from this
  repository's release workflow for the tag they download, so an attested
  build of an older tag is refused too; for `latest` they ask `gh` for the
  tag once. Releases up to 0.2.3 have none. Without the setting they say
  the checksum does not show who built the file.
- **The installers refuse directories another user can change (D61).**
  `install.ps1` makes a missing directory with an access list for you,
  SYSTEM and Administrators only, and refuses a junction or symbolic link
  in the path, a directory owned by another account, and one another
  account may change (not yet run on Windows). `install.sh` refuses, along
  the path as written and the real path a link leads to, a directory every
  user may write without the sticky bit, one a group with a member other
  than you and root may write, one owned by another user, and a symbolic
  link owned by another user. A group of only you and root, such as
  `admin` for a Homebrew prefix on a one-person Mac, is accepted; a macOS
  group that holds accounts it does not list, such as `everyone`, is
  refused; a group whose members it cannot list gets a note. It makes
  missing directories with mode 0755 and checks again after. The cases
  are in [SECURITY.md](SECURITY.md#release-binaries).

### Fixed

- **A machine no longer backs itself off from its peer by sending, and
  every request to a peer comes out of one budget.** Since 0.1.0 the
  exchange that carried a machine's new messages to a peer as an offer
  stored nothing locally and returned at once, and the scheduler counted
  it as a peer answering at once with nothing: it backed that peer off for
  1 s, then 2 s, doubling to 5 minutes, and later messages waited. It hurt
  most a machine that cannot be reached (`net outbound`, or a network that
  drops inbound), whose offers are its only way out. And each new write
  abandoned the request on the way, so on a link slower than the gap
  between writes nothing arrived until the writes stopped. Now:
  - An exchange that offers changes made since the last one is never
    counted as a spin. A request that carries an offer is not cancelled by
    a newer write (what changes meanwhile goes in the next; leaving the
    room and shutdown still cancel); a request
    times out after 30 s, a waiting pull after its wait and 30 s.
  - Every pull request to one peer, of every room, is admitted by a
    budget: a slot first (at most 3 in flight and not yet admitted at the
    listener, below its 4; a waiting pull counts until answered or for 1
    s), then a token of a bucket of 15 a second, burst 30, and only then
    prepared, its peer's membership checked again. Each further address a
    request tries takes another token. So at most 930 pull requests reach
    one peer in any minute of one run of the daemon (a restart or a
    networking change starts full buckets), a 25 % margin under the
    listener's limit, and the requester draws no 429 for its rate or for
    requests in flight (on links under about 1 s each way; on slower ones
    a waiting pull's slot can be freed before the listener holds it).
    Joins, file transfers, dial-back and dials to unverified addresses keep
    their own limits. Buckets are kept for at most 1,024 peers, never
    dropped while in use; past that, peers share one.
  - A peer that answers every wait at once is still backed off while the
    room is quiet, and gets at most that budget while it changes (0.2.3:
    up to 1,200 requests a minute in tests). One exchange sends at most 64
    requests; one cut there that stored something or offered records not
    offered before in that run is followed at once by the next, which
    offers again and takes detail from where it stopped; one that did
    neither is backed off. So a peer that says it holds nothing and
    changes its digest after every offer gets each record once after a
    write, and then the back-off.
  - Measured over two daemons on loopback (`scripts/bench.sh --only M5`,
    one run), B connecting out only: B to A one message at a time, before
    7 of 1,000 arrived within the 150 s budget, now all 1,000, median 61
    ms, p95 63 ms; a burst of 2,000 from B arrives with median 46 ms, p95
    79 ms; A to B one at a time, median 128 ms, p95 130 ms; no 429 on
    either side. Sending one message right after another is sustained
    writing, paced by the budget: a message costs about 3 requests at
    human pace and a
    question and answer about 4 a round, so machine-speed ping-pong
    between two machines is slower than in 0.2.3 by design (about 200 ms a
    round on a 5 ms link, against 40 ms). A star of one hub and four
    outbound-only spokes each writing every 500 ms runs every spoke near
    the budget's ceiling; the budget, not the listener, paces it, and every
    message still arrives (worst 275 ms in
    tests). Machines running 0.2.0 to 0.2.3 see fewer requests from this
    version, and this version answers theirs as before.
- **Serving and making a pull no longer gets slower as a room's history
  grows.** Every pull request served or made worked out the machine's
  summary of the room by reading every record of it, under the lock that
  covers every room, and a held pull did so again after each write. The
  store now keeps, per room in memory, the positions held without text,
  kept up to date by the writes as they commit and safe against a read
  whose request is cancelled. A summary then reads the room's streams and
  the few live texts past retention that pruning has not marked yet, and
  asks for pruning when it finds one. A page or offer reads each stream at
  most one more record than it can carry, and wanted text one record at a
  time; a record received from a peer reads the room's streams only when
  this machine's history permission needs them. The cache assumes, as the
  daemon's home lock ensures, that no other process writes the database.
  Measured in process on an Apple Silicon laptop (`go test -tags bench
  -run TestBenchPullHistory ./internal/node/`: two machines over the
  in-memory network, synthetic authors, one stream each, 200 B texts; CPU
  per request on the serving machine, two runs each, the second in
  brackets, in ms):

  | records, authors | idle opening (`same`) | opening after one new record | detail with a page of 1 | catch-up page |
  |---|---|---|---|---|
  | 1,000, 5 | 0.52 (0.52) → 0.16 (0.16) | 0.41 (0.42) → 0.06 (0.05) | 0.85 (0.82) → 0.20 (0.21) | 1.75 (1.78) → 1.11 (1.08) |
  | 10,000, 5 | 12 (13) → 0.16 (0.16) | 14 (14) → 0.05 (0.07) | 23 (28) → 0.20 (0.21) | 37 (40) → 6.29 (6.45) |
  | 50,000, 5 | 62 (66) → 0.18 (0.17) | 66 (72) → 0.07 (0.07) | 131 (131) → 0.22 (0.23) | 146 (142) → 7.01 (6.76) |
  | 100,000, 5 | 129 (146) → 0.18 (0.18) | 130 (131) → 0.07 (0.07) | 316 (275) → 0.24 (0.24) | 374 (296) → 7.78 (7.68) |
  | 1,000, 50 | 3.98 (2.05) → 0.96 (0.97) | 2.63 (1.41) → 0.20 (0.21) | 5.04 (2.64) → 1.11 (0.91) | 4.83 (2.58) → 1.12 (1.09) |
  | 10,000, 50 | 21 (11) → 0.99 (1.00) | 20 (9.79) → 0.30 (0.20) | 38 (19) → 1.04 (1.12) | 40 (21) → 1.92 (2.25) |
  | 50,000, 50 | 109 (70) → 0.96 (0.97) | 113 (67) → 0.31 (0.34) | 217 (134) → 1.10 (1.09) | 214 (134) → 2.43 (2.54) |
  | 100,000, 50 | 159 (147) → 0.95 (0.93) | 151 (145) → 0.29 (0.31) | 298 (290) → 1.16 (1.08) | 289 (292) → 2.47 (2.15) |

  On the requesting side, preparing an idle opening went from 0.36 ms to
  0.05 ms at 1,000 records and 5 authors and from 158 to 0.06 ms at
  100,000, and a catch-up costs 85 to 105 ms of CPU per request on both
  sides together at 100,000 (it was 610 to 875). Four peers' idle
  openings served at once rose from 2.4 to about 23,000 a second at
  100,000 records. What still grows with history: the streams of a room
  (restarts start new ones), the positions without text other than a run
  from the first, which the detail lists on the wire, and the catch-up
  page, which reads up to its limit of each stream it covers.
- **A send no longer slows down as a room's history grows, and an inbox
  much less (D56).** Looking up what refers to a message, its replies, its
  thread and a new record's dependants read every record of the room; they
  now read only the records that match. Measured on one laptop with a real
  daemon (`make bench-local`, one run each; the "before" run overlapped
  other work on the machine): a send's p50 was 4.1 ms at 10,000 messages
  and 10.8 ms at 30,000, now 0.26 and 0.27 ms. An agent's first inbox page
  of 50 plus its Seen, with the whole history unread, took 2.1 s at 1,000
  messages and 75 s at 10,000, now 73 ms and 0.40 s; that still grows with
  the unread backlog. Results and their order are unchanged.
- **Many agents waiting in one room no longer slow every send down.** A
  message this machine writes now wakes only the waiting inboxes it can
  show something to (its addressees, or the agents whose level lets a
  room-wide post in; a handled report wakes its writer), and a level
  change or an inbox receipt wakes only that agent's waiting inboxes.
  Other changes still wake every waiter of the room. A send with files
  that is declined or fails, a staged send that a file sweep declines or
  fails, and a room set aside while the daemon runs now wake waiters too,
  instead of waiting for the fallback look. A waiter still looks again
  each second, the only way it learns of an entry expiring by the clock
  before the pruning pass, a gap hint, and a file list marked invalid or
  failed. What an inbox shows is unchanged, and nothing new is stored.
  Fan-in on one machine (N workers each ask one orchestrator in one room
  at level `addressed` and wait for the answer; real daemons on an Apple
  Silicon laptop shared with other work; two runs each, before / after):
  answer p50 1.5-3.0 / 1.4-1.5 ms at N=1, 23-38 / 13 ms at N=10, 878-1009
  / 106-107 ms at N=50 and 38-44 s / 0.82-0.86 s at N=200. At N=200 the
  orchestrator gets 122-127 answers a second (was 3) and its reply takes
  5.5-6.0 ms (was 212-233). Room-wide at level `all`, N=50: 512-1181 /
  430-440 ms. With a room per worker, N=50: 99-227 / 96-99 ms. Local sends
  with no waiters (2,200-3,800 a second) and a send to one waiting inbox
  (0.53-0.59 ms) are unchanged.
- **A home whose path holds `#`, `?` or `%` now holds its database.** The
  path was given to SQLite unescaped, so `/x/a#b/a2p2.db` opened a file
  `/x/a` beside the home, and a `?` also dropped a connection setting. A
  `%` followed by two hex digits was decoded: `%00` cut the path the same
  way, and other escapes usually named a directory that did not exist, so
  the open failed. The path is now escaped. If the home holds no database
  yet and an a2p2 database (this store's schema version and tables,
  opened read-only to check) is at the path an older version opened, the
  daemon refuses to start (see Upgrading). Any other file at that path,
  such as another program's SQLite database, does not stop the start.
- **Leaving a room with many files no longer stalls the other rooms.**
  Since 0.2.0 the file deletions of a left room ran under the rooms lock,
  about 4 ms per file, so pulls and approvals of every room waited. They
  now run after the lock is released; other file work still waits.
- Pruning no longer fails in a room with more than about 32,000 authors
  whose readability is undecided, and an inbox read writes no receipts
  for a room left, or left and joined again, while it ran (both since
  0.1.0).
- **`team stop` tells the truth.** Since 0.1.0 it said "stopped" with a
  member still running, and marked the run ended when no supervisor
  answered. It now exits non-zero in both cases, leaves the roster as it
  was, and says when there is no member to stop.
- **CLI errors name the flag typed.** `a2p2 inbox --wait 600` said
  `wait_ms is 0 to 120000`, a local API field in milliseconds, while the
  flag takes seconds. `inbox`, `alerts` and `fetch` now say `--wait takes
  0 to 120 seconds, not 600` before anything is registered, and a negative
  `inbox --limit` or `history -n` names the flag. Present since 0.1.0.
- **A daemon that a command starts no longer keeps that command's working
  directory.** Since 0.1.0 it ran, for as long as it ran, in the directory
  of the first CLI command or MCP server that started it, which could
  hold a temporary worktree or keep a volume from being unmounted. With an
  absolute `A2P2_HOME`, the default, it now runs in its home. A relative
  `A2P2_HOME` names a place from that directory, so the daemon stays
  there, as before.
- On Windows, `doctor` names the daemon log with the system's separator
  (`C:\Users\you\.a2p2\daemon.log`). Since 0.2.0 it wrote
  `C:\Users\you\.a2p2/daemon.log`.
- `make e2e` no longer shows the agent-shell notice: the scripts' own
  `history` call now passes `--as`. The CLI never printed the notice for
  a command given `--as`, and a test now says so.

### Changed

- **The MCP `inbox` returns at most 18,000 bytes by default, and marks
  seen only what it returned (D70, D72).** A new `max_bytes` argument
  (512 to 32,000; 0 or absent is the default) sets the size of the result
  a reader wants. 1 to 511 and negative values are refused; a value above
  32,000 is read as 32,000, and the result's `note` says so. The daemon picks the
  entries, oldest first, and keeps receipts for those only: what did not
  fit is neither returned nor marked seen, and the new `more` says how
  many of those held when the daemon looked are still unread. 1 KB
  messages come about fifteen to a call instead of fifty; fifty one-line
  messages still fit one. The budget covers the whole result, a level set
  in the same call included, and a read refused for its budget sets no
  level. An oldest message too large for the budget is refused
  (`too_large`) with at least the `max_bytes` it needs; one too large for
  any budget is returned alone at 32,000 with its text cut to fit and
  `text_cut` saying so, so no result is over 32,000 bytes. A wait that
  ends with nothing is held to the budget too. `max_bytes`
  with `peek`, `recent`, `skipped`, `thread` or `fetch` is refused. On a
  daemon of 0.2.3 or earlier the MCP server reads as before, with no
  budget, and its `note` says to run `a2p2 stop`. The local page and the
  CLI read without a budget. The local API's `inbox` takes `max_bytes`
  and `level` (no budget when absent, and it refuses a value outside 512
  to 32,000) and answers `more`; `status` lists `features`. The sizes
  follow tokens measured in Claude Code, 1.5 to 3.4 bytes a token. The
  result is still marked seen before the model reads it,
  so a harness that cuts a result can still lose messages.
- **The MCP tool descriptions are clearer, and the server sends short
  instructions (D54).** The seven descriptions now say that messages reach
  other agents and people, that `whoami` comes first, what `inbox` also
  does (`level`, `fetch`), and when to use `reply`, `ack`, `send` and
  `ask_human`; "mail" became "message". Some parameter descriptions and
  the `whoami` result's note were reworded. The initialize and discover
  results carry an `instructions` text of 276 bytes. The `whoami`, `inbox`
  and `ack` descriptions were shortened again to pay for `max_bytes`. No
  other tool, parameter name, schema or annotation changed; the
  definitions are 3,995 bytes (3,979 in 0.2.3), under the 4,000 limit.
- **`room leave` asks first (D66).** Since 0.1.0 it deleted the room's
  data at once. It now asks at a terminal for a typed `yes` (and is
  refused away from one), shows the room's id
  and leaves that id, and says when this machine is the room's only admin
  or member. A room it cannot look up is not left.
- **`net on` says who gets the addresses it advertises:** every member and
  invite holder, which can include VPN and office addresses. It lists the
  `advertise` addresses of `config.json` when they are set.
- **The durability limit on macOS is stated.** `send` survives a crash of
  a2p2 or of the operating system, but on macOS a sudden power loss can
  lose the last few sent messages, because the database's sync does not
  flush the drive's cache there. REQUIREMENTS D1 and section 6 say so.
  Nothing in the program changed.
- **Recipes say only what the evidence supports.** Team-size numbers are
  gone, the round limit is called a chosen number, and a lead or a
  reviewer from another model family is described as a practice, not a
  proven gain. The room charter may set batching and a few defined
  abbreviations, and carries a version line.

### Added

- **Release binaries carry a GitHub artifact attestation (D55),** made by
  the release workflow and checked by `make verify-release` for every
  binary of a release against the tag. Check one with `gh attestation
  verify a2p2_<os>_<arch> --repo wallter/a2p2 --signer-workflow
  wallter/a2p2/.github/workflows/release.yml --source-ref
  refs/tags/<tag>`. Releases up to 0.2.3 have none, and the binaries are
  still not code-signed.
- **Three recipes.** `recipes/sub-teams.md` (smaller teams inside a team),
  `recipes/fewer-turns.md` (fewer model turns in a team) and
  `recipes/fan-out.md` (briefs to many workers, and reading their answers
  cheaply with `peek` and a bounded `inbox`). The skill lists them.
- **Benchmarks.** `make bench-local` measures real daemons on one
  computer (local send, the MCP server, two daemons, scale); `make
  bench-net` measures throughput and latency between daemons in Docker
  containers over shaped links. Neither is part of `make check`.
- **A test opens the databases written by each release from v0.2.0 to
  v0.2.3** with the current code, so a rule that would refuse old data
  fails it.

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

[Unreleased]: https://github.com/wallter/a2p2/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/wallter/a2p2/compare/v0.2.3...v0.3.0
[0.2.3]: https://github.com/wallter/a2p2/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/wallter/a2p2/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/wallter/a2p2/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/wallter/a2p2/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/wallter/a2p2/releases/tag/v0.1.0
