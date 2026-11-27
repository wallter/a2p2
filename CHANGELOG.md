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

- **The MCP `inbox` returns at most 24,000 bytes by default, and marks seen
  only what it returned (D70).** A new `max_bytes` argument (512 to 40,000)
  sets how many bytes of result a reader wants; without it the tool now
  applies 24,000, so a large backlog comes in more calls than before (1 KB
  messages about twenty to a call instead of fifty; fifty short messages
  still fit one). The daemon picks the entries, oldest first, and keeps
  receipts for those only: what did not fit is neither returned nor marked
  seen, and the result's new `more` says how many of those held when the
  daemon looked are still unread, so the agent calls again. The budget
  covers the whole result, a level set in the same call included; a read
  refused for its budget sets no level. An oldest message too large for
  the budget is refused (`too_large`) with at least the `max_bytes` it
  needs, and nothing is marked; one too large for any budget is returned
  alone at 40,000 with its text cut to fit and `text_cut` saying so, so no
  result is over 40,000 bytes. `max_bytes` with `peek`, `recent`,
  `skipped`, `thread` or `fetch` is refused. On a daemon before 0.2.4 the
  MCP server reads as before, with no budget, and its result's `note` says
  to run `a2p2 stop` to start the new daemon. The local page and the CLI
  read without a budget. The local API's `inbox` takes `max_bytes` and
  `level` (no budget when absent) and answers `more`, `status` lists
  `features`, and `a2p2 inbox` prints how many more are unread when its
  limit left some out. The result is marked seen before the model reads
  it, as before, so a harness that cuts a result can still lose
  messages. The `whoami`, `inbox` and `ack` descriptions were
  shortened to pay for the argument: the definitions are 3,992 bytes.
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

- **Serving and making a pull no longer gets slower as a room's history
  grows.** Every pull request served or made worked out the machine's
  summary of the room by reading every record of it, under the lock that
  covers every room on the machine. A held pull did the same again after
  each write. The store now keeps, per room in memory, the positions held
  without text, kept up to date by the writes as they commit and safe
  against a read whose request is cancelled while it runs. A summary then
  reads the room's streams and the few live texts past retention that
  pruning has not marked yet, and asks for pruning when it finds one. The
  database and the wire are unchanged; the cache assumes, as the daemon's
  home lock ensures, that no other process writes the database. A page or
  offer also reads each stream at most one more record than it can carry
  at a time, and wanted text one record at a time, instead of the rest of
  the stream, and a record received from a peer reads the room's streams
  only when this machine's history permission needs them. Measured in process on an Apple Silicon laptop
  (`go test -tags bench -run TestBenchPullHistory ./internal/node/`: two
  machines over the in-memory network, synthetic authors, one stream
  each, 200 B texts; CPU per request on the serving machine, two runs
  each, the second in brackets, in ms):

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

  On the requesting side, preparing an idle opening went from 0.36 ms
  to 0.05 ms at 1,000 records and 5 authors and from 158 to 0.06 ms at
  100,000, and a catch-up costs 85 to 105 ms of CPU per request on
  both sides together at 100,000 (it was 610 to 875).
  Four peers' idle openings served at once rose from 2.4 to about 23,000
  a second at 100,000 records. What still grows with history: the streams
  of a room (restarts start new ones), the positions without text other
  than a run from the first, which the detail lists on the wire, and the
  catch-up page, which reads up to its limit of each stream it covers.

