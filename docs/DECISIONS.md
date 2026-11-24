# Decision log

Append-only. One entry per decision: what, why, and what would change our mind.
Supersede an entry by adding a new one that names it; do not rewrite history.

The entries are dated history. File and package names in an entry are
those of its date and may since have moved or been renamed.

## D1. Build a small new core; use existing projects as reference only (2026-11-22)

Three independent static audits (Codex on open-agent-bridge and
agents-connector, Antigravity on agentmesh, Claude on agentmesh) reached the
same verdict. No candidate has authenticated rooms or paired cross-machine
transport; the strongest local experience (open-agent-bridge, ~22k lines of
TypeScript) depends on private harness sockets and has client-side-only room
filtering; all have under ten GitHub stars and a single maintainer.
**Revisit if** a maintained project appears that meets must-haves 2–4.

## D2. Go, not Rust (2026-11-22)

The initial brief proposed Rust + Iroh. We chose Go because: the standard
library alone provides TLS 1.3, Ed25519, HTTP, embedding and structured logging,
so the peer transport needs zero third-party code; agentmesh demonstrates the
exact pattern in ~250 lines; it produces one static binary with trivial
cross-compilation; and the toolchain is already installed where the project is
developed. Iroh's default relay behaviour is a footgun against the
"no hosted infrastructure" requirement.
**Revisit if** NAT traversal becomes a real requirement (then evaluate QUIC
hole punching), not before.

## D3. Mutual TLS over TCP with Ed25519 node keys; plain HTTP+JSON on top (2026-11-22)

Node id is the public key. Certificates are self-signed; trust comes from
pinning keys learned through invites. HTTP gives request/response framing, so a
`stored` receipt is the response body and there is no custom wire protocol.
The same HTTP+JSON style serves the local Unix-socket API.
**Revisit if** measured throughput is inadequate or one-directional
reachability (only one side can accept connections) becomes common; the fix is
a long-poll pull endpoint, not a new transport.

## D4. The node is the security principal; agents are labels (2026-11-22)

Per-agent credentials on a single-user machine would be theatre: any local
process can read the database. Room membership is per node. This removes a
whole layer (agent tokens, per-agent ACLs) from the design.
**Revisit if** multi-user machines or sandboxed agents become a target.

## D5. Owner-authoritative room membership, no signatures (2026-11-22)

One writer per room avoids consensus. Because nodes never forward for each
other, the TLS connection already authenticates the author of every message and
every membership update, so per-message signatures add nothing in v1.
**Revisit if** store-and-forward or relaying is ever added: forwarded content
must then be signed by its author.

## D6. Outbox retry is the only recovery mechanism (2026-11-22)

The brief sketched a `sync_since` protocol with high-water marks and gap sets.
With a durable outbox, in-order batches per destination and idempotent inserts,
retry already yields eventual delivery, so sync is omitted. `seq` is still
recorded so tests can assert there are no gaps.
**Revisit if** history backfill for late joiners is wanted.

## D7. Every room member receives every room message (2026-11-22)

`to` is addressing, not access control. The room is the privacy boundary, as in
any chat room. This removes per-agent routing and a distributed agent directory
from the delivery path.

## D8. Delivery modes are `pull`, `next_turn`, `wake`; default is the first two (2026-11-22)

Hooks at turn start and turn end exist in all three target harnesses
([harness notes](HARNESSES.md)). Waking idle sessions is harness-specific and partly
experimental, so it is an opt-in adapter feature, never core behaviour.

## D9. Dependencies (2026-11-22)

`github.com/modelcontextprotocol/go-sdk` (official, tracks the spec),
`modernc.org/sqlite` (pure Go, no cgo), `pgregory.net/rapid` (tests only).
mDNS (`github.com/brutella/dnssd`) only when M3 needs it. No web framework, no
ORM, no logging library, no CLI framework. A new direct dependency needs an
entry here.

## D10. License: MIT (provisional) (2026-11-22)

Matches every project in this space. The owner should confirm before the first
public release; Apache-2.0 is the alternative if an explicit patent grant is
wanted.

## D11. No self-update, no telemetry, no network calls except to room members (2026-11-22)

A tool whose pitch is "nothing hosted" must not phone anywhere.

