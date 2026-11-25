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

## D39. Team conventions ship as an optional skill and recipes (2026-11-24)

Requirement I3 asks for a portable skill and optional recipes; D36 said the
conventions the research supports go in text, not in the core. This adds
`skills/a2p2/SKILL.md` (Agent Skills format) and nine recipes in
`recipes/`: delegation brief, handoff with evidence, status lines,
advisory claims with a lease, review by a second agent, leading a small
team, asking the person, catching up, and rooms and threads.

- **Where.** At the repository root, beside `AGENTS.md`, so an agent that
  reads the README or `llms.txt` finds them without opening `docs/`, which
  is for people changing a2p2. `skills/a2p2/` is the layout skill loaders
  expect. Nothing installs them; `a2p2 install` is unchanged.
- **Kept out of the core on purpose.** No status field or typed message
  kinds (`DONE`, `BLOCKED`, `NEEDS-HUMAN`, `CLAIM` are first-line words),
  no lock or lease table for claims (a claim is a message replaced or
  withdrawn with `replaces`), no launcher, and no `brief` or `handoff`
  tool. Each would change the signed format (D27) or grow the tool budget
  (A2) for practices whose evidence is weak to moderate.
- **Kept honest by a check.** `scripts/recipes.sh`, in `make check`, fails
  if the text names a tool, argument or CLI command that does not exist.
  It reads the golden tools file and `a2p2 help`; it adds no dependency.
- **What writing them showed.** A brief sent before the worker's first
  tool call never reaches its inbox, so the recipes have the lead wait
  until `peers` lists the worker. A reader cannot tell from `inbox` that a
  room-wide post asked for handled reports (`ask_handled`), so the recipes
  ask in the text. A replacement is marked `"kind":"replaced"` only for a
  reader who had the first version from `inbox`.

**Revisit if** a measured run shows agents ignoring the text conventions
in a way a core feature would prevent, or the skill format changes.

## D40. A team launcher as a separate layer on top of a2p2 (2026-11-24)

The owner wants to start formations of different harnesses that
coordinate through a2p2. D36 put a launcher outside the core; this is it,
in `scripts/team/`, built only on the public CLI and the MCP server.

- **Nothing in the core changed.** No new tool, CLI command, config key,
  table or dependency. The launcher passes the MCP server to each harness
  for one run on its command line (Antigravity: a workspace file in a
  directory the launcher made) and never edits a harness's configuration.
- **Python 3 standard library**, because the launcher manages processes,
  JSON and timestamps, which are awkward in POSIX shell, and Python 3 is
  already on every machine that runs the harnesses. It is not part of the
  Go module, so the licence check and the static binary are unaffected.
- **A JSON manifest.** Every Python 3 can parse it without a library
  (`tomllib` needs 3.11), and a2p2 already speaks JSON. Long briefs go in
  files named by `brief_file`.
- **"Started" needs proof.** The launcher registers each label with the
  CLI before the kickoff (an agent only sees room posts made after it
  registered), and reports `started` only once the label has been
  registered again under the harness's name after launch, which only the
  member's own MCP server does.
- **Reports are ordinary room messages** from a `team` label: `started`,
  `done`, `failed`, `limit`, `silent`, `needs input`, `stopped`. They
  carry numbers and paths, never a member's text, and the watcher never
  acts on what members write.
- **Measurement comes with it.** `team report` writes per-member counts
  from the room and the harness logs as JSON and Markdown, because there
  is no evidence yet that a formation beats one strong agent at equal
  compute.

**Revisit if** a launcher needs something the CLI cannot give it without
scraping, such as a registration call or a way to read who is connected.


## D41. Each agent chooses how closely it follows a room (2026-11-24)

Requirements U1 to U4, the owner's "the model needs to really be able to
control this messaging". Until now every room-wide post was unread for
every agent in the room.

- **Three levels, per agent and room: `all`, `addressed`, `none`.** `all`
  is the default and behaves exactly as before, so nothing changes for an
  agent that never chooses. `addressed` shows messages whose `to` names
  the agent and room-wide posts marked for everyone; `none` shows no
  message until the agent asks. Notices about messages already shown are
  never filtered (D9).
- **"Marked for everyone" is `ask_handled`.** The signed formats are frozen
  (D27) and refuse unknown body fields, and a room-wide post and one sent
  to `everyone` are the same bytes (`to` omitted), so they could not be
  told apart. `ask_handled` is the one existing field that marks a
  room-wide post as wanting each reader's attention; machines that do not
  know levels still show it to everyone. A reserved addressee in `to` was
  rejected: older machines would hide such a post from all their agents.
  The cost: marking a post for everyone also asks for handled reports.