- **Many agents waiting in one room no longer slow every send down.** A
  message this machine writes now wakes only the waiting inboxes it can
  show something to (its addressees, or the agents whose level lets a
  room-wide post in; a handled report wakes its writer), and a level change
  wakes only that agent's waiting inboxes. Other changes still wake every
  waiter of the room. Some changes used to wait for the fallback look and
  now wake the waiters too: a send with files that is declined or fails,
  a staged send that a file sweep declines or fails, a room set aside
  while the daemon runs, and an inbox receipt, which can let a message
  through the level for the agent's other session. A waiter still looks
  again each second, and that look is still the only way a waiter learns
  of the changes nothing announces: an entry expiring by the clock before
  the pruning pass, a gap hint, and a file list marked invalid or failed.
  What an inbox shows is unchanged: Inbox and Seen walk from the cursors
  as before, and nothing new is stored. Fan-in on one
  machine (N workers each ask one orchestrator in one room at level
  `addressed` and wait for the answer; real daemons on an Apple Silicon
  laptop shared with other work; two runs each, before / after): answer
  p50 1.5-3.0 / 1.4-1.5 ms at N=1, 23-38 / 13 ms at N=10, 878-1009 /
  106-107 ms at N=50 and 38-44 s / 0.82-0.86 s at N=200. At N=200 the
  orchestrator gets 122-127 answers a second (was 3) and its reply takes
  5.5-6.0 ms (was 212-233). Room-wide at level `all`, N=50: 512-1181 /
  430-440 ms. With a room per worker, N=50: 99-227 / 96-99 ms. Local
  sends with no waiters (2,200-3,800 a second) and a send to one waiting
  inbox (0.53-0.59 ms) are unchanged.
- **A home directory whose path holds '#', '?' or '%' now holds its
  database.** The path was given to SQLite unescaped, so a path like
  `/x/a#b/a2p2.db` opened a file `/x/a` beside the home, and a '?'
  also dropped a connection setting. A '%' followed by two hex digits
  was decoded: `%00` cut the path the same way, and other escapes usually
  named a directory that did not exist, so the open failed. The path is
  now escaped. If no database is in the home yet and an a2p2 database
  (one with this store's schema version and tables, opened read-only to
  check) is at the path an older version opened, the daemon refuses to
  start rather than start an empty node beside it. The refusal names both
  paths and the `-wal` and `-shm` files, and says the destination must not
  exist. Once the files are moved into the home, the node starts with its
  data. Any other file at that path, such as another program's SQLite
  database, does not stop the start.
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
- **A machine no longer backs itself off from its peer by sending, and
  every request to a peer comes out of one budget.** Since 0.1.0, the
  exchange that carried a machine's new messages to a peer as an offer
  stored nothing locally and returned at once, and the scheduler counted
  it as a peer answering at once with nothing: it backed that peer off for
  1 s, then 2 s, doubling to 5 minutes, and later messages waited for the
  back-off to end. It hurt most a machine that cannot be reached (`net
  outbound`, or a network that drops inbound), whose offers are its only
  way out. And each new write abandoned the request on the way, offer and
  all, so on a link slower than the gap between writes nothing arrived
  until the writes stopped. Now an exchange that offers changes made since
  the last one is never counted as a spin, a request that carries an offer
  is not cancelled by a newer write (what changes meanwhile goes in the
  next; leaving the room and shutdown still cancel, and a request times
  out after 30 s, a waiting pull after its wait and 30 s), and every pull
  request to one peer, of every room, is admitted by a budget: a slot
  first (at most 3 in flight and not yet admitted at the listener, below
  its 4, a waiting pull counting until answered or for 1 s), then a token
  of a bucket of 15 a second, burst 30, and only then prepared, its
  peer's membership checked again. Each further address a request tries
  takes another token. So at most 930 pull requests reach one peer in any
  minute of one run of the daemon (a restart or a networking change
  starts full buckets), a 25 % margin under the listener's limit, and the
  requester draws no 429 for its rate or for requests in flight (on links
  under about 1 s each way; on slower ones a waiting pull's slot can be
  freed before the listener holds it). Joins,
  file transfers, dial-back and dials to unverified addresses keep their
  own limits. Buckets are kept for at most 1,024 peers, never dropped
  while in use; past that, peers share one. A
  peer that answers every wait at once is still backed off while the room
  is quiet, and gets at most that budget while it changes (0.2.3: up to
  1,200 requests a minute in tests). One exchange sends at most 64
  requests; one cut there that stored something or offered records not
  offered before in that run is followed at once by the next, which
  offers again and takes detail from where it stopped; one that did
  neither is backed off, so a peer that says it holds nothing and changes
  its digest after every offer gets each record once after a write, and
  then the back-off. Measured over two daemons on loopback (`scripts/bench.sh
  --only M5`, one run), B connecting out only: B to A one message at a
  time, before 7 of 1,000 arrived within the 150 s budget, now all 1,000,
  median 61 ms, p95 63 ms; a burst of 2,000 from B arrives with median
  46 ms, p95 79 ms; A to B one at a time, median 128 ms, p95 130 ms; no
  429 on either side. Sending one message right after another is
  sustained writing, paced by the budget: a message costs about 3
  requests at human pace and a question and answer about 4 a round, so
  machine-speed ping-pong between two machines is slower than in 0.2.3 by
  design (about 200 ms a round on a 5 ms link, against 40 ms). A star of
  one hub and four outbound-only spokes each writing every 500 ms runs
  every spoke near the budget's ceiling; the budget, not the listener,
  paces it, and every message still arrives (worst 275 ms in tests).
  No wire change: machines running 0.2.0 to 0.2.3 see fewer requests from
  this version, and this version answers theirs as before.