## D12. Members pull the member list from the owner; nothing is pushed (2026-11-22)

Supersedes the push wording in D5. Review found that pushed updates race with
data (a new member's first message arrives before the update and is rejected
forever). Now: members pull periodically and on evidence of a newer version
(each delivery batch carries the sender's version per room), and a message from
a not-yet-known sender is answered `retry`, not `rejected`, when the sender
claims a newer list. Removal is effective per node when it pulls. The owner
needs no durable push queue.

## D13. Dedup is keyed on the authenticated sender; local sends take a caller id (2026-11-22)

A repeat is a success only if id, room, sender node and seq all match; otherwise
`rejected`. Without this a member could reuse another message's id and get a
false `stored`. Locally, the caller may choose the message id so a retry after a
lost response does not create a second message.

## D14. `seq` is one counter per node, gaps allowed (2026-11-22)

Supersedes the per-room counter in the brief and in D6's "no gaps" remark.
Order is enforced by one in-flight request per destination and oldest-first
batches, not by gap detection, so a single counter bumped in the send
transaction is enough and removes per-room counter state.

## D15. Reading is `inbox` then `seen`; only `handled` receipts cross the network (2026-11-22)

A read cursor that advances when the daemon answers loses messages if the
answer never reaches the agent. Unread is now "no `seen` mark for this agent",
confirmed by the caller after hand-over. `seen` stays local (it leaked reading
activity to the whole room and added traffic for no decision anyone makes);
`handled` goes only to the author's node.

## D16. Invites are bound to the first redeeming node key and are retry-safe (2026-11-22)

Redemption is one transaction; the same key redeeming again gets the same
answer. The token is read from stdin. We do not add an owner-approval step in
v1: the token is the approval, and the design says plainly that it is a bearer
secret. **Revisit if** rooms are used across trust boundaries where the invite
channel cannot be trusted.

## D17. No mDNS, no SSE (2026-11-22)

Supersedes the mDNS line in D9. Pairing already needs an explicit address, and
members report their current addresses on every pull, which covers address
changes without multicast. The UI polls every two seconds; `inbox` long-polls
through an in-process channel. Both deletions remove a dependency or a
long-lived-connection code path. **Revisit if** address churn causes observed
delivery failures.

## D18. One mailbox per harness and project; no process-tree matching (2026-11-22)

Supersedes the pid-matching idea in the first harness notes. Label defaults to
`<harness>-<directory>`; a hook finds its mailbox from harness and working
directory. Deterministic and restart-stable. Two simultaneous same-harness
sessions in one directory share a mailbox unless named. The end-of-turn hook is
opt-in (`--on-stop`) because it can start another model turn.

## D19. Networking is off by default (2026-11-22)

A fresh node has no TCP listener except the loopback UI. `a2p2 net on` opts in.
This is the "opt-in" requirement made literal, and it means the single-machine
experience has no network attack surface at all.

## D20. Message kinds are message, question, answer, receipt (2026-11-22)

`request`/`response` from the brief are dropped: a reply is a message with
`reply_to`. Fewer kinds, same expressiveness. `approval_request` became
`question` because nothing is approved by it: it asks a human something and the
answer is information for the asking agent, which still acts under its own
harness permissions.

## D21. Message ids are scoped to their author (2026-11-22)

Supersedes the global-id part of D13. Code review showed that with globally
unique ids, a member who has seen another member's message id can get its own
message with that id to a third node first and permanently block the genuine
one. The uniqueness key is now `(from_node, id)`; deliveries and read marks
reference the local row. A reference by bare id (reply, ack, trace) resolves to
the first stored row.

## D22. Known gaps accepted for now (2026-11-22)

Recorded so they are not mistaken for oversights:
- No per-peer disk quota: a room member can fill your disk with valid messages
  until the 30-day prune. Mitigation today is the transport's per-node rate
  limit and removing the member.
- The state directory's ancestors are not checked for other-user write access.
- `next_turn` hook consumption is untested per harness; `wake` is not built.
- No "leave room" for non-owners; the owner removes a member.
- One-way reachability (only one side can accept connections) leaves the other
  direction queued; there is no pull endpoint yet (see D3).

## D23. License: Apache-2.0 with sign-off (2026-11-23)

Supersedes D10. Five independent reviewers were asked; four chose Apache-2.0
for its explicit patent grant, which some companies require before they will
embed a daemon, and one chose MIT for brevity. Contributions are
accepted under the Developer Certificate of Origin: a `Signed-off-by` line on
every commit and no contributor agreement. The dependencies (BSD-3-Clause and
MIT) are compatible. Nothing had been released under MIT.

## D24. A requirement register checked by `make check` (2026-11-23)

`docs/TRACE.md` has one row per requirement in `docs/REQUIREMENTS.md`: its
status and the tests that prove it. `scripts/trace.sh` fails the build when a
requirement has no row, a row names a test that does not exist, or a row
claims `met` without a test. Needed now because the requirements are about to
drive a rework of the delivery core, and rule 2 (a guarantee without a test
does not exist) had no mechanical check.

## D25. The delivery core will be reworked to the new requirements (2026-11-23)

The owner widened the goal (rooms that survive any machine being away or
lost, several admins, threads, subscriptions, a path to very large rooms) and
answered the open choices; see `REQUIREMENTS.md` section 10. Research found no
existing system to adopt. The
v0.2 push-from-the-author delivery (D3, D13) will be replaced, not extended:
nothing has been released, so there is nothing to stay compatible with. Order
of work is in `REQUIREMENTS.md` section 9: executable scenarios first, then
the design, then the build.

## D26. Rules are settled in executable models before the design (2026-11-23)

Hand-written rules for several admins were shown unsound twice by reviewers,
so step 0 of the plan became two pure-Go models with exhaustive small
scenarios:
`internal/model/members` (who is a member and who is an admin, given any set
of signed entries) and `internal/model/delivery` (store and forward in a room
where every member keeps everything). No new dependency and no separate
specification tool. Each was reviewed twice by an independent model and
fixed; the reviews found faults the first searches had not reached (for
example, removing a re-appointed admin a second time emptied the room), which
is why the searches now start from seeded rooms as well as from scratch.

What the models settled:
- Admin-set changes need two admins whenever a room has more than one. A
  threshold of one for two-admin rooms lets a single key take the room over.
- No cut-off field on removals; what a removed admin's grants keep is decided
  by what its removers had seen. A completed removal is closed.
- A machine restored from backup starts a new stream of messages, so it never
  reuses a position; continuing the old stream loses new messages silently.
- A machine added without earlier history carries signed stubs without text,
  so it can still pass history on.
- Text is missing on a machine only for a reason it can check itself.

Known gaps are skipped tests marked KNOWN GAP in each package, and stated
limits in `REQUIREMENTS.md` section 6. Model tests appear in `TRACE.md` after
"Model:" and never make a requirement `met`.

Codex declined to review the membership model (its provider flagged the
request as a possible cybersecurity risk); that review came from a different
Claude model instead. The delivery model was reviewed by Codex.

## D27. Signed formats frozen in `internal/core` (2026-11-23)

Build step 1 of `DESIGN.md`. Records and log entries are signed over the
exact header bytes and identified by a hash of a type label, one zero byte
and those bytes; nothing is ever re-encoded. Decoding is strict (duplicate
keys, number spellings, base64 padding, nesting depth, sizes checked before
allocation), because two machines that disagree about whether bytes are valid
disagree about the room. The contract is `PROTOCOL.md` sections 0 to 3;
53 vectors in `internal/core/testdata/vectors` pin it, and every receive
check has a failing vector that names it. The v0.2 envelope stays beside the
new types until its last caller is converted. An independent review found two
faults that would have been frozen (leading whitespace refused; an explicitly
empty text field mishandled); both are fixed with vectors.

## D28. The membership model is the membership code (2026-11-24)

Build step 2. `internal/model/members` moved to `internal/members` and is
what the daemon will call: the exhaustive searches run on the code that
ships, not on a copy. The id function is injected (a model hash in the
searches, the signed-bytes id in the daemon), and the searches now also run
every history under a second id function, because a review found a result
that depended on how ids sort. A full resolve of 3,000 entries went from
minutes to about 0.1 s by numbering entries and keeping what each had seen
as bit sets; the old resolver is kept as a test-only reference and agrees on
every history compared. Limits chosen: 1,024 held entries or 4 MiB per room,
8 entries kept per (key, position). Reviewed independently by another model family.

Still to do in `internal/core`: a type that exists only after signature
verification, so that `members` cannot be handed an unverified entry.

## D29. Records in the store and the local path, beside the old code (2026-11-24)

Build step 3a. The design's step 3 was split so that nothing breaks midway:
3a adds the new path (migration 2; `store.PutRecord`, `Write`, `View`, `Seen`,
`ExpireText`; `node.Machine` with send, replace, withdraw, handled, inbox,
thread and branch) and 3b switches the tools, CLI and page over and deletes
the v0.2 delivery code. Tables added now and why: `records` and `streams`
(signed records under the gap-free rule), `cursors`, `reads`, `read_heads`
(the two-step read), `forks` (evidence of a second record at a held
position, at most three kept), `registrations` (an agent registered later
does not see the backlog as unread).

Choices made in review (independent review by another model family):
- The store takes only `*core.VerifiedRecord`.
- `Seen` confirms an entry only if the inbox would show exactly it to that
  agent now, so an agent cannot confirm, and then report handled, something
  it was never shown.
- A write chooses its version, links and position inside the store's write
  transaction; reads are read-only transactions and never write.
- Text counts as expired once marked or once the clock passes 30 days;
  marking happens at start, hourly, and before every write. If the clock is
  set back before a marking pass has run (at most an hour), text can
  reappear; after a pass it cannot.

A differential test runs the delivery model and the real code side by side
and compares every inbox, thread and stored record after each step. It found
a defect in the model (a replace could reuse a version number once the
winning text had expired), fixed in both.

## D30. The room log, requests and membership-aware reading (2026-11-24)

Build step 4a, beside the v0.2 code. Migration 3 adds `log` (every entry
with its disposition and arrival order, so the membership machine is rebuilt
on start to the same state), `room_members` (a cache of the resolver's
result), `requests` (changes waiting for a person) and `rooms.left_at`.
`node.Receive` is the one way entries enter a room, from this machine or
another.

Choices made in review (independent review by another model family):
- A request records the exact action approved. If the state has moved so
  that confirming would now do something else (promote has become re-admit),
  it is refused and nothing is signed. Decisions on a request are serialised.
- The in-memory state changes only after the database commit. On any failure
  the room is rebuilt from its stored rows.
- Every admitting grant carries the signer's positions (`from`), with or
  without history. A newcomer's agents start reading there; `history`
  decides only whether earlier text is readable. This changes the protocol,
  which had forbidden `from` together with `history`.
- Text whose readability cannot be decided yet waits; the reader's cursor
  does not pass it.
- A machine that has been removed may still write locally (nobody takes it),
  and its own reader marks its records as from a removed author.

## D31. The pull exchange and its request forms (2026-11-24)

Build step 5a, beside the v0.2 code: `node.Exchange` serves and makes pulls
over a one-method transport; migration 4 adds `observed` (what each peer
last showed it holds, the only source of copy reports) and `refusals`. A
differential test runs the real exchange against the delivery model on
multi-machine schedules with lost responses, restores, clock steps, and
machines added and removed.

Choices made in review (independent review by another model family):
- **Requests carry one hash, not every digest.** The first build sent every
  author's digest on every request: 39 KB each way for one new message
  among 64 machines, 12.7 KB for an idle pull. Now a round opens with one
  hash (`sum`), the server answers `same` or its digests, and the requester
  works out the differing authors. Cost: a server learns what a peer holds
  only from `same` answers and detail, so some copy reports arrive a round
  later or, between two machines with a lasting difference, not at all for
  the authors that were equal.
- **`seen`.** Two machines can differ for 30 days for a legitimate reason
  (one was admitted without history). Without a way to say "I have already
  looked at exactly this", they could never be idle against each other and
  a waiting pull could never wait.
- **A textless floor per stream** (`x`), so a summary does not list every
  expired position for ever.
- **A non-member's log entries are applied all or none**, as before. Keeping
  part of a batch would let a removed machine fill a room's entry cap
  through any accomplice. The cost is a stated limit: a machine restored to
  a copy in which it knows no current member must join again.
- **A 403 says nothing about the room.** The requester instead resends its
  log once, then waits for a change.
- **The release point rebuilds** the response if anything was written since
  it was built, so a withdrawal or removal in between is honoured.
- **Kept:** text whose readability is undecided is not marked expired, since
  only a stored `missing` mark shows it arrived in time; the decision and
  the write are one transaction.
- **Not done:** detecting two records by one author at one position through
  summaries (a stated limit); a bucketed hash so that the digests answer
  shrinks for large rooms (measure first, step 7).

## D32. Pairing: token, invites and the join exchange (2026-11-24)

Build step 4b, beside the v0.2 code, in `core/token.go`, `store/invites.go`
(migration 6), `node/invite.go`, `node/joiner.go` and `peer/join.go`.

New tables, and why each is needed now:
- `room_invites`: the admin side of an invite. v0.2's `invites` has other
  columns and is still used by v0.2 joins, so the name differs until the
  switch.
- `joins`: the joiner's record of each preview a person approved. Without
  it a crash between sending `accept` and hearing the answer leaves the
  admin's person approving a join the joiner has forgotten, and an old
  token could bring back a room that was left.
- `requests.admins`: making an admin takes one entry or two depending on
  how many admins there are, so a request raised under one count must not
  be approved under another.

New on the wire: `invite` in `poll` (the hash of the secret, accepted only
from the bound key) so that a joiner resumes after a restart without
storing the secret; `member` in the preview.

Choices made in review (independent review by another model family):
- An invite is used once on the joiner too, and leaving a room ends its
  joins in the same transaction.
- A key that is already a member is shown the role and history it has;
  approval signs nothing and returns the log.
- The log is paged in the order the server stored it, which only appends.
- Expiry and state are checked inside the transaction that changes them.
- A correct secret that has expired is told so. The reviewer wanted it
  indistinguishable from a wrong secret; knowing the secret is the
  credential, and the person needs to hear that the invite ran out.
- **First three admins:** with one admin, a join as admin is one `admin`
  entry; with more, an `add` plus that admin's `admin` entry under one
  approval, and the other admin is asked to second it. A daemon never
  seconds without its own person's approval.

Added after the re-review by the same reviewer:
- A preview has an identifier (a hash of everything shown) that `accept`
  must carry, so a second preview cannot change what a person approved.
- The joiner reports the role and history its own log shows. A reviewer
  wanted a join refused when they differ from the preview; the log is
  signed by the room's admins and a promotion may land meanwhile, so the
  difference is reported, not refused.
- Paging uses `log.arrival`, a position allocated once per stored entry
  (migration 7), because a count shifts when a held entry is evicted.
- Admitting a machine that is not a member is possible only through an
  invite with an accepted preview; `Propose` refuses it otherwise.
- Not done: pruning the joiner's used-invite rows (it would reopen the
  leave-then-old-token case) and binding an invite to a membership period.