- **The level is local.** It is a reading choice, like cursors and reads:
  no record, entry, pull or wire version changes, and no other machine
  needs it. Two tables (migration 11): `levels` (one row per agent and
  room that is not `all`) and `skips` (messages a level change marked
  skipped). Without `skips`, raising a level would put every message the
  cursor had not yet passed back into the live inbox, depending on when
  the agent last confirmed; with it, the inbox continues from the change.
  Leaving a room deletes both.
- **No new tool or CLI command.** The inbox tool gains `level` (set first,
  for `room` or every room), `peek` (counts and first lines, marks
  nothing) and `skipped` (what the level kept out, marks nothing);
  `whoami` shows each room's level and unread count. The local API gains
  one call, `level`, and `skipped` on `history` and `agent` on `unread`;
  the CLI gains `inbox --level/--levels` and `history --skipped`. `recent`
  did not cover U3: it mixes what was read with what was skipped and stops
  at 50 messages. To stay within A2's 4,000 bytes, descriptions were
  shortened (3,991 bytes now).
- **The person has no level.** Setting one for `human` is refused: what
  is addressed to the person, and requests for a person, are never
  filtered; the page shows each agent's level and reads every room in
  full.

The rule is in the delivery model (`levels.go`) and the differential tests
apply random level changes on both sides. Level operations draw from
their own random source, so the existing seeded schedules stay as they
were.

**Revisit if** agents want a room-wide post marked for everyone without
asking for handled reports; that needs a wire change (a new optional
field and a wire version), which old machines would refuse.

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

## D43. Dropped text leaves the store's files, not only its tables (2026-11-24)

The container fault tests (D42) grep every member's files after a run and
found withdrawn text still on disk: in the write-ahead log until the daemon
stopped, and once in freed space of the database file. No view showed it,
but requirement D9 says machines drop the withdrawn text.

- The database is opened with `secure_delete`, so content freed by dropping
  a text is overwritten in the database file.
- `Store.Scrub` checkpoints and truncates the write-ahead log; the daemon
  calls it every minute. Text that was withdrawn, expired or voided is
  therefore out of every file of the store within about a minute.
- This is not a forensic guarantee. Blocks the operating system or the disk
  has freed are outside the store's reach, and backups keep what they copied.

Cost: slightly more writing when text is dropped, and one cheap checkpoint a
minute. Test: `TestWithdrawnTextIsInNoFileOfTheStoreAfterAScrub`.

## D44. OpenCode, Hermes Agent and Pi, for agents on local models (2026-11-24)