- On Windows, `doctor` names the daemon log with the system's separator
  (`C:\Users\you\.a2p2\daemon.log`). Since 0.2.0 it wrote
  `C:\Users\you\.a2p2/daemon.log`.

### Security

- **A key that is not a member no longer leaves state behind on a
  listener.** Since 0.1.0, a pull from any key, member or not, was
  recorded in memory, and its requests waited for the lock every room
  shares. Any host that reaches the peer port could grow the daemon's
  memory with fresh keys and slow members' pulls. Now such a request is
  refused before the lock, and nothing of it is kept; so is one for a
  room not held here. One that carries log entries is checked at most 2
  at a time, within a budget of 4 a second for the machine and one a
  second for each source address. What is kept of a peer that is no
  longer a member goes within 10 minutes, and so does a stream hint
  whose report is a day old (hints are new since 0.2.3).
- **Members no longer choose what this machine dials.** Since 0.1.0, any
  member could give addresses for itself or for other members: loopback
  ports, link-local and cloud metadata addresses, well-known ports, and
  names that resolve to these. This machine then dialled them with a TLS
  handshake, for pulls and, since 0.2.0, file transfers and `a2p2 net
  check`; and a dial-back it answered went to any port from 1, and to
  loopback when the request came over loopback. Now an address on
  another machine's word is kept and dialled only if it could be a
  member's, on every one of those paths: LAN, VPN, overlay and public
  addresses at port 443 or 1024 and above, never loopback, link-local, or
  an IPv6 form that carries a refused IPv4 address. A name is looked up
  once and only the addresses that pass are dialled. Addresses from an
  invite are dialled as given once the invite is accepted; a preview no
  longer marks them. An address `a2p2 net check HOST:PORT` is given is
  dialled as typed. Each member also gets a share of the unverified dial
  budget, and a host name pays it for every address it gives, even a
  name that answered before. A listener on a port below 1024 other than
  443, or on a plain link-local address, is now learned only from an
  invite. An address an older version stored that the rule refuses, even
  one where the member's key answered, is shown by `a2p2 status`, `a2p2
  doctor` and `a2p2 net check`, not dialled unless an accepted invite
  gave it, and dropped after a day of failing. A member known only by
  such addresses is no longer dialled: status and doctor say to join with
  an invite from it, which makes its address dialled as given.
- **A mute ends.** Since 0.1.0, a peer that answered `not_member` twice
  was left alone until either side's log changed. A listener that left
  the room and joined again could so stop a laptop pulling until a
  restart. Now the mute lasts 5 minutes, doubling up to an hour; a laptop
  removed from a room asks its former listener once an hour.
- **Strangers are not recorded as connected in.** Since 0.2.0, requests
  from keys that share no room counted, and enough of them pushed
  members out of what `a2p2 status` reports.
- **A warning before removing the only member reached.** Proposing or
  confirming the removal of the only member this machine exchanged with
  in the last hour now says so in `a2p2 confirm`, on the page and in the
  local API, with the member's name escaped and isolated as in the
  request's text.

- **A join can no longer write into a room this machine already holds
  from a machine that is not a member.** Since 0.1.0, a removed admin, or
  any machine holding a copy of the room, could send a member an invite.
  Once its person approved, the answer's entries were applied (a removed
  admin's late removals among them) and the `not_caught_up` gate opened,
  even with no heads. Now, for a room held here, the preview must name
  the create entry and label held here. Each batch of the answer counts
  only if the admin machine is a member here before or after it, else
  the whole batch is refused; this also holds for a join resumed after a
  restart once this machine is a member. The gate opens only for an
  answer from a member whose heads are exactly the heads of the log
  here. A page that breaks the page limits or does not move on ends the
  join, and so does an answer that passes 256 pages or 64 MiB, or pages
  for 15 minutes, or for 2 minutes without an entry this machine lacks.
  These count over every run of the join until the daemon restarts, so
  an answer cut short each run does not start them again, and the next
  run goes on from the last page read.
  Entries kept while the admin machine is not yet a member are checked
  again once a quarter page more of them is kept, not on every page.
- **A pull's answer follows the same membership rule as a request.**
  Since 0.1.0, the log entries of a pull's answer were written before the
  peer's membership was checked, so a peer removed while its answer was on
  the way still had its entries stored. Now they count only if the peer is
  a member here before or after them; otherwise nothing of the answer is
  stored, its addresses and file wants included. A page that removes the
  peer, with more of its log to come, is kept and the rest asked for, so
  the next page, if it admits it again, is taken. That request only reads:
  it carries no addresses, peers, log entries, summary, detail or offer of
  this machine's. A request queued when its peer is removed is not sent.
- **A joined room is held under the label its person approved, and never
  as `local`.** Since 0.1.0, the label shown in a join preview was not
  compared with the room's signed create entry, and a room created on
  another machine could be labelled `local`, so its messages read as this
  machine's local room. Now a create entry with another label is refused,
  a new room's label may not read as `local` or hold control, direction
  or invisible characters (a joiner the display keeps, such as one in an
  emoji or between Persian letters, passes), and a stored room labelled
  `local` that is not the local room is shown with its id in every result
  and never resolves as `local`. A person is shown it as its label, `#`
  and the first 8 hex of its id, which no other room changes, so the
  text of a request waiting for approval stays the same. A join recorded by an older version
  under another label is refused, and the room stays.