## D33. The pull endpoint, waiting pulls, the scheduler and addresses (2026-11-24)

Build step 5b, beside the v0.2 code: `POST /p/v2/pull` in `peer/pull.go`
over the existing pinned mutual TLS, now also offering HTTP/2; held
responses (`node/waiting.go`, `wake.go`); the scheduler (`node/sched.go`:
two anchors per room, one stable and one rotating); the address book.
Measured over loopback with the race detector on: a message reaches the
other two of three machines in about 15 ms; an idle room costs two
requests a minute per machine.

New table, and why it is needed now: `peer_addrs` (migration 8). The
address book was first kept in memory; a machine that does not accept
connections would then know nobody after a restart and could never be
reached by anyone, so it would be cut off for good.

New on the wire: `peers: true` in an opening request. Addresses used to
travel only with a digests answer, so a machine whose answers were all
`same` never learned of another member's address.

Choices made in review (independent review by another model family):
- **Caught up means covered.** A room counts as pulled since start only
  after an exchange with a current member in which every log head that
  member advertised is accepted here. Receiving entries alone does not
  count.
- **A person can override the gate.** If no other member can be reached, a
  confirmation fails with `not_caught_up`; the person may approve again
  with an explicit override, which is recorded with the request. Without
  it an admin whose peers are all gone could never remove them. With it, a
  person who has restored this machine from a backup can cause the fork
  the gate exists to prevent; the warning says so.
