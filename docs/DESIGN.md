# Design

This is how the daemon works: the stage 1 delivery core. See
[Requirements section 9](REQUIREMENTS.md#9-order-of-work) for the stages,
[Protocol](PROTOCOL.md) for formats, [Testing](TESTING.md) for checks on
the code and [Decisions](DECISIONS.md) for changes made during
implementation.

- [Records](#2-records), [room log](#3-room-log), [pull](#4-pull), and [storage](#5-storage)
- [Reading](#6-reading-and-surfaces) and [trust changes](#7-trust-changes)
- [Failure handling](#8-failure-handling), [how this was built](#9-how-this-was-built), and [known limits](#10-known-gaps-and-limits)

**[M4]** is rule 4 of the [membership model](../internal/members/FINDINGS.md).
**[R7]** is rule 7 and **[RM2]** is membership rule R2 of the
[delivery model](../internal/model/delivery/FINDINGS.md).
Requirements appear by id. **Not modelled** means no model test covers the choice.

## 1. Summary

Machines pull signed records from any member (D8, D2). Each daemon start
opens a new stream [R2]. A signed member log supports several admins
[M1–M14]. Text lasts 30 days and signed stubs remain [R6]. Withdrawals,
replacements, handled reports and replies use records [R7]. Each agent has
its own cursors [R7]. A person confirms short invites (T3, P3). Machines
can connect out only (N1).

One daemon runs per machine. The socket sets the local trust boundary.
Machines use Ed25519 keys, pinned mutual TLS 1.3 and HTTP+JSON. The
stateless MCP shim, CLI and socket offer the same operations (A1).
Peer text is data (T8). The system provides hooks, a loopback UI and the
`local` room. Logs omit message bodies.

On Windows (D47) the boundary is the same rule by other means. The
home is created with an access control list that lets only the user in,
and an existing home that lets anyone else in, or that someone else owns,
is refused. The socket file inherits that list. At each start the daemon
also writes a new random key to `local.key` in the home and refuses every
local API request without it in the `A2P2-Local-Key` header, so only a
process that can read the home can call the daemon. On macOS and Linux no
key is written or asked for.

## 2. Records

A record has a signed header and an optional body [R1]. The four kinds
are `message`, `handled`, `withdraw` and `replace` (delivery FINDINGS,
"Record"). `ask_human` sends a message to `human` on this machine.
`human` answers with a reply. See [Protocol sections 1 and 2](PROTOCOL.md#1-record-header).

- To store a record, the receiver checks PROTOCOL 2, the stream's next
  position and its matching `prev`. For a stream's first record it must
  hold every link by room, author, epoch, sequence and id [R4]. The receiver
  refuses a second record at a held position as a fork. It repairs text
  for a held header only when the machine still wants and may read it.
- Every start writes a new stream whose first record links every head of
  the author's history held [R2]; there is no cap on links.
- The writer sets a replacement's version to one above the winning version
  it knows (the model's `World.amend`). Only an agent shown the text can
  report handling [R7]. Receivers do not check these writer rules. They
  pick the highest version, then the highest id [R7], in any arrival order.
- **Not modelled: references.** A record whose `ref` or `parent` is not held
  is stored and resolved when the target arrives. Then: a withdraw or
  replace needs a `message` by the same author machine (any agent there,
  T2); a handled needs a `message` its author's agent may report (an
  addressee, or any agent on a room-wide post with `ask_handled`, the
  model's `canHandle`); a reply's `thread` must equal its parent's thread
  (or the parent's id). One that fails has no effect; a reply that fails is
  shown without its parent. Records of other rooms are never targets.
- Text expires 30 days after `created` by the holder's clock, recorded and
  never undone [R6]. **Not modelled:** a record dated more than 24 hours
  ahead of the receiver's clock is not accepted yet; its stream waits.

**Who may author [RM1].** A machine stores a record if its author is or was
a member in its view, and only from a peer that is a member now. A record by
an author not a member now is shown marked "author removed", whoever
relayed it. A voided author's records are no longer shown or served; agents
shown one get a "voided" notice
(`TestVoidedAdmissionHidesAndDropsItsRecordsAndTellsReadersWhoSawThem`).
With no cut-off on records, a removed machine's later writes can reach
informed machines through a member not yet told
(`TestRecordWrittenAfterRemovalReachesNoHonestMemberButARelayedCopyIsKeptMarked`),
so after a removal T1 holds only in this weaker, marked form.

**"Was a member"** means the machine has ever had an effective admission
in `members.State.Admitted` [M13], regardless of membership period.
`Admitted` can shrink as entries arrive
(`TestAdmittedCanShrinkWhenMoreEntriesArrive`). The store must keep
records when that is the only reason to drop them [M13]. **Not modelled.**
The store keeps these records hidden and does not serve them. They return
if the machine re-enters `Admitted`. The delivery model drops them [RM1].

**Not modelled: too many heads.** Repeated restores without catching up can
leave more heads than a first record's header can hold. The machine refuses
to write and says why. The person re-adds it under a new key.

## 3. Room log

An entry is one of `create`, `add`, `remove`, `admin` [M14]; `create`
carries the room label (**not modelled**, display only). An `add`, or an
`admin` that also admits (its target was not a member in the signer's
view), carries `history` and `from`, the signer's per-author heads, with
`from` omitted for an empty frontier (PROTOCOL 3). Promoting an existing
member does not change its history permission.

**Not modelled: identity and chain.** A room's create entry is pinned when
the room is created or joined: its id is part of the room's identity and
comes from the invite preview, and any other create entry is refused. An
entry's chain (`prev` held, same author, `seq` one below) is validated
before its position is reserved or any fork evidence recorded, and the log
index allows both branches of a fork.

- Entries go through `members.Machine` [M11]: held until their parents are
  present; a second entry by one key at one position is refused with its
  dependants and kept as evidence. A fork by a key that was not an admin at
  either entry changes nothing (`TestForkByMemberWhoIsNotAnAdminChangesNothing`).
- `members.Resolve` gives members, admins and a reason for every entry
  without effect (A4) by [M2]–[M10] and [M12], with no timers [M14, T10].
  `admin` also grants membership; removing an admin removes the machine;
  a later add brings it back [M8]. The `members` table is a cache of it.
- **Fork evidence is a vote [M12].** Two entries one key signed at one
  position, one at least while it was an admin, count as its vote for its
  own removal; one other admin's vote completes it; evidence alone removes
  nobody (`TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone`,
  `TestForkSettledByOneRemoverKeepsTheBranchItHadSeen`). A sole admin's
  fork never settles.
- **Parents.** A machine signs entries naming as parents only entries it
  has accepted, never one it refused as a fork, so the branch the settling
  admin had accepted stands [M11, M12]
  (`TestSettlingRemovalNamesOnlyAcceptedEntries`). One open fork does not
  hold back settling another (`TestOneOpenForkDoesNotHoldBackSettlingAnother`).
- Entries are passed on in causal order, evidence after them.
- **Not modelled: pull before signing.** Since starting, a daemon signs no
  entry for a room until an exchange with a current member has left it
  holding every log head that member advertised (rooms it created, and
  rooms where it is the only member, are exempt; a room it just joined is
  exempt only if the join's last page listed heads, all accepted here,
  from a machine that is a member here). It narrows
  the restore problem and cannot detect a restore. If no other member can
  be reached, the request fails with `not_caught_up`; the person may
  approve it again with an explicit, recorded override after a warning
  that names the risk (a machine restored from a backup may sign over a
  position it has already used). Without the override an admin whose
  peers are all gone could never remove them.
- **History permission.** A machine's current permission comes from the
  effective grants of its current membership period (after its latest
  effective removal). When they differ the more restrictive applies [RM2]:
  without history beats with history, and of two frontiers the one that
  hides more per author, the model's per-stream maximum (`restrict` in
  `membership.go`; `TestTwoGrantsThatDifferOnHistoryTheMoreRestrictiveApplies`).
  **Not modelled:** a frontier id that cannot be resolved fails closed (no
  history), and `members.Resolve` exposes these grants for each member.

`a2p2 room list` and the page show members, admins, changes waiting for a
second admin [M4], entries without effect with reasons, fork evidence and
the section 6 limits for the admin count; an unsettled fork is an alert.

## 4. Pull

One request type per room carries the model's Pull [R3, R5] both ways
over pinned mutual TLS. See [Protocol section 4](PROTOCOL.md#4-pull).

- **Server.** It applies the request's log entries first (from a non-member
  only if they make it a member, T4), then requires the peer to be a member
  now (T5), then stores the offer under [RM1] and [R4]. A page carries
  wanted text first, then later records, predecessors first, only for the
  authors the requester takes [R3], with text only within the requester's
  permission [R8, RM2]. Membership and permission are checked when a page
  is built and again when a held response is released [RM2]
  (`TestHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited`,
  `TestHeldResponseIsCheckedAgainstAPermissionNarrowedWhileItWaited`).
- **Offers [RM3].** A request may carry records for the server, built
  against the server's last summary, only for a peer the sender sees as a
  member and within its permission; with no summary, no offer
  (`TestNoHistoryNewcomerIsNeverHandedEarlierTextIncludingThroughAnOffer`).
  A stale offer costs bytes, never correctness, and is how an N1 machine
  passes on what it writes (`TestMachineThatOnlyOpensRequestsStillConverges`).
  An offer cut by the size limit says so, and the rest follows at once.
- **Requester.** It applies the response only if the room's local
  generation is the one the request was made under (it changes on leave and
  on join), then log entries, then the page [RM1, R4], in one transaction.
  Each side keeps the other's summary as a dated observation, the only
  source of copy reports [R5].

**Not modelled: summary encoding.** A round opens with one hash of the
requester's per-author digests. When neither side has log entries to send
and the server's hash matches that hash or `seen`, the server answers
`same`. Otherwise it returns its digests. The requester sends its detail
for authors whose digests differ, 16 at a time (PROTOCOL 4.3). Equal
digests stand for equal summaries. For the other authors, the server
gets the full [R3] summary and pages as the model does.

A round that stores nothing on either side ends the exchange until the
next trigger. `seen` tells the server the requester already examined that
hash and nothing changed. This keeps machines idle when a difference
persists, as when one was admitted without history and holds headers
where the other holds text. Copy reports come from `same` and from detail.
Compared with the model, they reach the server later or only in part if
the difference persists. The differential test counts the reports it
leaves uncompared for this reason.

Measured with `TestPullSizesInASixtyFourMachineRoomAfterThirtyDaysOfDailyRestarts`
(skipped unless `A2P2_MODEL_DEEP=1`; the log lines say "64 machines", but
no 64 machines run). Two real nodes, `a` and `b`, in one process, joined
by the in-memory network. The other 62 members are synthetic keys that
only sign records, which both nodes are handed directly; they run no
node. Each key writes one record a day for 30 days, a new stream each day:
62 authors times 30 streams is 1,860 streams (the test's log label says
1,920). Counted are the bytes of each pull request and response as JSON,
inside the process: no TLS, no HTTP framing, no compression. In that
setup an idle pull of `b` from `a` takes 177 bytes up and 72 down. One
new message takes 1.7 KB up and 14.9 KB down in three requests. These
are sizes of the summary exchange, not a measurement of 64 machines or
of a network.

**Not modelled: when and from whom.**

| Trigger | Action |
|---|---|
| Start or wake | Short pulls of every room from two members |
| Steady state | A waiting pull per room to each of two *anchors*: the most reliable member, and one that rotates at every end, cancellation or failure of a wait through every eligible member and each of its addresses (N2) |
| Local write or new records | Re-issue waiting pulls at once, with offers |
| A peer holds what we lack | Pull from it at once; repeat while `more` |
| Failure | Back off 1 s doubling to 5 min with jitter; next member |

A server holds a waiting pull at most 60 s; client and server deadlines
exceed it by 30 s. A rotating anchor is first probed with a 10 s pull; failure moves on.
Waiting pulls to one peer share one HTTP/2 connection.

**Inbound connections.** A listener cannot know who connects until the TLS
handshake proves the client's key, and any key completes one. So it does
each handshake itself and counts connections in three pools. A
connection in its handshake takes one of 64 shared slots and a slot of
each group of its source: 8 for an IPv4 address or an IPv6 /64, 16 for a
/56 and 32 for a /48. When those are full, an IPv4 address or IPv6 /64
that completed a member's handshake here in the last 24 hours may use 16
slots kept for such groups, at most 4 at once, whatever its /56 and /48
hold. A connection there that does not prove a member's key is closed,
and its group is no longer known. A connection with no slot is closed at
once, and one whose handshake and membership lookup have not finished
after 5 s is closed. Only once the handshake has finished,
signature included, does the key move the connection to the members'
pool (256, at most 16 of one key), when it is a member of a room held
here, or to the strangers' pool (32, still counted against its source).
With no room there the connection is closed. Membership is checked again
for every request: a key that became a member moves to the members'
pool, a removed key is served as a stranger from then on, and a key
added back is a member's again. A stranger's connection serves one
request at a time, every answer to a stranger closes its connection, and
one lasts 15 s at most.
Strangers have rate buckets of their own, so they cannot fill the
members' table, and their refused requests are logged at Debug; a
member's are logged at Info 5 at once, then one per 10 s
(`TestOneSourceCannotHoldAllInboundSlots`,
`TestACopiedMemberCertificateWithoutItsKeyTakesNoMemberSlot`,
`TestAHandshakeFloodFromManySourcesDoesNotKeepOutAKnownMember`,
`TestMembershipIsCheckedAgainForEveryRequestOnAConnection`,
`TestRefusedPeerRequestsDoNotGrowLog`). A joiner, a removed machine and a
member not yet known here (whose request carries the entries that admit
it) are strangers until the room's log here names them; their requests
are answered as before, only in the smaller pool, and a waiting pull
started then is cut at 15 s. The join route limits each source address,
and each IPv6 /64 at 4 times as much, with budgets for bodies read
(before the body is read), opens and polls (PROTOCOL 5.3). A poll by hash
is looked up before it is charged to the budget for unknown invites, and
a joiner sends an accept answered 429 or 503 again after the stated
delay. What a flood from many source groups can still do is in
SECURITY.md.

**Cost (estimates).** About 130 transfers of about 2 KB deliver a 1 KiB
message to 63 machines. Not yet measured: duplicate offers, 100 rooms,
anchor load, and N1 behind a relay.

## 5. Storage

**Not modelled: schema** (PROTOCOL 8). Migrations 2 to 9 and 11 create the
tables; migration 10 removes the tables of the earlier v0.2 daemon, and if
they held messages or rooms the daemon tells the person once. In stage 1,
the machine keeps signed headers and read state for the room's life.
`DECISIONS.md` entries justify the tables.

Each send, response application, request log and offer application, Seen,
log entry signing and pruning pass takes one transaction. On startup the
daemon rebuilds `members.Machine` by replaying `log` in arrival batches.
The result is deterministic. A stored entry is checked for its form and
signature only, not for the content rules a received entry meets (such as
what a room label or a name may contain), so a rule added to receipt later
does not reject a log stored before it. The replay compares each entry's
disposition with the stored one, not its reason: a reason is an
explanation whose wording may change, and the stored text is left as it
is, so the version that wrote it can still open the database. A room
whose stored log does not parse, does not verify, or replays to another
disposition is set aside: it is not served or pulled, a send, replace,
withdraw, handled report, send with files, Seen or change of its files
setting is refused, and setting it aside deletes nothing. Seen across
rooms leaves its keys `not_shown` and confirms the other rooms. Its stored
messages can still be read here (a read still records what it showed,
which marks nothing seen). Its reading frontier is not known, so a stream
without a cursor is held back, and missing text is neither shown as
expired nor marked expired. Other text past retention still expires, and the file
sweep drops only files whose text is withdrawn or past retention; the
stored member cache decides nothing. `status` and `a2p2 doctor` show it
with the reason and what to do: run the version that stored it, or leave
it and join it again (not the local room, which cannot be left). The
other rooms open as usual. A batch that drops entries (over the held or
branch bound) is decided again without them, as a start would decide the
rows it leaves, so no input a peer delivers makes a room fail its own
replay. It prunes hourly and before every read, page
and offer. Text past expiry becomes `expired` permanently. The daemon
deletes withdrawn text when it stores the withdraw [R6].

## 6. Reading and surfaces

The inbox is the model's walk from each agent's cursors [R7]: winning text,
`expired`, withdraw and replace notices, `late`, and what it waits for. Only
Seen moves a cursor, confirming exactly the keys and versions a full inbox
read listed (its receipts, below); the hook prints a count and sender labels, never
message text ([harness notes](HARNESSES.md#what-the-hook-prints)). Claude Code,
Codex and Antigravity get the MCP server and a hook; OpenCode and Hermes
Agent get the MCP server only, and Pi, which has no MCP, a skill that
points its model at the CLI ([harness notes](HARNESSES.md)). Install
records any entry named `a2p2` it replaces in `$A2P2_HOME/installs/`, and
uninstall puts it back; without a record uninstall removes nothing
(D44). Install and uninstall write a harness file only if it still holds
what they read (D60). Install approves no a2p2 tool unless the person passes
`--approve-tools` (Codex only), so whether `send` waits for the person
is the harness's own rule; it refuses a binary in a temporary directory
or one another user could replace, and Pi's skill passes message text to
bash only through a quoted heredoc ([security](../SECURITY.md)). Replies carry
`parent_state`. The thread view lists held messages of a thread, or with
`after` only those that follow a given message of it; the branch view
follows `parent` down from the message, held or not, and misses replies
below a reply not held (G3). The inbox tool's `recent` (1 to 50) reads a
room's last messages through `history`. None of these three marks anything
seen. A reply goes to its
parent's author unless the sender names addressees (D10).

**Not modelled: the reply tool's addressees.** Without `to`, the `reply`
tool addresses the parent's author and every agent of a current member that
has posted in the thread as held here, without the sender, at most 32 (the
parent's author always kept, then the newest posters). The list is written
as `to`, so the record is an ordinary addressed reply and the reading rule
is unchanged. `a2p2 send --reply-to` without `--to` still reaches only the
parent's author.

**Reading levels (U1 to U4).** Each agent chooses, per room, how closely
it follows the room: `all` (the default: every message for it, as before
levels existed), `addressed` (messages whose `to` names it, and room-wide
posts marked for everyone) or `none` (no message reaches its inbox). The
level is local reading state, like cursors: it lives in the agent's own
machine's `levels` table, changes no record, entry or pull, and no other
machine learns it. "Marked for everyone" means the message's text begins
with `@everyone`: ASCII, any case, followed by whitespace, punctuation or
the end of the text (`core.MarkedForEveryone`). The signed formats are
frozen (D27), and an ordinary room-wide post and one sent to `everyone`
are the same bytes, so the mark is in the text, where the writer controls
it and every reader sees it. `ask_handled` asks for handled reports and
nothing else; it no longer decides a level. While a message's text is
missing, the mark is unknown: the inbox walk waits for the text rather than
classify the message. Machines that do not know levels show such a post to
everyone, as before. Replies count as addressed when they name the agent,
which the reply tool does for the parent's author and the thread's posters.
Notices about messages the agent was already shown (withdrawn, replaced,
voided) are never filtered (D9). The rule is in the delivery model
(`internal/model/delivery/levels.go`) and the differential tests compare
the code with it under random level changes.

**Receipts.** A full inbox read writes, in a transaction right after the
one that lists the entries, a receipt per entry: the agent, the room, the
entry's key and the version listed (table `shown`). If the room was left
in between, or left and joined again (another generation), it writes
none. `Seen` confirms only an entry with a
receipt whose version is the one given, and deletes the receipt; anything
else is refused as `not_shown`, so an id from `peek`, `recent`, `skipped`
or a thread can never be marked seen. A receipt outlives a level change:
a message the inbox listed is never kept out or written to `skips`, so it
can still be confirmed and its later notices (replaced, withdrawn) still
arrive. Leaving a room deletes its receipts.

A level change runs in one transaction: every message held then that the
old level kept out and the new one lets in, and that the agent has not
confirmed, is written to `skips`, so raising a level never floods the live
inbox with the past; the inbox continues from now. What the level kept out
(now, or by a skip) and the agent has not confirmed, after where its time
began, is read apart from the inbox with the inbox tool's `skipped` or
`a2p2 history --skipped`, and marks nothing. It lists a message whose text
expired while it was kept out as expired. It pages from the newest back
with `before` (a message id), like history. Each entry of `recent` (the
room's last N, whatever the level) says `skipped` when the reader's level
kept it out. The inbox tool sets
the level with `level` (for `room`, or every room), `whoami` shows each
room's level and unread count, and `peek` gives counts and first lines
without marking anything; `peek` for one room resolves the room as every
other read does and takes the first lines from that room only. The person (`human`) has no level: what is
addressed to the person, and requests for a person, are never filtered, and
the page reads every room in full and shows each local agent's level.

**Handled reports.** Only an agent shown the text can report it handled
[R7]. Every read that shows an agent a message's text counts: `inbox`, a
thread or branch view, `recent` and `skipped`. Those other views record
that in `shown` as a text mark (key `s:` and the message id), which moves
no cursor and marks nothing seen; only an agent the addressee rule allows
gets one. `reply` sends the report after the reply; a later `ack`
of the same message, or a second `ack`, succeeds without a second report
and names the first. An `ack` that is not allowed is refused with the
reason: no such message, not addressed and no `ask_handled`, the agent's
own message, or not yet shown its text by any read. Unless the message
was withdrawn, the read must also have shown the version reads show now:
its newest replacement held with text, or the message itself while its
text is live. A replacement since the read is refused as `not_shown`
until a read shows it, and so is a replacement whose text is not here
yet, which the inbox and thread also wait for. An expired message with
no live replacement is not checked, and neither is an expired message
the agent confirmed no text of: only the inbox's replace notice, shown
after a confirmed read, shows such an agent the replacement. `Draft.ReplyVersion` applies the
same rule to a reply that names the version answered; the local API,
the page and the MCP reply tool do not pass one yet. A message entry
whose text is its author's replacement says `edited` (node `Entry` and
local API), in the inbox, thread, branch, history, `recent` and
`skipped` views; the CLI, the MCP tools and the page do not show it
yet. The reply and ack
tools name the message `id`. Entries say `ask_handled` when a room-wide
post asks for handled reports.

**Reading order.** Every view (inbox, history, `recent`, `skipped`,
thread and branch) orders messages by place: `created`, then the author
machine, then the position in its stream, then the id. One author's
messages of one millisecond therefore come in the order written, and a page
boundary (`before`) is a place, so paging neither repeats nor drops a
message written in the same millisecond as the boundary. The store keeps
an index in that order (`records_reading`).

**Branch view under an unheld message.** The replies to a message name it,
so the branch under a message this machine does not hold, or holds only as
a header, still lists them, their parent marked missing (D10). A reply
below a reply that is not held is still not reached: gap G3.

Reasons shown (A4). `a2p2 trace ID` needs a message whose header this
machine holds (an unknown id is an error) and prints the state of its text
here: available, waiting for a copy, outside this machine's permitted room
history, withdrawn or expired. It shows no refusal and no fork evidence.
The reasons a log entry has no effect (every `members.Resolve` reason,
including a fork refused) and the entries waiting for a second admin show
in `room list` and the page. A record refused on receipt (does not
verify, not the next position, fork, author never admitted, dated ahead)
is counted per peer and room in the `refusals` table, and the second record
of a forked stream is kept in the `forks` table; no command, tool or page
reads either yet (a gap in A4, TRACE.md). A fork of a log key is shown: in
`room list` as conflicting membership entries, settled or unresolved, and
while unresolved as a `fork` alert (`a2p2 alerts`, the page). Every
refusal of the local API and the tools is a sentence, one of a few stable
codes and whether repeating the call later may succeed (PROTOCOL 7).

MCP tools stay seven (PROTOCOL 6): `send` with `reply_to`, `ask_handled`
and `replaces`; `inbox` with `thread`, `after`, `before`, `recent`,
`level`, `peek`, `skipped` and `wait_seconds`; `reply` and `ack` taking `id`; `peers` showing each room's
machines and the agents seen writing there; `ask_human` sending to `human`.
Each tool carries annotations: `whoami` and `peers` are read-only; `send`
is destructive, since `replaces` rewrites or withdraws a message; `ack` is
idempotent. The inbox tool waits at most 30 seconds (the CLI and the local
API allow 120), says when a wait ended with nothing, and a cancelled wait
marks nothing seen. Results are compact JSON with `<`, `>` and `&` as
written. Copy and handled reports stay in `a2p2 trace` and the page.

A2 is checked by a proxy, compact definition bytes divided by four and
rounded up: 3,991 bytes, 998 of the 1,000 allowed. The server also sends a short `instructions` text (at most 300 bytes) in its `initialize` and `server/discover` results, outside that count (D54). `TestToolsGolden` checks
the definitions match the protocol, and `TestToolDefinitionsFitTheTokenBudget`
enforces the 4,000-byte limit. The count by each model family's own
tokenizer has not been measured.

**Not modelled: alerts (A3).** `a2p2 alerts --wait` returns on a request
for a person, a fork, text missing for a day, a question for the person,
networking that is on but cannot listen, or a room whose other members
have all refused this machine as not a member for at least 10 minutes.
That room is shown in `status`, `room list` and the page as "this machine
may have been removed from this room; ask an admin"; it only changes what
is shown.

**Networking and stopping.** A person's networking decision (`a2p2 net`)
is recorded as a decided request, and the daemon applies it at once,
without a restart; `status` shows "being applied" until it has. If `on`
cannot listen, the daemon runs outbound only and says why. `a2p2 stop` is
honoured at any time, also while a decision is being applied: waiting
calls end and held pulls are released before the store closes.

## 7. Trust changes

**Not modelled: join (T3).** The token holds a secret, the admin key's
fingerprint and addresses (PROTOCOL 5). The joiner checks the admin's TLS
key against the fingerprint and stops on a mismatch; sends the secret; the
admin machine binds the invite to the joiner's key (single use) and answers
with a preview of room, role and history; a person on the joiner confirms
and the joiner accepts; the admin machine raises a request naming the
joiner's fingerprint, label, role and history; a person confirms; the
daemon revalidates, then stores the approval and the signed grant in one
transaction; the joiner polls. An invite is previewed, accepted, signed,
refused or expired (PROTOCOL 5.3). Once signed, `poll` returns the log,
expired invite or not, until the joiner is removed, which `poll` then says.
A machine that left locally but is still listed needs a fresh invite,
preview and confirmation. No choice means no history.

The joiner pins the label the person approved: a create entry whose
label is not the preview's is refused, and so is a new room whose label
reads as `local` (`node.LabelKey`) or holds a character the display
would escape (`DisplayHidden`). Rooms already stored are not checked; one
labelled `local` that is not this machine's local room is shown with its
id (`Rooms.ShownLabel`) and never resolves as `local`. For a room it
holds already, the joiner refuses a preview naming another create entry
or label, and takes each batch of the answer only as a pull takes it
(T4): if the admin machine is a member here before or after it. The
same holds for a join that pinned the room in an earlier run once this
machine is a member, and a join recorded by an older version under a
label other than the one held is refused, the room kept. Pages are kept
in memory until they make the admin machine a member, up to 1,024
entries or 4 MiB that this machine does not hold; otherwise nothing is
stored and the join is refused. A page with more than 256 entries or
3 MiB, or one marked `more` that does not move past the last, ends the
join. Results show a room's label through `Rooms.ShownLabel`: room
info, unread counts, levels, request texts and quarantine. A join lifts the
`not_caught_up` gate only if its last page lists heads, all accepted
here, and the admin machine is a member here, as `caughtUp` does.

**Not modelled: requests for a person (P3), narrowed.** Signing log
entries and sending files wait for a recorded confirmation naming exactly
what is approved, given on the local page or by `a2p2 confirm` in a
terminal (always-on machines are often headless). Turning networking on
and accepting a join invite are decided at a terminal, by `a2p2 net on`
and `a2p2 join`, each with a typed `yes` that the daemon records; the page
offers neither. No decision goes through agent tools. A same-user agent
set on bypassing it can; an OS-separated approver may come later.

A request's text is built from local facts and escapes every string that
can come from another machine (`EscapeDisplay`, PROTOCOL 10.8): the room
label a remote creator chose and the names in entries, each also isolated
(`Isolate`: U+2068 and U+2069; a `bdi` element on the page, its own line
in the terminal when it holds right-to-left text). Its line cannot be
cleared, reordered or split by them. A seconding request says the name
it carries was written by the admin who signed the first entry, shows
the name the room records for the machine, and names other members with
the name shown, compared without case, up to three by fingerprint and
the rest counted; the page and `confirm` no longer say the machine chose
it. Because the text depends on the room's members, the page's approval
and `a2p2 confirm` carry the text they showed; the daemon refuses a
decision when it changed and compares it again under the decision lock.
The stored label and entries are not checked or changed: logs signed
before keep loading. The alert that this machine may have been removed
from a room, and the CLI's join preview, escape and isolate the room's
label the same way.

On the page, room labels, machine names, agent ids, message text,
notices, thread entries, reply previews and choices are escaped when
drawn (`escapeDisplay`, `escapeText`, `escapeChoice` in `app.js`, the
same tables as the daemon's). Two rooms whose labels may be drawn the
same (escaped, in Unicode normal form C, with each run of Unicode white
space or blank braille as one space, without the invisible characters
the rules keep, and without case) are shown with their ids; `status`
does the same without NFC (`node.LabelKey`). The CLI's output and the text of MCP tool results
escape what other machines wrote the same way; `--json` prints the
stored strings. A message that asks the person (choices, or addressed
to `human`) is drawn under its own heading, apart from requests, with who asked, from which machine as
that room records it, and whether it was asked of the person or of
everyone; it says it approves nothing, and its choices are plain buttons.
A choice whose button would not show it exactly as it is sent (a hidden
character, a line break, a blank other than U+0020, a space at either
end or next to another) is not sent: the person types the reply instead.
An emoji sequence in a choice is shown and sent as it is.

| Change | Entries | Confirmed on |
|---|---|---|
| Add a machine | one `add` by any admin [M3] | that admin and the joiner |
| Remove a non-admin | one `remove` [M3] | that admin |
| Make an admin, one admin so far | one `admin` [M4] | that admin (and a new joiner) |
| Make an admin, more admins | matching `admin` from two [M4] | both |
| Remove an admin | matching `remove` from two, own vote not counted [M4, M5]; one, if the target signed a fork [M12] | each signer |
| Remove a machine being made an admin concurrently | matching `remove` from two [M4] | both |

`a2p2 room admin` replaces a lost admin (T9); the other admin sees the
first entry as a request [M4].

**Request budgets.** Requests this machine started (proposals, invite
accepts and sends with files) wait at most 32 per room, checked again in
the transaction that stores one. A proposal to remove an admin passes
that budget, once per target. Requests raised to second another admin's
entry have their own budgets, counting only requests whose entry still
waits: 32 in all per room, and 4 per signing admin. A signing admin past
its 4 takes only a spare place: one left after every other admin of the
room but this machine keeps the places of its 4 that it has not used. The
first removal of an admin per signing admin and target passes both
budgets and takes no place in them. So one admin that signs many waiting
entries takes the spare places on each other admin but not the 4 of
another admin (while the room has at most 9 admins: 8 others times 4 is
32), and cannot stop a removal of itself from being proposed or raised;
removals waiting do not crowd out other requests. With more admins, those
under their 4 can fill the 32 together. A pending request whose entry no
longer waits (another admin seconded it, or a later entry changed its
effect) is closed as
failed; if the entry waits again it is raised again. The flooder's
further entries stay in the log; each waiting entry without a request is
raised when a decision frees a place, after each batch, and at the next
start, a removal of an admin first, then in arrival order. If raising
fails after a decision or at start, the room's next batch or pull tries
again, and the start goes on.

**First three admins (T9 default).** The second machine gets one `admin`
entry; the third an `add` (it joins at once) plus `admin` entries from both
admins, confirmed on both and on the joiner. The model test
`TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations` covers only
resolution; `TestBootstrapFirstThreeMachinesBecomeAdminsWithBothAdminsConfirmingTheThird`
covers the daemon's code. A daemon never co-signs alone.

**Leaving.** A vote on one's own removal does not count [M5], so `a2p2 room
leave` (otherwise a machine cannot stop carrying a room) is local: the
room's data is deleted and its generation changes, so no late response
applies. Its files are deleted after the rooms lock is released, so
leaving a room with many files does not stall the other rooms' pulls and
approvals (other file work still waits for the deletions). Rejoining
needs a new invite; an admin removes it from the list.

## 8. Failure handling

| Fault | What happens | Model test |
|---|---|---|
| Sleep | Others carry on; it pulls on wake | `TestRelayDeliversAndReportsHandledWithoutAuthorAndReaderEverAwakeTogether` |
| Crash, lost reply | Send durable on return across a crash of a2p2 or of the operating system (D1; not a power loss on macOS, where the last sends may be lost); unconfirmed entries re-shown; next pull repeats | `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Disk lost | Others repair its records; it rejoins under a new key | `TestDiskLostOnOnlyOtherHolderRecordSurvivesOnAuthorAndRepairContinues`; rejoining not covered by a model test |
| Restore, author | New stream, no fork; older records may arrive late, marked | `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork` |
| Restore, reader | May be re-shown; handled not repeated | `TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped` |
| Restore, admin | Pulls the log before signing; a fork is an alert until one other admin settles it [M12] | `TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone` |
| Clock back / forward | Expired stays expired / expires early; future-dated records wait | `TestTextExpiredHereStaysExpiredWhenTheClockIsSetBack`, `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects`; waiting not covered |
| Removed machine | Refused as a peer once known; its records kept, marked | `TestMessageOfARemovedAuthorReachesAMachineThatNeverMetItMarked`, `TestHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited` |
| Forked log key | First branch kept, evidence passed on, alert | `TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember`; the alert not covered |
| Forked record stream | Second record refused and kept as evidence; nothing shows or passes it on, and there is no alert | Q8 checks closure only; showing, passing on and alerting not built |
| Relay alters or strips | Refused, or text repaired | `TestRelayThatAltersARecordIsRefused`, `TestRelayThatStripsTextAndClaimsWithdrawalCannotLoseIt` |
| Peer withholds | Other streams progress; rotation reaches other holders | `TestARefusedStreamDoesNotStarveOthersUnderAPageLimit` covers paging; rotation not covered (G4) |

Where this table says "not covered", no model test covers the case. Tests
of the daemon's code for each fault are in the [fault matrix](TESTING.md#fault-matrix).

## 9. How this was built

The delivery core was built in seven steps, each ending with `make check`
passing: `core` records and entries with golden vectors and fuzzing; the
membership model promoted to `internal/members`; storage; the room log,
invites, join and requests; pull, offers and the scheduler; the final tool
definitions and the web page; and the differential test against the
delivery model with the measurements. The new code was built beside the
v0.2 daemon and the surfaces were switched over in one change, so rooms
with other machines kept working throughout. [Decisions](DECISIONS.md)
D27 to D34 record each step.

## 10. Known gaps and limits

These are the design review's findings. See
[Requirements section 6](REQUIREMENTS.md#6-limits-in-plain-words) for the
user-facing limits.

| Skipped test | Response |
|---|---|
| `TestAuthorLearnsOfCopiesOnMachinesItNeverMet` (G1) | Accept: copies only from direct contact |
| `TestNewcomerWithoutHistoryIsNotHandedPostsItsAdmitterHadNotSeen` (G2) | Accept as the stated T3 exception |
| `TestBranchUnderAMessageIncludesRepliesBelowAMissingReply` (G3) | Accept for stage 1; the thread view lists them |
| `TestWithholderThatUnderstatesWhatItHoldsIsDetected` (G4) | Accept; rotation narrows it |
| `TestCopyReportsDoNotOutliveAHolderRestoredToBeforeTheCopy` (G5) | Accept; reports carry dates |
| `TestWithdrawThatOnlyTheRemovedAuthorHoldsStillReachesMembers` (G6) | Accept: what a removed machine wrote that no member still allowed to relay fetched is lost |
| `TestAMoreRestrictiveGrantLearnedLaterDoesNotTakeBackTextAlreadyShown` (G7) | Accept; a notice is to be modelled next |
| `TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval`, `TestStolenKeyCanSecondAForkingKeysVoidedRemovalAndThenJoinTheRemoval` (members gap 4) | Accept; no rule can refuse it |

Members gap 1 is closed by [M12]; gaps 2, 3 and 5 are passing tests of
stated limits.

What remains partly resolved, as REQUIREMENTS section 6 limits:

| Finding | Limit |
|---|---|
| Removed machine's posts relayed by a stale member, marked; offers before a removal is known | "Removal is not instant" |
| Restored admin pulls from a stale peer and reuses a position | "An admin machine restored from backup must catch up…" |
| A sole admin's fork never settles | "While a room has had only one admin, that key is the room" |
| Too many heads | Planned addition to section 6 |
| A machine restored to a copy in which the peer asking it is not yet a member refuses that peer until it has caught up by its own pulls; if it knows no current member, it cannot | It joins again with a token (`TestARestoredServerPagesTheLogFromAMemberThenServesANewPeer`). See section 6 |
| Two records signed by one author at one position look equal in summaries, so machines can hold different branches unnoticed and a copy report for that author can be wrong | Only a faulty or dishonest author machine can do it, since a restore starts a new stream (`TestKnownGapForkedStreamsLookEqualAndGiveAWrongCopyReport`) |
| A removed machine is not told; its pulls are refused with an answer that says nothing | It sees refusals from every member; `poll` on its invite says `removed` (PROTOCOL 5.3) |

Open: P3 against a same-user agent; stream state grows until compaction is
designed (1,920 streams per 64-machine room after 30 days, from 64
machines of the delivery model, not real nodes;
`TestStreamCostForSixtyFourMachinesOverThirtyDays`, which runs with
`A2P2_MODEL_DEEP=1`; the real-node size test seeds 62 authors, 1,860 streams); one pull per peer for
all shared rooms may be needed at 100 rooms; posting policy (T6) is
deferred to stage 3 and needs a new entry kind [M14].

Whoever can edit the home is the user: editing the recorded networking
decision or `portmap.json` by hand can widen the listener or point a
cleanup at a forward, and no check here is meant to stop that.

## 11. Reachability

**Can this machine talk to others, and why not (`a2p2 net check`).** A
person learning that a laptop drops every inbound connection should not
have to ask a colleague. `a2p2 net check` reports the networking state,
whether the listener is bound, the addresses advertised, and for every
other member of every room each known address tried now: a TCP connection,
then the pinned TLS handshake, closed without a request, with the outcome
(reached, refused, timed out, wrong key, intercepted, not an a2p2 machine,
unreachable) and the time taken. It ends with one sentence saying what to do. It
observes and decides nothing; with networking off it dials nothing.
`a2p2 net check HOST:PORT` and `--invite` test addresses before any room
exists, from the CLI process under a throwaway key (PROTOCOL 4.7).

**Everything a2p2 contacts.** Members' addresses (pulls, files, dial-back,
and an invite's addresses at a join), only while networking is on or
outbound only; the local router after `net on --map-port`; the address a
person names to `net check HOST:PORT` or `--invite`, from the CLI and
whatever the networking state; and, for `install.sh` and `install.ps1`,
GitHub's release page (and GitHub's API through `gh` when
`A2P2_VERIFY_ATTESTATION=1` asks for the attestation check), run by a
person and not the daemon. No other code opens a
connection: no update check, no telemetry, no lookup of its own (D11, with
its 2026-11-25 note; [SECURITY](../SECURITY.md#what-a2p2-connects-to)).

**Two diagnoses.** The checks accept and refuse as before; only what the
tool says differs. When the far end presents more than one certificate,
or a leaf that holds another kind of key or is not signed by its own key,
and the leaf's subject is a machine's key, the certificate is not the
other machine's own: the result is `intercepted`. The line shows the
issuer the certificate claims. That name is whatever the other end wrote,
so it is cut to 64 characters, every character other than a letter, a
digit, a space and `.-_,()&'/@:+` is shown as `?`, and the text is shown
as claimed, never as verified. The advice says the certificate is not the
other machine's own, that it most likely comes from a TLS-inspecting proxy
on this network that re-signs connections, that a2p2 refuses such a
connection because it could not tell the other machine from whatever
answers, and offers: use a2p2 between machines inside the network, ask
whether the destination can be exempt from inspection, or connect from
another network. Bare `a2p2 net check` ends with the same sentences, in
a short form without the options, when no member could be reached and a
member's address was intercepted. A join or a pull that meets the same
condition fails with the same substance. A server whose certificate does
not name a machine's key (a web server on the wrong port) and a plain TCP
answer stay "not an a2p2 machine". When the dial fails with "no route to
host" (`EHOSTUNREACH`; `WSAEHOSTUNREACH` on Windows) within 250 ms, at an
address inside a subnet attached to one of this machine's interfaces (up,
not loopback, not point-to-point, with a plain mask longer than 0 and not
link-local), the system has the route and refuses it at once, so the
advice says a VPN or firewall on this machine is probably blocking
local-network access. A neighbour that does not answer fails after
seconds and keeps the ordinary advice; so does a link-local destination,
whose zone is unknown. The same sentence ends the daemon's check when no
member could be reached and a member's address failed that way. Which
interface holds the default route is not read: it is not available
cheaply on every platform, and a tunnel interface name proves nothing.

**The dial-back.** Only another machine can say whether this one is
reachable. A member asks a member to connect back to the addresses it
advertises and hears which of them completed a handshake with its own key
(PROTOCOL 4.7). The responder dials only for a current member of a shared
room, only expecting that member's key, and only the IP address the
request came from, on at most two ports (the listening port and a mapped
external port); any other address it answers as not tried. So an IPv6
address is checked by asking over IPv6, and the daemon asks about each
address family it advertises only members it reaches over that family;
`net check` says "not checked: no member reachable over IPv6" when there
is none. It sends no request after the handshake, answers no sooner than
its deadline unless every address dialled was reached, and limits
requests per key and for the whole machine, so it adds no way to aim
traffic at a third party; the one limit left is that a member behind a
shared address can cause two connection attempts to that address per
request. The daemon asks after each run starts (so after each networking
decision) and then hourly; `status` and the page show "reachable by
peers: yes, no or unknown" with when it was checked. The rules are in `node.DialBackServer`
and `node.Reacher`; the dialling is `peer.Prober`.

**Members that connect in.** In the common setup (one machine listens,
laptops connect out only) no member can be dialled, so no dial-back can
run, yet the listener has direct evidence: a member completed a pinned
connection to it. The daemon keeps in memory, per key TLS proved, the
last such connection and the local address it arrived on (nothing on
disk, no new message). While a current member of a shared room connected
in within the last 10 minutes, "reachable by peers" is `yes`, saying so
("bfb81469 connected in 5 s ago"), unlike a dial-back's "reached
ADDRESS". That proves the member reaches the local address it used and
says nothing about the other addresses advertised; a dial-back result,
where one exists, is still shown. A key in no shared room, or one
removed since, counts for nothing. `net check` shows such a member as
"connects in (last 5 s ago)" rather than as a problem, counts it in its
advice ("1 of 1 members connects in to this machine; nothing to do"),
and tells a person to check outgoing connections only when a member has
a known address and none could be reached.

**More machines reachable without a third party.** N1 lets a machine that
cannot accept connections take part through a member that can. Two
machines behind ordinary home routers have no such member. Four things
widen who can be reached, none of them a service run by anyone else.

- **Not classic hole punching.** Punching needs a rendezvous machine that
  both unreachable peers reach at the same time. In a2p2 such a machine is,
  by construction, a member that already carries their messages, so the
  direct path would add much code (simultaneous opens, keep-alives, NAT
  behaviour classes) to save one hop that costs nothing. It is not done.
- **IPv6.** Many home networks hand out global IPv6 addresses with no NAT,
  only a firewall that may let a port in. Under a networking decision taken
  with this version, a listener on the wildcard address (`0.0.0.0` or `::`)
  is one dual-stack socket, and `a2p2 net on` says so and names the IPv6
  addresses it will advertise. A decision recorded before keeps the scope
  it was taken with: an IPv4 listener and no IPv6 address, until the
  person runs `a2p2 net on` again; `status` and `net check` say so. An
  `advertise` entry the listener cannot serve under that scope (an IPv6
  address under an IPv4-only decision) is not advertised, and `net check`
  says why. A person who wants IPv4 only gives `listen` one IPv4 address
  of the machine. The machine advertises its IPv4 addresses (not loopback, not
  link-local) and its global IPv6 addresses, never link-local ones, and a
  temporary (privacy) address only if the interface has no stable one.
  Unique-local addresses (`fc00::/7`) are advertised after the others, and
  every dial path (pulls, dial-backs, probes, joins) tries one only if this
  machine holds an address in the same `/48` now, so a peer elsewhere never
  waits on an address it cannot route. Which addresses are temporary is
  read from the system where it says so (Linux `/proc/net/if_inet6`, the
  macOS `SIOCGIFAFLAG_IN6` ioctl); where it cannot be read (Windows) every
  global address counts as stable. At most 8 addresses are advertised: a
  verified mapped one first, then IPv4, then global IPv6, then
  unique-local. A machine's own list in a pull replaces what was learned
  from it before; an empty list changes nothing, so a listener that once
  advertised nothing is not forgotten by members that can reach it only
  at a stored address. The limit: an address of a machine that other
  members passed on (not its own word) stays in a book, tried last, until
  it has failed for a day and five attempts; and a machine that stops
  listening altogether is still dialled at its last list until then.
- **The address others see is shown, never advertised.** The dial-back
  answer carries the IP address the responder saw the request come from
  (`saw`). `net check` shows it ("members see this machine as") and
  compares it with the router's external address to detect a second layer
  of translation. It never becomes an address to advertise, even after a
  dial-back to it succeeds: the member that reports it is the one that
  would test it, so one lying member could make this machine publish
  someone else's address to every other member. A person who forwarded a
  port by hand says so with the existing `advertise` key, and `net check`
  prints the exact line when members see a public address this machine
  does not advertise.
- **Port mapping, by a person's decision.** `a2p2 net on --map-port` asks
  the router of the local network to forward the listening port: PCP
  (RFC 6887) first, NAT-PMP (RFC 6886) if the router answers PCP with
  NAT-PMP's version refusal, then UPnP IGD (SSDP, then `AddPortMapping`
  on the gateway's `WANIPConnection` or `WANPPPConnection` service). Only
  the standard library is used. The gateway is the default route's next
  hop, read from the system (Linux `/proc/net/route`, the route with the
  lowest metric; the macOS routing table, the unscoped default route);
  with several uplinks only that route's router is asked, so a mapping on
  another uplink's router is never made. On Windows mapping reports itself
  unavailable. This machine's address on that network is the source
  address the system selects for the gateway (a route lookup that sends
  nothing), not the first address in the gateway's subnet. A mapping is
  made only for a listener bound to the wildcard address or to that
  address,
  so the router never forwards to another service that holds the port on
  another address, and it ends with that listener, however the listener
  ends. Answers are checked before they count: the UDP source must be the
  gateway's port, a PCP answer must carry the request's nonce, a grant
  must have a lifetime, an external port and an address, an SSDP answer
  must be a 200 for a gateway device, a SOAP answer must be a complete
  response, and a SOAP fault counts as a refusal (one that moves to the
  next port, asks for a permanent lease or counts a removal as done) only
  if it is a complete fault with a UPnP error code; a UPnP description or
  control URL is followed only if its host is the gateway. NAT-PMP and SSDP carry no authentication, so a machine on
  the same network that spoofs the gateway's address can still answer
  first; what it can obtain is a wrong mapping shown in `status`, not a
  connection to anything but this machine's listener. The mapping asks for
  two hours, is renewed at half of what was granted, however short, and a
  permanent UPnP lease is asked for only when the router refuses finite
  ones. The external address the router reports is the only candidate for
  advertising: it is advertised after a member reaches it, while a member
  has reached it within three hours, while the mapping exists and while
  its lease has not run out, whether or not a renewal is being tried. A
  new mapping, or one that comes back, starts unconfirmed; one failed check
  does not withdraw a confirmed address, two in a row do. A mapping that
  keeps changing asks members to check at most every five minutes. If it is
  not the address members see, the router is behind another NAT
  (carrier-grade NAT or a second router), and `net check` says the
  mapping cannot help. An invite carries that address whether or not a
  member has reached it, because a room's first invite is issued before
  any member exists to reach it: it goes first in the token, ahead of the
  advertised addresses, and keeps its place when the 8-address cap cuts
  the list. The joiner dials a token's addresses in order, 3 seconds each,
  and a joiner outside the network would otherwise wait out the private
  ones; a wrong address costs a timeout, since the key's fingerprint is
  checked before anything is sent. What the machine itself advertises and
  asks members to dial back is unchanged. A joiner keeps a token's
  addresses as it keeps any it learns and may pass them to other members
  as hints, so the mapped address can reach them that way before it is
  confirmed; it is the machine's own router's answer, never another
  member's claim, and if it does not work it costs a failed dial. An
  invite still needs an advertised address: a mapping alone issues none,
  so a listener that failed cannot hand out a mapping it no longer has.
  Mapping is a decision like turning networking on:
  asked at a terminal, saying that it opens a port on the router to the
  internet and that only room members get anything from it, and recorded
  in the networking decision. It is never on by default and no agent tool
  or hook can ask for it. `status` and `net check` show the protocol, the
  external address, whether a member reached it, and when it expires.
- **Mappings are always cleaned up, or said not to be.** Before each
  request that could create a mapping, everything that removing it needs
  (the external port among it, even before the router names an external
  address) is written to `portmap.json` in the home, durably (a new private
  file, synced, renamed, the directory synced), and it stays there while
  it may exist:
  after a failed renewal, after a NAT-PMP or UPnP grant whose external
  address could then not be read, after an answer that cannot be used. It
  is removed when the listener stops, on `net off`, on a decision without
  `--map-port` and on a clean stop, and at every start, whatever the
  decision, for what an earlier run left. A removal that fails is shown in
  `status` and `net check` and retried every five minutes, at stop and at
  the next start. What a crash leaves: a finite mapping until its lease
  ends (at most two hours), and a permanent UPnP rule, if the router
  granted only that, until a2p2 next starts on that network and removes it,
  or until a person deletes the rule described `a2p2` on the router's own
  page. The record is used whole or not at all: a field out of range or
  unknown, a mapping on another router than this network's (after a move),
  or a file that does not parse, and nothing is sent to any router, the
  file is kept, and `status` says the record cannot be used and what to
  remove by hand. A deletion reaches only this machine's own mapping: PCP
  by the nonce kept from the request, NAT-PMP by the sending host's
  internal port, and a UPnP forward only after the router says it points
  at this machine's address and port with the description `a2p2`; one that
  does not is left alone.

Interfaces and gateways are read through `internal/daemon` (build-tagged
per system); the mapping protocols are `internal/daemon/portmap` and every
test of them runs against fake PCP, NAT-PMP and UPnP responders on
loopback. No test or check sends anything to a real router.

## 12. Attachments

How a message carries files (requirement L1), added in 0.2.0. **[F1]** to
**[F12]** are rules of the
[delivery model](../internal/model/delivery/FINDINGS.md#attachments)
(`attach.go`) that the code is tested against; formats are in
[Protocol section 10](PROTOCOL.md#10-attachments).

1. **The header says only that there are files.** A `message` or `replace`
   with files has one optional top-level header field, `files`: the
   SHA-256 of its manifest. Nothing else, not even a count. The manifest
   (a random salt, then name, size, SHA-256 and media type of each file)
   is content, like the text. Why: a machine admitted without history
   holds earlier headers, and headers outlive a withdrawal, so names,
   sizes and hashes in a header would reach machines that may not read
   the text. The salt keeps the hash from confirming a guessed file list.
   0.1.0 stores a record with an unknown top-level field, keeps it in the
   id, relays the exact bytes and shows the text without files (run
   against the 0.1.0 code). A malformed `files` is stored as 0.1.0 stores
   it and ignored when read, so machines never disagree about the room
   (D27).
2. **The manifest travels like a file, at once [F4, F12].** A machine that
   may read the text fetches the manifest (at most 16 KiB) after the pull
   that brought the header, on either side of it (the exchange asks the
   files part at once rather than waiting for its one-minute tick): it is
   U4's cheap first step. The first ask that finds no list is not a failed
   answer and is repeated once after 5 s, since a member may be getting
   it from an author only it can reach. A want ends a pull of its author
   held here, so it travels at once, and an agent's fetch with a wait
   keeps waiting while the file is coming. Until it arrives a
   reader sees "has files; list not here yet". After 8 failed answers,
   with backoff, it is `unavailable` until a person or agent asks again. A
   manifest that matches its hash but breaks a rule is `invalid` for good,
   since no holder can have other bytes, and nothing it lists is fetched.
   File bytes move only when an agent that was shown the version listing
   them asks (on the author's machine too, except the agent that wrote
   the version fetched), or the machine carries or keeps them (item 5). The page fetches as
   the person's own id, so it serves a file only of a message one of its
   views (inbox, history, thread) has shown; those views mark it shown
   without marking it seen.
3. **Content lives while the text may [F1, F8].** A machine keeps the
   manifest and the files for a message while the message's winning
   version (by header) commits to them and its text is one the machine may
   keep: not withdrawn, not past 30 days by its own clock, within its
   permission. Storing a withdraw, a replacement that does not list a
   file, passing the 30 days, learning a narrower grant or a voided
   admission deletes the bytes, partial downloads and agents' copies, which
   are scoped to the message they were authorised for (D43). The deletion
   runs in its own sweep (`SweepFiles`) after each pull, each replace or
   withdraw written here, each background tick and at start, not inside the
   record's transaction. Serving, fetching, uploading and publishing a copy
   each ask the rules again from rows when they act, and a response or
   upload asks before every read of at most 64 KiB, so a sweep that has not
   run yet widens nothing (D49). A
   replacement shares its files with whoever may read it, exactly as it
   shares its text, so nothing is carried forward silently: a replacement
   lists only files its writer lists again, and a person confirms them.
   Keeping a file means naming verified bytes held for the version being
   replaced; approval checks again that this version still wins, is still
   readable here and still lists the file, or the request fails and its
   staging goes. While a replacement's manifest is on its way, what the
   machine holds for the message stays, unserved, and never longer than an
   earlier version that lists it may be kept.
4. **Served under the text's permission [F2].** Bytes, manifests included,
   go only to a peer the sender sees as a member now that may read the
   winning version's text in the sender's view, and only bytes the sender
   holds for that message (it wrote it, or received them for it). Every
   refusal is the same 404 with the same body, decided from rows before
   any disk access. Each chunk is checked again, and an active transfer
   stops when the sender learns of a revocation. Why the second
   condition: a member that learned a hash elsewhere could list it in a
   message of its own (`TestANewReferenceToAKnownHashDoesNotWidenWhoIsServed`).
5. **Uploads from the author, and carry on request [F9].** A machine puts a
   message's files in `want` only in its answer to a pull from that
   message's author, and only when it cannot dial the author. The author
   knows its own files, so a want discloses nothing, and nobody promises
   bytes it does not hold. A want is bound to the message, the winning
   version and the hash, reserves its size, and lapses after a day or
   three answers without the bytes; the receiver takes an upload only
   from the author against a want it issued, and checks before reading
   the body. In a room where laptops only dial out and one member
   listens, that member is the path: asked by a permitted peer for a file
   it may keep but lacks, it answers "not yet", starts one carry job (a
   want to the author), and holds the file within its budget while the
   text lives. A lapsed job is answered "gave up" for a day, and the
   reader is told `unavailable`. The author is never carried for: it
   wrote the file, and asking for it would only hold a member's budget.
   Carry jobs and `keep` wants of one author's files that are still
   live reserve at most a quarter of the room budget, each counted by the
   bytes not yet received, so an author that uploads one byte of each
   file or none cannot hold the rest; a part left by an upload goes when
   its want lapses or an edit ends the want, since nothing can finish it
   then. A want this
   machine's own agent asked for is not capped. A per-room setting has three values:
   `off`, `carry` (default, since every member already carries the room's
   text for others) and `keep` (want every file); the CLI asks for a
   change at a terminal, as for any decision of the person.
6. **Nothing unverified is exposed [F3].** The declared total must equal
   the manifest's size (at most 16 KiB for a manifest) before a byte is
   taken; a partial download is hidden; the whole must match the SHA-256,
   or it is discarded and the peer counted (`bad_file`). Permission and
   "shown this version" are checked again before a copy is written for an
   agent.
7. **a2p2 never opens a file.** It writes the verified copy, mode 0600,
   under `$A2P2_HOME/files/<room id>/` with a sanitised name and never over
   an existing file. The copy is published as a hard link from a
   temporary file, so a file system without hard links cannot take copies.
   Names are not Unicode-normalised: a combining mark becomes `_`. It does not decompress, preview, render or run it.
   Tool results say a file is data from another machine.
8. **A person confirms the exact bytes [F6, F11].** `send` with `files`
   is refused while the room's requests are full before any path is
   touched. Then each path is checked by name before anything touches
   it: on Windows only a drive letter, a colon, a separator and plain
   names pass (no UNC path, namespace, stream or device name); on Unix
   nothing under `/dev`, `/proc`, `/sys`, `/net` or, on macOS, another
   automount point. Every link on the way is read and its target checked
   the same way before it is looked at. Each component is looked up in
   the directory opened before it. The file is then opened once, only if
   it is a regular file and still the entry checked, and refused if it is
   one of a2p2's own: nothing under the home but agents' copies in
   `files/`, and no file that is the same file as one in the home outside
   `files/` (the machine key, the local key, the database, a blob), a hard
   link included. Approval checks a pending request again, by its path
   and by the bytes of the keys, so one an older binary staged from them
   is not sent. Sizes, the path shown and the bytes staged come from that
   handle. The same bytes listed twice are listed once. At most four sends
   stage files at a time. The request shows each path as resolved from the opened file, in full,
   with control, bidirectional and invisible characters escaped
   (`EscapeDisplay`, PROTOCOL 10.8), its size and
   SHA-256, the room, the recipients, and for a replacement which message
   it replaces. A file with several names on disk is said to have them:
   the path shown is only the name opened. The CLI and the page show the whole text, every line
   escaped, and send back the terms digest they showed; an approval
   without it or with other terms is refused (`terms_changed`) and
   decides nothing. There is one way to approve a send with files
   (`Files.ApproveSend`, with the terms); the general confirm path can
   only decline one. For "everyone" the terms bind the set of members
   when the agent asked (a digest of their sorted keys) and show its
   size; an implicit reply's terms name the parent's author it goes to;
   a replacement's terms and display name the audience of the message
   it replaces (a reply sent to everyone stays a send to everyone). If
   the member set, read in the publishing transaction, differs from the
   one approved, nothing is sent and the request ends `terms_changed`.
   The whole record is built and checked when the agent asks, before
   anything is staged, and again at approval before any byte is copied
   into place; a publication that still fails queues the blobs it wrote
   for deletion in the same step, counted against the budget until they
   are removed. Every queued blob carries its size and counts until its
   file is known absent: from the removal itself where a removal is not
   known durable until a start, and again whenever a run or a start finds
   the file there and cannot remove it. The request's expiry is checked by
   the store's clock before any byte is copied into place and again in the
   publishing transaction. A replacement's eligibility is decided by the clock read
   in the publishing transaction. Approval re-checks the staged digests and sends them;
   the source is never read again. Declined, expired after 24 hours,
   failed, or after this machine's own admission is voided, nothing is
   written and staging is deleted.
9. **Bounds and cleanup [F7, F10].** At most 8 files per record, 256 MiB
   per file, 1 GiB of held, partial, staged and copied bytes per room and 4
   GiB per machine. A reference over a limit is refused before anything is
   scheduled; the budget is reserved at the declared size before the first
   byte. One transfer per peer per direction and 8 in all, with a total
   time limit and a minimum rate, under limits separate from pulls. Bytes
   and their directory are synced before the row that makes them live (on
   Windows a directory cannot be synced from Go, so a2p2 relies on NTFS's
   journal and repairs at start what a crash can lose); a
   send's record and its file rows commit in one transaction after its
   bytes are safe, so a send is never published without them. Temporary
   files have a recognisable name, `.a2p2-tmp-` and 24 lowercase
   hexadecimal characters; only a name of exactly that form counts as one.
   The snapshot a page download serves is such a file directly under
   `blobs/`, never in the system's temporary directory, so one a crash
   leaves is removed at the next start. Deletions are by rows, queued in the
   sweep transaction that decides them (item 3), and never remove a path a live row
   names; a copy's deletion removes only the file a2p2 linked there (by
   its file identity), and an entry leaves the queue only after its
   directory sync succeeds, even when the path is already gone. A start
   completes the queue, drops every partial transfer, which starts again
   from zero, and removes only what carries a name a2p2 generates (a
   blob, a part, a request's staging, a temporary file) that no row
   names; any other file under `blobs/` is left and counted in `status`
   as unknown. An orphan staging directory loses only the files a2p2
   names there; if anything else is left, it and the directory stay and
   are counted. Where a directory cannot be synced (Windows), a removal
   stays queued until a start finds its path gone; an entry whose path
   holds a file a2p2 did not publish is finished at once. Bytes are never adopted unverified: a file already at a
   blob's path is hashed before it counts as that blob, and replaced by
   the verified bytes if it does not match; a start hashes every held
   blob and removes one that fails, to be fetched again. A held blob the
   start could not examine (any error but its absence) keeps its row and
   is hashed, through the handle that serves it, the first time it is
   opened; it is served only if it matches. Serving does not otherwise
   hash again. A request names the file's hash in its path, so a fetch,
   a list fetch and a carry go only to the author or a member this
   machine can affirm may read the message and its winning version, by
   the rule the serving side applies, asked again immediately before
   each request (a fetch holds a lease on it). The clock for a promotion,
   and for choosing the wants of a pull answer, is read inside the
   transaction; a want past its life is not advertised before the sweep
   lapses it. Answers from a machine that is no longer a member do not
   count in a fetch's state. Permission is decided once under one view
   of membership and rows, held by the transfer and asked again in the
   transaction that makes bytes or a copy live. A commit that changes a
   room's membership is counted from before the commit until its new
   state is published in memory, and a decision waits while one is in
   progress, so no lease is taken against membership older than the
   database; each fetch writes and checks a new copy.

**Who can reach whom.** Text and files take the same connections; what
files add is item 5.

| Author | Reader | How the file arrives |
|---|---|---|
| listens | listens or dials out | the reader fetches from the author |
| dials out | listens | the answer to the author's pull carries the reader's `want`; the author uploads (`TestAReaderThatListensGetsAFileFromAnOutboundOnlyAuthorByUpload`) |
| dials out | dials out, one member listens | the listener wants the manifest and then, asked by the reader, the file, from the author's next pull; the reader fetches from it (`TestInARoomWithOneListenerAFileFromAnOutboundOnlyAuthorReachesAnOutboundOnlyReader`) |
| dials out | dials out, no listener | no path, as for the text |
| asleep or gone | any | any member that fetched, carried or keeps it and can be dialled; otherwise `coming` until a want lapses, `unavailable` from every member asked (`TestAFileWhoseOnlyHolderIsGoneIsUnavailableAndTheReaderIsToldSo`) |
| any | asleep | asks when it wakes |
| networking off | any, or any and networking off | nothing moves to or from that machine |

The listener does not help when it is set `off`, when the file is over its
budget, or when it may not read the text; the reader is then told
`unavailable` (`TestWithCarryOffTheListenerAnswersNotHeldAndTheFileStaysUnavailable`,
`TestAListenerOverItsBudgetDoesNotCarryAFile`,
`TestAListenerThatMayNotReadTheTextNeitherCarriesNorReceivesTheFile`). A
copy held only by a machine nobody can dial, other than the author, is
not reachable (`TestAWantGoesOnlyToTheMessagesAuthorNeverToAnotherHolder`);
under `carry` the listener asked first already holds it. A reader is told
`here`, `partial`, `coming`, `not asked`, `fetchable`, `unavailable` (who
said no or gave up, when), `list pending`, `list unavailable`, `list
invalid` or `gone` and why.

**Mixed versions.** A 0.1.0 writer's replacement has no `files`, so it
drops the files, as a 0.2 replacement without files does. 0.1.0 shows the
highest version whose text it holds, while files follow the winning
header; when a newer header arrives without readable text the two can
differ, and the reader is told the files are gone (`why: live`). A first
record of a stream may have a header of 8,192 bytes; `files` adds 75.

**Limits, said plainly.** The confirmation is a guard rail (P3): an agent
running as the same user can call `confirm` itself, or read a file and
paste it as text. A send with files tells the agent only the request and
that it waits, and refuses every path that cannot be staged with one
message, but an agent can still learn that a path can be staged at all,
which its other tools could tell it anyway. Below the request limit, a
send that fits raises a request the person sees and one that does not
fit is refused, so an agent can learn roughly how large a file is, and
each guess that fits shows the person a request naming the file. Withdrawing after others
fetched removes the copies a2p2 keeps; a copy an agent moved or changed is
out of reach (a copy is known by its file number and modification time).
A removed machine keeps what it fetched (T5). F2 binds honest machines: a
member holding bytes can hand them to anyone. A member that may read a
message can tell from the answers ("not yet", "gave up") whether a
listener carries or keeps; machines that may not read it cannot. A
refusal's timing is not made equal: a member that may read a message can
tell from how long a `not_held` takes whether this machine holds its
header. After a revocation, one buffer may still be written in the 25 ms
the lease takes to cut a response: bounded, and less than a machine that
has not yet learned of the revocation serves (T5). Likewise a fetch asks
its rule immediately before each request; a removal that commits between
that check and the request's write is ordered as if it arrived a moment
later. Every version of one
message that carries the same commitment shares its list, since the hash
makes the content identical. A path to attach is walked one component
at a time, each looked up in the directory opened before it (`os.Root`)
and opened only if it is still the entry checked, so a directory or a
link swapped in on the way after its check is refused and never
followed. Links are read and checked before they are followed; the person
is shown the path of the file actually opened. A network file system the
person mounted, or a drive letter mapped to a share, is used as any
local path: on Windows the handle of such a file reports
`\\server\share\...`, which is taken only for a path that began with a
drive letter the system reports as a network drive.
A cloud placeholder file can download its bytes when read. A process that can write inside the home can race a2p2's own
file operations: the home is the person's. On Windows a power loss can
undo a rename that was not flushed; a start reconciles, and a blob that
is gone is fetched again. An upload goes only to a machine that asked
for that file by hash, so a member removed at that instant learns nothing
from it. While an approved send is being published, its staged copy and
its new copy exist together: a machine needs free space for twice the
size of a send, and its pulls may wait until that copy finishes. A send
declined or expired after approval's first check of its expiry can still
cost one copy of its files, which is then removed; one already expired
when approval starts copies nothing. On Windows a download's snapshot
still open at a start cannot be removed then and is left for a later
start.
Attacks on the program that later opens a file
are out of scope, since a2p2 opens nothing.