- **An agent can tell this machine's person from a `human` on another
  machine.** Since 0.1.0, any member machine of a shared room could post
  as `human@<its id>`, MCP results left out the author's machine key,
  and `ask_human` said only that the answer comes "from human", so an
  agent of any owner in a shared room could take another machine's
  `human` for its own person. A short id is 32 bits and nothing marked a
  machine whose short id matched this one, and a room created elsewhere
  could be labelled `local`. Now every message an agent reads says
  `from_this_machine` (by the full key) and, for `human` in any case,
  `person`; `ask_human` names the person's id by the full key; a machine
  whose short id another known machine shares is written in full and
  marked `id_shared` in tool results, the local API and status; only the
  local room is shown as `local`, no two rooms are shown alike, and a
  room name that could mean two rooms is refused; `room create` refuses
  `local`; and in `peers` a machine name two machines use, or one that
  reads as another machine's id, carries the id. Since 0.1.0 an agent could also register
  as `Human`, and `a2p2 mcp --harness human` as the person; both are now
  refused. The same marks now reach the page's notices and trace, the
  CLI's `trace`, question alerts and the hook, whose notice used to say
  no sender was the user even when this machine's person was one. A
  replaced notice says `edited` and names the message's own label, as
  the thread does; when another agent of the author's machine replaced
  it, both name that agent in `changed_by`. `trace` gives the author and writes a handler with a shared short
  id in full. `unread` first lines and MCP `peek` lines carry
  `changed_by` and `edited` too, MCP gives `changed_by_person`, the CLI,
  the page and question alerts mark a `human` editor, and the hook and
  `unread`'s senders name the agent that wrote the text shown: a local
  agent that replaced the person's message used to be summed up as the
  person. Any agent on a machine can replace or withdraw the messages of
  every other label there, the person's included; the marks show who did.
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
  10 s per key. At Debug each refusal is still a line; the log file is
  now rotated (below).
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
- **The local API marks a replaced message, and a handled report is tied
  to the text shown.** Since 0.1.0, the author of a message could replace
  it after a reader answered, and thread, branch and history showed the
  new text above the answer with nothing to tell it was changed; a
  handled report was accepted for a version replaced since the read.
  Message entries in the local API now say `edited` when their text is a
  replacement, MCP inbox messages carry `edited`, the CLI prints
  `[edited]` and the page marks the message edited. A replacement whose
  text expired keeps the mark, and the message is shown as expired rather
  than with its first text. A handled report is refused as `not_shown` until a read shows the
  newest text held, and while newer text is announced but not here. So
  is a reply by an addressee shown an older text, from any client. The
  page sends the version it drew with a reply and a handled report
  (local API `reply_version` on send), and its background requests keep
  no receipt. A signed reply or handled report still names only the
  message, not the version answered.