- **A waiting pull answered at once is a failure** for back-off, so a
  faulty peer cannot draw requests in a tight loop (about 11 requests in
  the first 8.5 minutes, then one every 4 to 6 minutes per room).
- **Addresses.** A node's own report of its addresses outranks what others
  say; at most 8 per node; one that has failed for a day and five attempts
  is dropped; dials to addresses never verified by a pinned handshake
  share one budget for the daemon.
- **Limits before work.** The size and rate checks come before the body is
  read; a request holds a concurrency slot until it actually waits.
- **Not done:** no wake-up when text expires during a hold; the answer is
  worked out again when the hold ends, at most 60 s later.

## D34. The surfaces run on the new path (2026-11-24)

The switch, part A: the local API, MCP tools, CLI, web page, hooks and
daemon call the next version's code through one facade,
`node/surface.go`. The seven tools are unchanged in number and are the
golden file of PROTOCOL 6 (3,741 bytes compact, about 936 tokens by the
bytes/4 proxy; a test holds them under 4,000 bytes).

New, and why each is needed now:
- **CLI commands** `confirm`, `alerts`, `room admin`, `room leave`: a
  person must be able to approve or decline what agents and peers ask for
  without a browser (always-on machines are often headless), to hear that
  something waits, to replace a lost admin, and to stop carrying a room.