People run agents on local models through harnesses other than the three
a2p2 supported: OpenCode, Nous Research's Hermes Agent and the Pi coding
agent are the ones asked for. Each was run against a throwaway a2p2 home on
Ollama models on one macOS computer, first with a2p2 given for one run and
then through the configuration `a2p2 install` writes (into a scratch home);
the results are in the [harness notes](HARNESSES.md#local-models-opencode-hermes-agent-and-pi).

- **Three new arguments to `install`, `uninstall` and `doctor`, no new
  command:** `opencode`, `hermes` and `pi`. Each writes one entry in one
  file and nothing else, keeps the original as `<file>.a2p2.bak` or marks a
  file it created, and `uninstall` puts the home back (I2).
- **a2p2 edits these files itself** instead of going through the harness's
  CLI, as it does for the other three. `opencode mcp add` (1.18.30) only
  prompts. `hermes mcp add` (0.15.2) cannot pass an argument starting with
  `--` to the server, connects to the server to list its tools (which
  would start a daemon and register an agent during install) and then
  prompts. Pi has no MCP.
- **OpenCode:** `mcp.a2p2` in `~/.config/opencode/opencode.json` (or under
  `$XDG_CONFIG_HOME`), which OpenCode merges with the `opencode.jsonc` it
  creates itself. a2p2 writes only plain JSON and refuses a file with
  comments. No permission entry is needed: the default agent allows MCP
  tools, also in `opencode run`.
- **Hermes:** the `a2p2` entry under `mcp_servers` in
  `$HERMES_HOME/config.yaml` (default `~/.hermes`). There is no YAML
  library in the module and adding one for a single entry is not worth a
  dependency, so the edit is textual and refuses layouts it cannot follow
  (flow style, tabs, several documents). Hermes passes a server only a few
  environment variables, so an `A2P2_HOME` set at install time is written
  into the entry.
- **Pi:** a skill, `~/.pi/agent/skills/a2p2/SKILL.md` (or under
  `$PI_CODING_AGENT_DIR`), that tells the model to use the CLI with `--as
  pi-<directory>`. Pi's documented way to add a capability is a skill
  pointing at a command-line tool; an MCP extension would be a2p2 code
  running inside Pi.
- **No hooks for these three.** OpenCode has plugins and Hermes has shell
  hooks (its `pre_llm_call` event can add context), but neither was tried
  in a run, and a hook is installed only once it has been seen to reach
  the model. The tool descriptions and the skill tell the model to check
  `inbox`.

What the CLI cannot do that the tools can, as Pi sees it: there is no
`whoami` or `peers` (the agent reads `status --json`), `send --reply-to`
addresses only the author and does not report the message handled (the
agent runs `ack`), `ask_human` has no choices, and the inbox `--json`
output has no notice that messages are information, not instructions (the
skill says it).

**Revisit if** OpenCode's or Hermes's CLI gains a non-interactive way to
register a stdio server with arbitrary arguments, or a harness's hook is
seen to reach the model.

The tool definitions change in one place: the description of `inbox`'s
`recent` argument now ends "not new mail" (3,991 of 4,000 bytes), because
an 8B model read its mail with `recent` and could then not report it
handled; with the new wording it did so in 0 of 7 runs (2 of 7 before). A weaker
model would also be helped by a description for `reply`'s and `ack`'s
`id` ("message id from inbox") and by optional string arguments that
accept null, but the first needs bytes the budget does not have and the
second changes the schema, so both are left.

**Install records (review fix, same day).** A review found that installing
over an entry named `a2p2` the person already had, then uninstalling,
erased that entry, and that uninstall deleted the backup even when the
file had changed since. Each `install` into any of the six harnesses now
first writes a record to `$A2P2_HOME/installs/<harness>.json` (a directory
of a2p2's own state, not a database table): which file, and the entry
named `a2p2` it replaced, if any. A second install keeps the first record.
`uninstall` removes a2p2's entry, puts the recorded one back, keeps every
other edit, and deletes the record; with no record it removes nothing and
says so, since it cannot tell a2p2's entry from the person's. A
`<file>.a2p2.bak` is deleted only when the file equals it again. A config
path that is a symlink is followed for all six; a dangling one is refused
before anything is written, since writing would replace the link. The
Hermes edit now keeps CRLF line endings, a missing final newline and a
comment after `mcp_servers:`, refuses documents that are not a block
mapping or mix line endings, and, like the OpenCode edit, parses its
result back and checks that every original line outside a2p2's entry is
still there before writing. Tests: `TestUninstallPutsBackTheEntryNamedA2P2ThatInstallReplaced`,
`TestUninstallWithoutARecordRemovesNothing`,
`TestHermesEditNeverTurnsAValidFileIntoAnInvalidOne`,
`TestOpenCodeAndPiRefuseWhatTheyCannotEditExactly`,
`TestConfigSymlinksAreFollowedAndDanglingOnesRefused`.

## D45. What counts as shown, receipts for Seen, and "@everyone" (2026-11-24)

Using levels (D41) showed four problems. A worker that joined after a
kickoff could read it with `recent` but could not report it handled. An
agent at `none` could not page back through what it skipped. The inbox
could confirm an entry the agent had never been listed, if it guessed the
key. And `ask_handled` did two jobs, so a post could not reach everyone at
`addressed` without also asking for reports.

- **Any read that showed the text counts as shown.** `inbox`, a thread or
  branch view, `recent` and `skipped` all show text, so each lets the
  agent report handled (the addressee rule is unchanged). The non-inbox
  views record a text mark; none moves a cursor or marks anything seen.
- **Seen needs a receipt.** A full inbox read records, in the same
  transaction, the key and version of each entry it lists. `Seen`
  confirms only an entry with a matching receipt, and deletes it. A
  receipt survives a level change: the message is not kept out or
  skipped, so it can be confirmed and its later notices arrive. Both
  kinds of row live in one new table, `shown` (migration 12), because they
  have the same shape and the same lifetime (deleted on leave). `reads`
  could not hold them: it means "confirmed".
- **"Marked for everyone" is the text's own `@everyone`.** ASCII, any
  case, followed by whitespace, punctuation or the end of the text. It
  replaces D41's use of `ask_handled`, which now only asks for reports.
  The mark is in the signed text, so it needs no wire change, unlike the
  field D41's "revisit if" foresaw. While the text is missing the mark is
  unknown, and the inbox waits for it. D41 is otherwise unchanged.
- **One reading order.** Every view orders messages by place: created,
  author machine, position, id. Paging (`before`) compares places, so one
  author's messages of one millisecond keep the order written and are
  never repeated or dropped at a page boundary. Migration 12 replaces the
  index `records_history` with `records_reading` in that order.
- **Smaller additions.** Entries carry `ask_handled` and, in `recent` for
  an agent, `skipped`. `skipped` pages with `before` (tool, local API,
  CLI) and lists as expired a message whose text expired while kept out.
  `peek` for one room resolves the room and filters before the cap
  (`room` on `unread`). No new tool, command, config key or dependency.
- **Branch view under an unheld message.** The deep differential schedules
  found the code's branch view empty where the model listed the replies.
  By D10 the model was right; the code now follows the replies naming a
  message it does not hold. G3 (replies below an unheld reply) remains.

**Revisit if** receipts grow large for agents that read but never call
`Seen`: they are bounded by the inbox limit per read, but a cleanup by
age may be needed.

## D46. Versions, a changelog, a written release process, and small hosted CI (2026-11-24)

The owner asked for ordinary versioning and release practice, and for the
hosted CI to stay small: testing should happen locally, with containers
where another system is needed.

- Semantic Versioning with annotated `vX.Y.Z` tags; `-rc.N` candidates are
  pre-releases; a pushed tag is never moved. `CHANGELOG.md` in the Keep a
  Changelog form is written for users, and the release workflow publishes
  the version's section as the release notes (`scripts/changelog.sh`).
  The process is `docs/RELEASING.md`.
- Hosted CI went from five jobs on every push (Linux and macOS checks,
  lint, vuln, fuzz: about 15 wall-clock minutes a push, and macOS minutes
  are billed at ten times the Linux rate on a private repository) to one
  Linux job on pull requests that touch code and on request. Nothing runs
  on a push to `main`. The release workflow still runs the full check,
  lint and the vulnerability scan on Linux before it builds.
- What moved local: `make prerelease` runs the check, lint, the
  vulnerability scan, the test suite on Linux in a container
  (`make check-linux`) and a short chaos run. macOS is the development
  system, so it is tested on every local `make check`.

Cost: a broken `main` is caught by the next local run or the next pull
request, not by a push. Accepted: one maintainer, and the release gate is
strict. Revisit if outside contributions become frequent.

## D47. a2p2 runs on Windows: the home's access list and a local key (2026-11-24)

Windows 10 1803 and later, and Windows 11, on amd64 and arm64, without a
change on macOS or Linux. Until then the daemon and CLI refused to run
there (D36). The one hard part was the local trust boundary: on Unix it
is the socket's mode and a home directory that only its owner can open,
and on Windows a file's mode means nothing (Go reports 0666 or 0777 and
`chmod` only sets the read-only attribute).

Choice: keep the AF_UNIX socket in the home, guard the home with an
access control list, and add a per-start local key. Not a named pipe.

- **The home.** a2p2 creates it with a protected access control list
  (`O:<user>D:P(A;OICI;FA;;;<user>)`: nothing inherited from the parent,
  full control for the user only, passed on to everything created
  inside). An existing home is refused if it is owned by someone else or
  if any entry allows in anyone but the user, SYSTEM or Administrators
  (the last two can take any file on the machine, as root can on Unix,
  so allowing them takes nothing away). This is the Windows form of the
  0700 and owner checks, and is tested on every system from the
  descriptor's text (`internal/daemon/acl.go`).
- **The socket.** Microsoft's AF_UNIX announcement says that "for
  connecting to a stream socket, the connecting process should have write
  permission on the socket", and that the socket file is an NTFS reparse
  point created by `bind`
  (devblogs.microsoft.com/commandline/af_unix-comes-to-windows, 2017).
  The file is created in the home and inherits its list, so another user
  has no write permission on it. That is documentation, not something
  tested here: no test has a second Windows user try to connect.
- **The local key, so that safety does not rest on that one sentence.**
  At each start the daemon writes 24 random bytes in hex to
  `local.key` in the home (which inherits the same list) and refuses,
  with `not_allowed` and before any handler runs, every local API request
  whose `A2P2-Local-Key` header does not match. Every client in a2p2 (the
  CLI, the MCP shim, the hooks) reads the file at each call. The key is
  new at every start and removed at stop, like the page's token. A
  process that can read the home can call the daemon, which is the same
  rule as on Unix; one that cannot is refused even if it can connect.
- **Not a named pipe.** A pipe with a security descriptor for the user is
  the usual Windows answer, but doing it well means overlapped I/O, which
  in practice means `golang.org/x/sys/windows` or `go-winio` as a direct
  dependency (I4) or a few hundred lines of raw calls, and a second
  transport in `localapi` and every client. The socket and the key reuse
  what exists and add about a hundred lines.
- **macOS and Linux.** Nothing changes there: `localKeyRequired` is false,
  no key is written, and the socket's mode and the home's 0700 and owner
  checks are as before. The key code runs on every system in the tests
  through a test-only option.
- **The rest.** The lock is `LockFileEx` on the first byte of `lock`. The
  background daemon starts with `DETACHED_PROCESS` and
  `CREATE_NEW_PROCESS_GROUP` (no console window, and Ctrl-C in the
  caller's console does not reach it), asking to leave the caller's job
  and starting inside it when the job does not allow that. Decisions
  (`confirm`, `net`, approving a join) need a console: `GetConsoleMode` on
  stdin, so mintty and pipes are not one. Ctrl-C in a foreground daemon
  is `os.Interrupt`, and closing its console is `SIGTERM` in Go; `stop`
  goes through the local API as before. A socket path of 108 bytes or
  more is refused with a sentence naming `A2P2_HOME`. `a2p2 ui` opens the
  page with `rundll32 url.dll,FileProtocolHandler`, which passes the URL
  with its `#token` unchanged. All of this uses the standard `syscall`
  package and lazy calls into kernel32 and advapi32: no new dependency.
- **Installers.** A hook (Claude Code, Codex) and Pi's skill name the
  binary inside a command that a shell runs, and on Windows that shell
  differs (Claude Code: Git Bash, or PowerShell without it; Codex: not
  documented) and each quotes differently. So on Windows the path is
  written with forward slashes and only if it is one plain word in all of
  them (letters, digits and `/ . _ - :`); any other path is refused with
  a sentence saying where to copy the binary, rather than quoted for a
  guess. OpenCode and Hermes take the command as a list, so their paths
  are kept as they are. Hermes's home defaults to `%LOCALAPPDATA%\hermes`
  on Windows, where its Windows installer puts it. `a2p2 install agy`
  says it is not supported on Windows: the Antigravity CLI's locations
  and hook shell there are not documented.

Verified on this change: the build, `go vet` (test files included) and
the compilation of every test binary for windows/amd64 and windows/arm64,
and every test on macOS. Not verified: any run on Windows. A
dispatch-only CI job runs `go build`, `go vet` and `go test` on
`windows-latest`; until it has passed, the README says a2p2 builds for
Windows and has not yet run there.

After the first dispatched run (2026-11-24; 5 of 12 packages failed):

- **SIDs are compared resolved.** Every home was refused as "owned by
  another user (LA)". The runner's account is the machine's built-in
  Administrator (RID 500), and SDDL writes that account as the constant
  `LA` while the token gives `S-1-5-21-...-500`: the same account, compared
  as text. The check now resolves each owner and trustee with
  `ConvertStringSidToSid` before comparing. The rule is unchanged: the
  owner must be the token's user or the token's default owner
  (`TokenOwner`, which for an elevated administrator can be the
  Administrators group), and the list may allow in only the user, SYSTEM
  and Administrators. A home a2p2 creates names the user as owner and
  sole entry. This lets no one else in: another standard user is still
  refused if any entry allows them, and an owner of Administrators admits
  only those who could take the directory anyway.
- **Line endings.** Git for Windows checked the golden vectors out with
  CRLF, so every vector differed. `.gitattributes` now checks every text
  file out with LF on every system; the test is unchanged.
- **Tests, not the product:** one harness test compared a directory in
  Unix spelling (`/work/proj`; the daemon keeps `\work\proj`), and two
  `internal/node` tests left a database open, which Windows cannot remove
  at the end of a test. The deep differential schedules of `internal/node`
  took 19 minutes on the runner and are left out of the Windows job; they
  test the delivery rules, not the system. The record-test lines in that
  log were `-v` logging, not failures.
- **The workflow** takes a `what` input (`linux`, the default, `windows`
  or `all`) so a run by hand spends only the minutes asked for; pull
  requests still run only the Linux job.

The third run (2026-11-24, Windows Server 2025, amd64) passed all twelve
packages. The second had passed eleven: the twelfth failed because a
test's recording proxy between the MCP shim and the daemon dropped the
local key, and the daemon refused the call, as it should. Still not
verified on Windows: an install with a real harness, the page in a
browser, a second user trying to reach the daemon, and arm64.

**Revisit if** a run shows that the socket file does not
inherit the home's list, or if a harness documents its Windows hook
shell, which would let paths with spaces be quoted for it.

## D48. Reachability: `a2p2 net check`, a dial-back message, IPv6 and port mapping (2026-11-24)

Two reports drove this. The owner learned that a work laptop dropped every
inbound connection only by asking a colleague; a2p2 could not tell. And
two machines behind ordinary home routers cannot reach each other unless a
third member is reachable or someone forwards a port by hand.

- **`a2p2 net check`** is an argument of the existing `net` command, not a
  new command, and a new local API call `net/check`, because the probes
  need the daemon's key and address book. It decides nothing, needs no
  terminal, and dials nothing while networking is off. The `HOST:PORT` and
  `--invite` forms run in the CLI process under a throwaway key, so testing
  an address before an invite neither needs a daemon nor tells the other
  side who is asking. `status` gains `reach`; no config key, no table.
- **New protocol message: `POST /p/v2/dialback`** (PROTOCOL 4.7). Needed
  now because no amount of probing from inside tells a machine whether
  others can connect to it; only a peer can, and a peer that is a member
  is the only one a2p2 trusts with the question. Alternatives considered:
  a public echo service (a third party, against N3); asking in a pull
  request (the pull exchange's answers are worked out per room and held,
  and a field there would change its shape for every room); doing
  nothing (the report above). Old machines answer 404, which the asker
  reports; the wire version stays 2.
- **Why it is safe to answer.** The asker is authenticated by TLS and must
  be a current member of a shared room. The responder dials only the IP
  address the request came from, on at most two ports; anything else is
  answered `not_tried`. So a member can cause at most two TCP connects and
  TLS ClientHellos, and only to the address it asks from; the dial-back
  adds no way to aim traffic at a third party. An earlier allowance, also
  dialling addresses the responder's book held for the asker, was
  removed after the second review: a member could have listed a third
  party's endpoint as its own and had it probed at once, outside the
  scheduler's dial budget. The limit left: a member behind a shared
  address can cause two connection attempts to that address. To check an
  IPv6 address the asker asks over IPv6. It closes after the handshake, skips special and (for a remote
  asker) loopback addresses, never resolves names, and answers at a fixed
  deadline unless everything dialled was reached. Rate limits per key (4,
  then one per 10 minutes) and per machine (16, then one per 10 seconds,
  at most 4 at once) bound the rest.
- **IPv6, as part of a new decision.** A wildcard listener is one
  dual-stack socket and global IPv6 is advertised beside IPv4 (DESIGN 11),
  because on many home networks IPv6 needs no NAT and only a host firewall
  stands between two members. A decision recorded before this version
  stays IPv4 only (the role of an `on` decision now holds the word
  `ipv6`), until the person runs `a2p2 net on` again, whose prompt names
  the IPv6 addresses. IPv4 only on purpose: `listen` set to one IPv4
  address. No flag, no config key.
- **What members say they see is display only.** `saw` is validated as an
  IP literal, shown, and compared with a router's external address. It is
  never advertised, even after a dial-back to it: the member that reports
  it is the one that would confirm it, so one lying member could make this
  machine publish someone else's address. A port forwarded by hand is
  declared with the existing `advertise` key, whose line `net check`
  prints.
- **`a2p2 net on --map-port`**, a flag of the existing command and a field
  `map_port` of the existing `net` call, stored in the role of the
  networking decision (no column, no table, no migration). It asks the
  router to forward the listening port with PCP, then NAT-PMP, then UPnP
  IGD, written against the RFCs and the UPnP specification with the
  standard library only (`internal/daemon/portmap`): no new dependency,
  because the three protocols are small and a dependency would bring a
  router discovery we could not bound. For the same reason the macOS
  default route is read with the frozen `syscall` routing calls, under a
  staticcheck exception, not `golang.org/x/net/route`. It is asked at a
  terminal with a plain warning and recorded, never on by default, never
  through an agent tool or the page; made only for a listener on the
  wildcard address or on this machine's address on the gateway's network,
  and ended with that listener. The router's mapped address is the only
  address ever verified for advertising: advertised after a member
  reaches it, while one has within three hours, while the mapping exists
  and while its lease has not run out; a new mapping starts unconfirmed,
  two failed checks in a row withdraw it, and checks a changing mapping
  asks for are paced at one per five minutes.
- **A new file in the home, `portmap.json`**, not a table: what would
  remove each mapping that may exist, written before each request, so
  that `net off`, a stop, or the next start after a crash removes it.
  It is not a database table because it must be written before a UDP or
  HTTP request that is not part of any store transaction, and read by a
  run that may have networking off. It holds every field a deletion
  needs, the external port separately from the external address (a
  mapping noted before the router answered has no address, and the second
  review found such a record would have deleted port 0). It is written to
  an exclusive private temporary file, synced, renamed and the directory
  synced, and read strictly: a record that cannot be used whole, or one
  for another router, makes the daemon send nothing and keep the file,
  and `status` says what to remove by hand. A UPnP forward is deleted only
  after the router says it is this machine's and a2p2's. A removal that
  fails is shown and retried. A crash leaves a finite mapping until its lease ends (at most 2
  hours), and a permanent UPnP rule, granted only when the router refuses
  finite ones, until the next start on that network or a person removes
  it on the router.
- **Declined from the second review:** guarding against hand edits of the
  recorded networking decision. Whoever can edit the home is the user
  (DESIGN 10).
- **Declined from the review:** an empty address list withdrawing a
  machine's earlier addresses. A listener that briefly advertised nothing
  would be forgotten by every member that can reach it only at a stored
  address. A non-empty list already replaces the machine's own earlier
  word.
- **Not done:** hole punching (DESIGN 11 says why); port mapping and
  temporary-address detection on Windows, which report "not available";
  CGNAT and corporate networks cannot be fixed from the machine, and
  `net check` says so.


## D49. Attachments, built to DESIGN 12 and PROTOCOL 10 (2026-11-24)

The mechanism of the reviewed attachments design is built in `core`,
`store`, `node` and `peer`, and the daemon serves it. What it adds, and why
each is needed now:

- **Migration 13** (`file_sets`, `file_items`, `files`, `file_parts`,
  `file_for`, `file_obs`, `file_wants`, `file_lists`, `file_copies`,
  `file_trash`, `room_files`, and `requests.draft` and `requests.terms`):
  file state, wants, the deletion queue and the room setting must survive
  a restart and change in the same transaction as the record they belong
  to. Columns beyond PROTOCOL 10.8, each for one rule: `file_trash`
  records what it removes (kind, room, message, hash, size), so a queued
  deletion never removes bytes a live row names and a copy is removed only
  if it is still the file written; `file_wants.list` and `sent_at` mark a
  manifest's want and pace repeats; `file_parts.at` dates a part;
  `file_copies.size` and `tmp` let a start remove only the files a row
  proves a2p2 wrote.
- **Migration 14** (`file_copies.ident`, `file_trash.ident`,
  `file_trash.unlinked`): a copy's deletion removes only the file a2p2
  linked there, by its file identity, never a byte-identical file an agent
  put at the name; `unlinked` marks a removal whose directory could not
  be synced, retried at start (Windows).
- **`want` in the pull answer** and **`GET`/`PUT /p/v2/file/...`**: as
  PROTOCOL 10.3 to 10.5; old machines ignore `want` (tested with the 0.1.0
  decoder).
- No new direct dependency. Names are not NFC-normalised (it would need
  one); a combining mark becomes `_` in a copy's name.

Departures, each the smallest that keeps the model's rules:

- F1's deletions run in their own transaction (`SweepFiles`) after each
  pull, each replace or withdraw written here, each background tick and
  at start, not inside the record's transaction. Serving, fetching,
  uploading and publishing a copy each decide under one view: the
  membership and the rows read with no commit between (retried, and
  refused if it never settles; an undecided permission is a refusal).
  The decision is held as a lease that asks again before every read of
  at most 64 KiB when anything was committed or the clock passed its
  bound, and a watcher ends it, cutting a read or write blocked on the
  network. The rule is asked once more inside the transaction that makes
  bytes or a copy live, so a sweep that has not run yet widens nothing.
- A manifest's list is bound to its message: a record whose header
  commits to another message's manifest gets no list, no want and no
  bytes of its own (fix-files-a2 B1).
- **Removed, for 0.2** (fix-files-a2): resuming a transfer across a
  restart (every part is dropped at start and a transfer starts from
  zero; within a run a transfer also starts from zero); reusing a copy
  already written for an agent (each fetch writes and checks a new one,
  and the earlier one is queued); copying a manifest's items from another
  record. Fewer states, and the findings that lived in them go too.
- File requests are admitted by their own per-machine bucket, so file
  traffic never spends the pull exchange's; no transaction is held across
  a network read.
- A send with files checks its request's expiry by the machine clock
  read inside the publishing transaction, after its bytes are in place.
- Approval (fix-files-b) never links staging into place: each staged file
  is copied into a new private file while hashed and published only at
  the approved size and digest. The terms digest (`a2p2 send files 2`)
  adds the thread parent, the version replaced and, for everyone, the
  member count (`SendDraft.Members`); `Files.ApproveSend` refuses a digest
  that is not the request's (`terms_changed`) and is the only way to
  approve one (the general confirm path only declines it; the code is in
  `node.ErrorCodes`). Staging refuses Windows
  remote and device paths by name before any I/O and shows the path the
  open handle reports (standard library only: `F_GETPATH`,
  `/proc/self/fd`, `GetFinalPathNameByHandleW`). File errors are
  `store.FileError` (operation and code, never a path). On the author's
  machine only the agent that wrote a message fetches it unshown.
- Staged bytes are counted from the pending request's draft, and a
  request being staged holds an in-memory reservation, instead of a
  `files` row in state `staged`.
- A copy for an agent is published with a hard link from its temporary
  file (no replace on any system); a file system without hard links
  cannot take copies.
- On Windows a directory cannot be synced from Go: a2p2 relies on NTFS's
  metadata journal there and repairs at start what a crash can lose (see
  `store/files_windows.go`).

**Revisit if** a harness needs files pushed rather than fetched, or the
budgets prove wrong in use.

## D50. A faster gate, tests run in shards, and a release workflow that does not repeat them (2026-11-24)

The owner asked for faster releases and faster iteration without testing
less: "parallelize tests, improve the performance, slim out, profile".
Measured on the development machine (18 cores) on the same commit:
`make check` took 288 seconds, almost all of it `internal/node`, whose
tests Go runs one after another; `make prerelease` ran its five steps one
after another in about 13 minutes (769 seconds); and the release workflow ran `make
check`, `make lint` and `make vuln` again on a two-core runner, about 20
minutes, once past Go's ten-minute test limit.

- **Shards from outside the package.** `scripts/gotest.sh` compiles each
  package whose tests took 20 seconds or more (by
  `scripts/test-timings.tsv`, refreshed with `make test-timings`) once and
  runs its top-level tests as several processes of that binary, each with
  `-test.run` an anchored list of names and its own `TMPDIR`, balanced by
  recorded times. It fails if the shards are not exactly the listed tests
  or a shard did not report each of its tests once. The same tests, flags,
  seeds and race detector run as before. `t.Parallel()` inside the
  packages was not used: other work was changing them at the same time,
  and running tests of one package in one process at once would have
  needed every one of them checked for shared state; separate processes
  share nothing but the disk, and the tests use only temporary
  directories and port 0.
- **Steps at the same time.** `make check` runs its eight steps together
  (`scripts/gate.sh`), and `make prerelease` runs check, lint, vuln,
  check-linux and chaos together, each to its own log, one line per step
  with its time, the full log of a failed step, and a non-zero exit
  naming every failed step. `make check-linux` uses the same shards in its
  container.
- **The tag is gated locally.** `make prerelease` writes
  `.prerelease/stamp` (commit and time, git-ignored) only when every step
  passed on a clean tree. `make tag VERSION=...` refuses unless the stamp
  names HEAD, the tree is clean, HEAD is on `origin/main`, the changelog
  has the section and the tag is new; then it tags and pushes.
  `make verify-release` waits for the workflow and checks the published
  assets, the checksum and the version from this machine.
- **The release workflow only builds and publishes**, with `go vet`, the
  changelog check and the version check. It no longer runs `make check`,
  `make lint` or `make vuln`: they ran on this commit locally, on macOS
  and on Linux in a container, minutes before. Running them again on a
  two-core runner took about 20 minutes and, with a release candidate,
  doubled the time from a green gate to a release. The release job now
  builds in about 90 seconds on two CPUs without a cache (measured in a
  container limited to two CPUs here; not yet on the runner).
- A release candidate is now optional, for releases that change the wire
  protocol or the database.

Cost: the hosted workflow trusts the local gate. `make tag` refuses a
commit without a passing `make prerelease`, but a tag made by hand with
`git tag` and `git push` is published without any test having run on it.
Accepted: one maintainer, who uses `make tag`, and docs/RELEASING.md says
not to tag by hand. Also, running everything at once loads the machine
more, which exposes timing races: in seven runs of `make prerelease` and
five more of `make check` on 2026-11-24, two tests with tight waits failed once
each (TESTING.md names them), and the recipes check failed once because
a `grep -q` under `pipefail` made the `printf` feeding it fail; that
script was fixed. Revisit if a second person can push tags, if timing
failures keep recurring, or if a release goes out that the local gate
did not cover.

## D51. Attachments, the surfaces (2026-11-24)

The mechanism of D49 had no way in for a person or an agent. Each
addition is the one PROTOCOL 10.6 and 10.7 name, needed now because
without it L1 cannot be used:

- **Local API:** `send` takes `files`; `fetch` and `room/files` are new
  calls; `requests` describes a `send_files` request term by term; `trace`
  and `status` show files and budgets; an `Entry` has `files`,
  `files_pending` and `files_note`. A new entry kind, `request`, tells the
  agent that asked whether its send was `sent` or `declined`; it is
  confirmed with `seen` like any entry, recorded as a row of the `shown`
  table with key `q:<request id>` (no new table or column).
- **CLI:** `a2p2 send --file`, `a2p2 fetch` and `a2p2 room files`.
  Changing the files setting is asked at a terminal with a "yes", as
  `confirm` and `net` are (after review: `keep` makes this machine fetch
  and hold every file, `off` stops it carrying, so it is a person's
  decision); reading it is not.
- **Confirm:** `confirm` takes `terms`; an approval of a `send_files`
  request must carry the terms the CLI or the page showed (no new call).
  The request adds `body`, `reply_author` and `replaces.version`, so that
  the whole text and the effective recipients are shown.
- **MCP:** no new tool; `send` has `files` and `inbox` has `fetch`, and
  hints equal to the MCP default are left out of `tools/list` by a
  middleware, because SDK 1.7 and later write them and the list would
  otherwise pass 4,000 bytes.
- **Page:** `GET /api/file` serves a file that is `here`, as an attachment,
  and `POST /api/room-files` reads or sets the setting, both behind the
  page's token and origin checks.

Two small changes in `node` that the surfaces needed: a thread, history
or skipped view now records a text mark for a message with files, so an
agent may fetch what such a view showed it (a handled report still needs
an addressee or `ask_handled`); and a fetch after a withdraw of a
message the agent confirmed answers `gone`, since a confirmed read
replaces the receipt it looked for.

**Revisit if** agents need to be told of a decided request without
reading their inbox (a hook line).