- **A peer's report can no longer add stream rows without limit.** Since
  0.1.0, any member could name made-up epochs of other authors in an
  unsigned pull report, and each became a stream row here, kept for the
  life of the room and listed as still being fetched. Now at most 32 such
  rows per author are kept on one peer's word, also when it takes them
  over from another peer, and they are deleted when it stops naming them
  or leaves the author out of a whole report. A peer that raises another
  peer's hint and then drops it leaves that peer's last report in place.
- Pruning no longer fails in a room with more than about 32,000 authors
  whose readability is undecided, and an inbox read writes no receipts
  for a room left, or left and joined again, while it ran (both since
  0.1.0).
- **An agent can no longer attach a2p2's own key or database to a send.**
  Since 0.2.0, an agent steered by room text could stage the machine key,
  the local key or `a2p2.db`, and one careless approval sent it to every
  member that may read the room. Staging now refuses every file under the
  home except agents' copies in `files/`, and any file that is the same
  file as one in the home, a hard link included. A request staged before
  the upgrade from the machine key is refused at approval.
- **A path to attach can no longer make the machine connect out before
  anyone approves.** Since 0.2.0, on Windows a path such as
  `\??\UNC\host\share\x` passed the name check and was opened, which can
  send the user's Windows credentials to that host; on Unix a path under
  an automount point such as `/net` could make the machine connect to the
  host it names. Staging now accepts on Windows only a drive letter and
  plain names, and on Unix refuses `/dev`, `/proc`, `/sys`, `/net` and the
  macOS automount points, checking every link on the way before it is
  followed. Each part of the path is looked up in the directory opened
  before it. On macOS and Linux each part is opened without following a
  link and kept only if it is the entry checked, so a link or directory
  swapped in after its check is refused before its target is opened; on
  Windows it is refused after the open. A drive letter mapped to a share
  still works as in 0.2.3. A file under `/net`, `/home` on macOS or
  another refused point must be copied elsewhere to be attached. The
  Windows part has not been run on Windows.
- **Staging tells an agent less about files it cannot read.** Since 0.2.0,
  once 32 requests waited an agent could learn whether two files had the
  same bytes, one bit per call, with no request for the person to see. A
  full queue is now refused before any path is touched, and the same
  bytes listed twice are listed once. Since 0.2.0, a send that named a
  readable file beside an entry that could never be staged also opened,
  sized and copied the file before it was refused, with no request
  raised. The whole send is now checked before any path is opened, and a
  refusal after opening (unreadable, too large, over the budget) has the
  same words and steps. A request now says when a file has several names
  on disk.