- **`net on --outbound-only`** and the third networking state: requirement
  N1 (a machine that cannot accept connections) needs pulls without a
  listener, and "off" must mean that nothing leaves the machine.
- **Request kind `network`**: turning networking on or off is one of the
  decisions P3 names, so it is recorded with the others.
- **Index `records_history`** (migration 9): `history` paged by reading
  the whole room.

Choices made in review (independent review by another model family):
- `room/admin` and `room/remove` only raise a request; nothing is signed
  until a person confirms.
- Decisions (`confirm`, `net`, accepting a join) are refused by the CLI
  unless it runs at a terminal, and are not among the agent tools or
  hooks. Any program running as the same user can still call the socket;
  comments and help text now say so rather than implying enforcement.
- A request shows the full fingerprint and its terms as separate fields,
  and the page sends back the text it showed; confirming fails if the text
  has changed.
- **Not done:** issuing an invite needs no confirmation (it does nothing
  until the admin's person approves the accept). A question from
  `ask_human` has no marker of its own in the record: the body decoder
  refuses unknown keys and the signed formats are frozen (D27), so the
  page treats any unread message to the person, or one with choices, as a
  question. Subscription levels (U1 to U3) are stage 3. The page's script
  has not been run in a browser.


## D35. What five agents using a2p2 together changed (2026-11-24)

Five agents from three model families (three Codex sessions, two
Antigravity sessions) polished this project's documents and surfaces while
coordinating through a2p2 itself: one room across two daemons on one
computer, a lead, two writing tracks and three reviewers. They and the lead
listed sixteen points of friction. A product round then took these:

- **`reply` addresses everyone who has posted in the thread.** Without `to`
  the reply tool now reaches the parent's author and every agent that has
  posted in the thread here, at most 32. Under the earlier rule the worker
  named in each kickoff received none of the reviewers' findings. This
  amends requirement D10 (threads) for the reply tool and command only. It
  is done where the reply is written, as an ordinary addressed reply, so
  the signed format, the reading rule and the delivery model are unchanged;
  a record-level reply with no addressees still means the parent's author.
- **`reply` reports what it did and takes `to`**: whom it addressed and
  whether a handled report was sent.
- **`ack` says why it refuses**: no such message, not addressed and no
  `ask_handled`, the agent's own message, not yet shown by `inbox`, or
  already reported. The rule is unchanged.
- **`inbox` takes `recent`** (1 to 50) to read a room's last messages
  without marking anything seen, so an agent that registered late can catch
  up.
- **`peers` lists, per room, the machines and the agents seen writing
  there**, so an agent can find a remote agent's id. Before, it listed only
  local agents.
- **Compact, uniform results.** Every tool returns one JSON object with one
  set of names (`machine` and `machine_name`, `room` and `room_label`); an
  inbox entry dropped repeated ids, and ten messages went from 4,319 to
  1,816 bytes. `human` is an addressee. The tool definitions are 4,000
  bytes compact, the whole A2 budget.
- On the surfaces: alerts include questions for the person, "Dismiss"
  became "Mark handled without replying", plain `status` no longer prints
  the page's token, and networking checks the terminal and the home
  directory's permissions before asking for `yes`.

Declined, and why:
- **Following threads in the reading rule**, so that replies in a thread an
  agent posted in reach its inbox. It makes what an agent sees depend on
  when its cursor passed a reply, which breaks the check that reading from
  the cursors equals reading from the start. Addressing at write time gives
  the same reach without that.
- **Showing a late registrant earlier messages in its inbox.** U3 stands:
  a new agent's inbox starts empty, and `recent` is the explicit way back.
- **`ack` after a thread read.** A thread view marks nothing seen, so it
  does not make a message eligible; the refusal now says so.
- **A cursor for thread reads** (`after`): not built in this round; with
  replies now addressed to the thread's participants, the inbox carries new
  replies, and `recent` and the whole-thread view cover the rest.
- **Attachments**: later (L1); agents passed file paths, which work only on
  one machine.
- Antigravity's own copy of the tool schemas under `~/.gemini` is the
  harness's behaviour, noted in [HARNESSES.md](HARNESSES.md).

**Revisit if** a second measured run shows lost handoffs, many empty polls
or refusals that agents cannot act on.

## D36. What the research on agent teams puts in and keeps out of the core (2026-11-24)

The owner asked which practices for agent communication the evidence
supports, and whether teams of mixed harnesses, shared memory, other
protocols and Windows belong in or around a2p2. Three consultants from
other model families reviewed the conclusions through a2p2. The core stays
a small durable messaging layer; the rest is text, a separate layer, or
later.

- **Conventions ship as text, not mechanism.** Delegation briefs, "done
  with evidence" handoffs, first-line status prefixes, advisory claims with
  an expiry and separate verification go in an optional recipe file that
  is installed only when asked. The evidence for each is weak to moderate,
  and new status values in the signed envelope would change the frozen
  format (D27) and fix a vocabulary before any data.
- **A team launcher is a separate layer.** Spawning sessions, choosing
  models, budgets, roles and harness flags change monthly and differ by
  vendor; they would make the daemon large and security-relevant. A
  launcher uses the public CLI and MCP tools and posts `started`, `done` or
  `failed` as ordinary messages. No core seams were added for it; labels
  cost nothing, and waiting in `inbox` covers waking.
- **No knowledge graph.** Studies found full context beating derived
  memory for small histories and graphs losing on plain facts. The room's
  30-day log is the shared record; durable memory belongs in the
  repository's own documents, with an opt-in exporter later, and memory is
  never injected automatically.
- **No A2A, SLIM or ANP in the core.** They assume a reachable server or a
  hosted router, against a design where a machine may only open outbound
  connections; measured use of A2A is small and published attacks remain
  open. A gateway waits until a user asks for one.
- **Windows after the first release.** Everything builds except the daemon
  and the CLI. The real work is the local trust boundary: file mode does
  not protect a Unix socket on Windows, so each local API connection would
  need a per-user token read from a user-only directory, preferred to named
  pipes, which would add a dependency. A `GOOS=windows go vet` gate comes
  first, then full support.

**Revisit if** a measured run shows that typed status values or a
launcher in the core would prevent failures the text conventions do not.

## D37. The working record is kept outside the repository (2026-11-24)

Private working notes are kept outside the public repository.
Contributor-facing harness evidence is in [HARNESSES.md](HARNESSES.md).

**Revisit if** a reviewed selection of the working notes is published.

## D38. The last core items before publishing (2026-11-24)

What changed, and why each is needed before the repository is public:

- **Refusals carry a code.** Every refusal of the local API and the tools
  is `{"error","code","retryable"}`: a sentence, one of twelve stable codes
  (`node.ErrorCodes`, PROTOCOL 7) and whether repeating the call later may
  succeed, so a program or agent can decide what to do without parsing
  the wording.
- **The inbox tool waits at most 30 seconds**, says when a wait ended with
  nothing, and marks nothing seen when the call is cancelled. Codex ends a
  tool call after 60 seconds by default, so a longer wait would be cut
  off. The CLI and the local API keep 120 seconds.
- **Tool annotations.** Each tool says whether it reads, adds or rewrites.
  Codex refuses unannotated MCP tool calls by default since 0.169.
- **The Codex installer approves a2p2's tools.** `a2p2 install codex` adds
  one line, `default_tools_approval_mode = "approve" # added by a2p2
  install`, under `[mcp_servers.a2p2]` in `~/.codex/config.toml`, so the
  approval covers this server only; `uninstall` removes exactly that line.
  Without it Codex refuses every call in a headless run. The installer
  says when it cannot find the table and leaves approval to the person.
- **`id` on `reply` and `ack`.** The parameter was `message_id`; every
  result names a message `id`, so the tools now take the name an agent
  has just read.
- **`ack` is idempotent.** A second report by the same agent, including an
  `ack` after `reply` has reported the message, succeeds with `already`
  and names the first report instead of failing. The CLI prints "already
  reported handled". A repeated report changes nothing, so refusing it
  only gave the agent an error to handle.
- **A thread read takes `after`**, so an agent following a thread reads
  only what follows a message it has seen instead of the whole thread
  again. D35 had left it for later.
- Tool results and `--json` output write `<`, `>` and `&` as they are.
  Uninstall removes what install added and restores a hook file a2p2
  backed up, if nothing else has changed it since; a registration named
  `a2p2` that existed before install was replaced by install and is not
  brought back. `doctor` checks the MCP registration as well as the hook. A CLI call made under a
  harness agent's label no longer unregisters that agent's hook.
- **The tool budget.** The definitions are now 3,991 of the 4,000 compact
  bytes A2 allows (998 tokens by the bytes/4 proxy).

Left undone on purpose: an agent shown a message only through `recent` or
a thread read still cannot report it handled, because recording "shown"
without marking it seen needs a new table and a change to the delivery
model's rule; the refusal says why.

**Revisit if** agents in a measured run keep trying to `ack` messages they
read through `recent` or a thread.

## D42. A chaos harness of real daemons in containers (2026-11-24)

Nothing tested real daemons killed at random or split by the network
(TRACE, D1 and D6). `scripts/chaos/` now does, with `make chaos`.

- **Where.** `scripts/chaos/`, next to `scripts/e2e/`: like the end-to-end
  runner it drives the built binary from outside, needs something `make
  check` does not have (Docker), takes minutes, and is run by hand. It is
  not a Go package, so it does not belong under `internal/` or in
  `go test ./...`.
- **How.** A Python 3 driver (standard library only, like `scripts/e2e`)
  builds the static Linux binary into a busybox image (busybox gives
  `script` for a pseudo-terminal and `ip` for blackhole routes in 1.6 MB),
  runs one daemon per container as its main process with its own home
  volume on one user-defined network, and drives the CLI with
  `docker exec`. `net on`, `join` and `confirm` are answered at a
  pseudo-terminal made by `script` inside the container; the product is
  unchanged. Partitions are blackhole routes inside the containers (they
  need `NET_ADMIN`), rewritten by the entry point after a restart, so a
  node killed during a partition stays on its side. `tc netem` is not
  used: it would need iproute2 and kernel modules for little gain over
  dropping packets.
- **The target.** `make chaos` is not part of `make check` and needs no
  new dependency, protocol message, tool, command, config key or table.
  Its output goes to `.chaos/`, which is git-ignored.

**Proposed wording for the register (not applied).** TRACE.md now says a
row is `met` when "the tests named here exercise every clause of the
requirement as written, against the real code", and `make trace` only
knows Go tests. Proposed addition to its status rules: "A clause may also
be shown by `make chaos` when the row names the fault it exercises and the
seeds of a recorded run without violations, of the current wire version;
such a row also names the Go test that `make check` runs for the same
clause where one exists." Until the owner decides, D1 and D6 stay
`partly` and name the chaos runs as evidence in their notes.

**Revisit if** the chaos runs become slow enough or flaky enough that
nobody runs them, or CI gains Docker and a time budget for them.