- **A message's author can no longer hold another member's file budget.**
  Since 0.2.0, the author of a message with files could ask a member in
  the default `carry` setting for its own files: the member reserved
  their size for a day while the author never uploaded, and for as long
  as the message lived if the author uploaded one byte of each. Its own
  sends with files were refused as too large. A request from the author
  now starts no carry job, one author's unfinished carry jobs reserve at
  most a quarter of the room budget, each counted at its full size so
  that bytes uploaded but not yet checked free nothing, and
  a part an upload left goes when its want lapses or the author edits
  the message, and a part a 0.2.3 binary left under a lapsed want goes
  at the next sweep.
- **An agent with a shell no longer acts as the person by accident.**
  Since 0.1.0 the CLI acted as `human@<machine id>` whenever `--as` was
  left out or empty, with no check for a person. Any agent that ran
  `a2p2` in its shell, or one told to by a room message, could answer
  the person's questions as the person and mark them seen, so they left
  the person's list. Now an empty `--as` is an error. In a shell that
  Claude Code, Codex or Gemini CLI marks as its agent's, or with
  `AI_AGENT` set, the CLI acts as that agent (under `A2P2_NAME` when the
  shell has it) and says on stderr which id it used. Elsewhere `send`,
  `ack` and an `inbox` that marks messages seen act as the person only
  when stdin or stderr is a terminal (so text piped in at a terminal
  works) or with the new `--person` flag. `a2p2 ui`, whose address
  lets the page act as the person, is refused in an agent's shell and
  away from a terminal unless `--person` is given, and takes no
  `--json`. Over SSH it opens nothing on the remote computer and prints
  the address with the port to forward; a browser that cannot be started
  is said, with the address. The daemon log records `--person` used away
  from a terminal or in an agent's shell, and `status` and `doctor` show
  how many times since the daemon started, with the last time.
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
- **The page's address no longer carries a long-lived secret.** Since
  0.1.0, `a2p2 ui` opened an address holding the page's token for the
  daemon's whole run, and on Linux `xdg-open` and the browser it starts
  keep that address in their command lines, which other users of the
  machine can read; with it they could read every room, send as you and
  approve requests on the page. Now the address holds a code that works
  once and for 30 seconds; the page trades it for a session kept in the
  tab. A code used twice ends the session it gave, however late the
  second use. On Linux a code is refused to a connection that the kernel's socket
  table, matched on both addresses and both ports, shows is another
  user's; where that table cannot say (macOS, Windows, a cut-down
  `/proc`), the code alone decides.
  `a2p2 ui --print` still prints the address alone on
  standard output, and says on standard error that it works once. With
  a daemon before this release still running, `a2p2 ui` opens and prints
  nothing and says to run `a2p2 stop`. A tab left open after its daemon
  stopped no longer shows what another listener on its port answers
  (every answer names its run), forgets its session when it signs out,
  and sends a message or a decision only right after a state from its
  own daemon for its own session.
  A daemon whose page port was taken says so in its log.
- **`daemon.log` no longer grows without end.** Since 0.1.0 another
  machine could make it grow (a refused request is a line). Once over
  10 MiB its last 10 MiB are moved to `daemon.log.1`, checked every 10
  seconds, so about 20 MiB is kept plus what is written between checks;
  if that copy fails, the log is emptied anyway.
- **A home under a directory other users can change is refused.** On
  macOS and Linux, a user who could change a directory above the a2p2
  home could put another home with their own socket at its path, which a
  running MCP session would then dial. a2p2 now refuses such a home and
  names the directory, including one reached through links: one another
  user owns, one others can write without the sticky bit, or one a group
  you are not in can write. A directory a group you are in can write (on
  macOS, `staff`) is not refused; the log, `status` and `doctor` warn.
  A directory no other user can reach (under a 0700 directory of yours,
  as `~/Documents` is on macOS) is neither refused nor warned about.
- **A device on the local network has less hold on port mapping.**
  Exposed since 0.2.0: a person who ran `a2p2 net on --map-port`, on a
  network with another device that answers as the router. That device
  could make a2p2 skip PCP with one datagram that holds nothing of the
  request and forget the PCP mapping it may have made, grant a lifetime
  of 136 years so a2p2 never renewed, report a multicast address so that
  no later start mapped or removed anything, or report a private address
  that a2p2 then put first in invites. Now a version answer counts only
  if no PCP answer comes in any of the tries and the PCP request stays
  recorded to be removed, a lifetime longer than the 2 hours asked counts
  as 2 hours, an external address that is not a public IPv4 address is
  not taken and a2p2 tries to remove what the router made, a UPnP port
  past 65535 is never asked for, a run adds at most 8 mappings to remove,
  and only a gateway with a private address, not a loopback one, is asked
  for a mapping (what an earlier version recorded on another one is still
  removed). The mapped address is no longer given out once networking
  stops using it, even if the router did not confirm the removal, nor
  after a renewal answered with something that cannot be used, until a
  usable grant. Once a lease has run out, a router that does not answer
  is asked every 5 minutes, not every second. Port mapping stays off
  unless a person asks for it; the CLI asks at a terminal each time. The
  `net on --map-port` text now says a router may keep a mapping past 2
  hours after a crash.
- **Port mapping turned on by 0.2.3 or earlier is off after the
  upgrade.** A `--map-port` decision an earlier version recorded is not
  applied: nothing is mapped, what that version left on the router is
  removed, and `status` and `net on` say so. Run `a2p2 net on --map-port`
  again to map. A decision taken now carries a new role word,
  `map_port_v2`, next to `map_port`; 0.2.3 reads it as before. A removal
  that fails at `net off` or a stop is logged and shown in `status` from
  the start of the next run, and retried.
- **The team launcher refuses a state directory another user could
  swap.** Since 0.1.0 `scripts/team` used a `state_dir` that another user
  of the machine could replace, for example under a directory they made
  in `/tmp`, and its supervisor ran the command lines saved there, so
  that user's command could run as you. `up` now refuses a `state_dir`
  or `a2p2_home` whose path another user could change, and an existing
  state directory group or others may write (a group that is only you,
  as with the umask 002 on many Linux systems, does not count); it checks the home again
  after making it, and makes missing directories 0700 whatever the umask.
  `status`, `stop`, `logs`, `report` and the supervisor refuse such a
  state directory too, and the supervisor runs from the manifest `up`
  checked, saved in the state directory.
- **The team launcher's `allowed_tools` check catches more ways to allow
  every command.** Since 0.1.0 it let through `Read,Bash`, `Read Bash`
  and rules such as `Bash(sh:*)`, `Bash(python3:*)` or `Bash(git *)`,
  which give a Claude Code member unscoped shell access. These are
  refused now, also in other case or with a control character, and so is
  a model or prompt that starts with `-`. A kickoff that starts with `-`
  is now posted as text. `dangerous` is refused in a tool's name only, so
  a rule such as `Read(./notes/dangerous-ideas.md)` is accepted again.
- **The team launcher tells members and the person apart by full key.**
  Since 0.1.0 its watcher, `team status` and `team report` matched
  messages by the 8-character short id and counted a message to any
  `human@` as a question to the person. Another machine with the same
  short id could hide a member's activity or have its person's
  questions reported as this one's. They now use `from_node` and the
  exact id `a2p2 status` gives the person.
- **`team stop` tells the truth.** Since 0.1.0 it said "stopped" after
  its wait even with a member still running, and when no supervisor
  answered it marked the run ended, though a busy supervisor might still
  run. It now exits non-zero in both cases and leaves the roster as it
  was, and says when there is no member to stop.
- **`team status` and `team logs` escape terminal controls.** Since 0.1.0
  they printed C1 controls (such as CSI, U+009B) and bidi overrides that a
  member wrote or a harness logged. They are written as `\u{...}` now, by
  the daemon's rules, so a Persian non-joiner or an emoji with a joiner
  still shows as written; `team logs --raw` prints the bytes, and
  `team logs -n` reads only the end of each log.


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
