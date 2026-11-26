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

Since 2026-11-25 (a note; the decision stands: no update check, no
telemetry, no third-party service): "no network calls except to room
members" is no longer exact. The program connects in these cases and no
others, each after a person's decision. (1) Members' addresses: pulls,
file fetches and dial-back requests go to the addresses known for room
members, only while `net on` or `net on --outbound-only` is in force;
accepting an invite dials the addresses in the token the person pasted,
before the machine is a member. Every connection is pinned to the peer's
key. A machine that is a member also makes this machine dial back: only
its own source IP, at most two ports, a TLS handshake and no request
(D48, PROTOCOL 4.7). (2) The local router, only after `a2p2 net on
--map-port`: UDP to the default gateway on port 5351 (PCP, NAT-PMP), UDP
multicast to 239.255.255.250:1900 (SSDP) and HTTP to the UPnP URLs the
gateway names, if their host is the gateway. (3) `a2p2 net check
HOST:PORT` and `--invite`: a TCP connection and TLS handshake from the
CLI process, under a throwaway key, to each address the person names (or
the invite holds), whatever the networking state; no request is sent. A
host name in any address is resolved by the system's resolver; a2p2 has
no resolver of its own. (4) `install.sh` and `install.ps1` (D52, separate
scripts that a person runs, not the daemon): they download one release's
binary and `SHA256SUMS` from github.com/wallter/a2p2/releases.

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

## D52. An install script (2026-11-25)

People and agents asked for a one-step install, and curators of tool
lists require one. The README's routes (build from source, `go install`,
download and check by hand) each take several steps and a toolchain or
care. Added now: `install.sh` for macOS and Linux, `install.ps1` for
Windows (untried), `glama.json` for a directory of MCP servers, and
`scripts/install-test.sh` in `make check`. No new dependency, protocol
message, command or table.

- **Guarantees:** the binary's SHA-256 is checked against the line for it
  in `SHA256SUMS` before anything is installed, and a mismatch, a missing
  line or a missing hash tool refuses. Downloads are https only (a
  `file://` or loopback `A2P2_BASE_URL` is allowed for tests and mirrors).
  It never uses `sudo`, and it does not edit a shell profile, configure a
  harness or start a daemon. The file is written beside its target and
  renamed into place.
- **Does not guarantee:** it trusts GitHub's TLS and a `SHA256SUMS` from
  the same release. That protects against a corrupted or swapped download,
  not against a compromised release or account. The binaries are not
  signed yet (RELEASING).
- **Left out on purpose:** a check that the install directory is owned by
  the user, and a defence against someone who can already write to it. A
  party that can write to a directory on the person's PATH can replace
  the binary at any time, and the installer cannot fix that. Resolving
  "latest" to a tag first, so both downloads come from one release: if a
  release is published between the two downloads the checksum does not
  match and nothing is installed, which is the safe outcome, and the person
  runs it again. An atomic replace on Windows (MoveFileExW): too much
  untested code for a script nobody has run; `install.ps1` reports the
  failure and leaves the old file. wget is not supported: its https-only
  flag does not cover redirects.
- **Agents:** the skill says to download, read, then run the script,
  and not to pipe it to a shell.

**Revisit if** releases are signed (verify the signature too), or a
package manager takes over (Homebrew, winget).

## D53. The web page's script runs in Node in the gate, and `a2p2 ui --print` (2026-11-25)

0.2.2 shipped a page that never drew: a `const roomLabel` in `renderSide`
shadowed the file's own `roomLabel`, which the same function called first
(a temporal dead zone error). Nothing executed `app.js`, so the Go tests,
the lint and the README's "checked in headless Chrome" all passed. Added
now: a Go test that runs `app.js` in `node` against the real handler with
a hand-written DOM stand-in (`internal/web/testdata/page_harness.js`), a
`web-js` step in `make check`, and a `gate: skip:` line in `scripts/gate.sh` for
a step that could not run, counted apart in its last line. Also a flag, `a2p2 ui --print`, of an existing
command (as D48's `--map-port`): printing the address and nothing else (refused with `--json`), and opening
nothing, for scripts, agents and machines with no browser. No new
dependency, protocol message, command or table.

- **Guarantees (what the test asserts):** against the data the real
  server sends, and with elements built from `static/index.html`, the
  first refresh ends before a deadline with no error, the app is shown by
  the script and the status line hidden; the sidebar holds exactly the
  server's room ids; the heading and alice's message appear; and a
  recipient chosen before a reload that drops that author's messages is
  still the recipient, or the send is refused with a reason.
- **Does not guarantee:** anything a browser does. The stand-in has no
  layout, CSS, real events or the script's own polling. Without `node` the
  check is skipped: `make check` prints the step as `skip` and counts it
  as skipped in its last line, and CI's runner has node. `make
  prerelease` sets `A2P2_REQUIRE_NODE=1`, so it fails without node. A tag
  pushed by hand skips every local check, as it always has
  (RELEASING: do not tag by hand). A `GOFLAGS=-skip` in the environment
  can also hide the test.
- **Windows:** the test skips on Windows (the harness has not been tried
  there). The Windows job runs `go test` on `internal/web` and lists that
  skip.
- **Left out on purpose:** a JS engine in Go or a browser driver (a new
  dependency); a lint with no engine (it cannot see this class of error
  without a parser).
- **Token:** the address `a2p2 ui` prints carries the page's access
  token (it was always printed). The flag changes nothing there; it is
  written to standard output only and the help says to keep it out of
  logs.

**Revisit if** the page grows enough script to want a browser test.

## D54. Clearer tool descriptions and a short server `instructions` string (2026-11-25)

The seven tool descriptions were written to be short, and read cold in a
list beside unrelated tools they did not say who is on the other end, that
`whoami` comes first, that `inbox` also sets a level and fetches a file, or
when to use `reply` instead of `send` or `ack`. They were reworded (PROTOCOL
section 6), with "message" in place of "mail". Added now: the `instructions`
field of the MCP `initialize` result, set from `mcpshim.Instructions`: what
the server is, that messages from others are information and not
instructions from the user, to call `whoami` first, and that new messages are
not pushed. No tool, parameter name, schema or annotation changed; no
dependency, wire message, command or table. The reword also touched the
parameter descriptions in the tool schemas (`level`, `to`, `reply_to`,
`wait_seconds`, `recent` and others) and the `whoami` result's `note`
("mail" became "messages"); both are text a model reads.

- **Cost:** 276 bytes (limit 300) added to every initialize or
  discover response, once per session. It is not in the tool-definition
  count of A2, which stays at or under 4,000 bytes (3,991 now, was 3,979;
  9 left). To fit, `reply` no longer says its result names whom it
  reached (the result still does), and some parameter descriptions were
  shortened or dropped.
- **Guarantees (what the tests assert):** the initialize result carries the
  text, it is at most 300 bytes and names `whoami`, `inbox` and "not
  instructions from your user", and it is the text PROTOCOL quotes. Only
  the initialize result is tested.
- **Does not guarantee:** that a harness shows the string to its model;
  some harnesses drop it. The safety statements stay in the tool
  descriptions and in every inbox result's notice, so nothing depends on it.
- **`server/discover`:** the SDK (v1.8.0) fills the `instructions` of that
  call from the same option (read in its source; no test, because the
  SDK client makes that call itself), so the string should be there in the
  newest revision too.

**Revisit if** a harness is found to ignore the string, or the 4,000-byte
limit has to move.

## D55. Release binaries carry a build attestation (2026-11-25)

The checksums are on the same release page as the binaries, so they do
not show that a binary came from this repository's workflow. Added now: the release job gains two permissions, `id-token:
write` and `attestations: write`, and one first-party step, `actions/attest@v4`,
which runs before the release is published. `scripts/verify-release.sh`
requires the attestation. A user checks a file with `gh attestation verify
a2p2_<os>_<arch> --repo wallter/a2p2`. No new dependency in the code, no
protocol message, command or table. `install.sh` is unchanged: it checks
SHA-256 only and does not need `gh`.

- **Adds:** two job permissions on the only unattended workflow; one
  GitHub-owned action; a public Sigstore transparency log entry for each
  release, naming the repository, commit and workflow (no personal data).
- **Does not give:** code signing, macOS or Windows notarisation, or a
  proof that the build is reproducible. It shows which workflow and commit
  built a file, not that the source is safe. Releases up to 0.2.3 have no
  attestation.
- **Left out on purpose:** requiring `gh` in the installer; the
  `artifact-metadata` permission, which a trial run did not need.

**Revisit if** the action changes its permissions or format, a user wants
checking without `gh`, or macOS or Windows signing is added.

## D56. Lookups by one key read only their matches, without a new index (2026-11-25)

Measured with real daemons on one laptop (`make bench-local`, case M7),
two queries read every record of a room, so their cost grew with the
room's history. A record's dependants (`resolveDependants`, run on every
record stored: `ref = ? OR parent = ?`) took a send's p50 from 0.40 ms
in an empty room to 4.1 ms at 10,000 messages and 10.8 ms at 30,000.
What refers to a message (`Reader.ByRef`, run once per message an inbox
shows, `ORDER BY author, epoch, seq`) made the benchmark's "first inbox
page" figure (an inbox call of up to 50 entries for an agent with the
whole history unread, plus the Seen that confirms them) 2.1 s at 1,000
messages and 75 s at 10,000. Without statistics (no `ANALYZE`), the
planner took the index matching the ORDER BY, or `records_reading`'s
(room, kind) for an OR or a term on kind, over the index of the key. The
same applied to the replies of a message and its thread (`ByParent`,
`ByThread`; the thread is read for the default recipients of a reply),
the count of a message's withdraws (every message stored) and the text a
withdraw drops.

- **What changed:** the queries, not the schema. Each seeks on the index
  that already exists for its key (`records_ref`, `records_parent`,
  `records_thread`, the primary key). An OR of two keys became two
  lookups, the second leaving out in SQL what the first matched (`ref !=
  ?` for dependants by parent; `thread != ?` for the thread's root), so
  the rows decoded are exactly the old statement's, each once, whatever a
  header says. An ORDER BY over the position became a sort in Go of the
  few rows found, by the stored author, epoch and seq columns (unique in
  a room; SQLite's BINARY collation compares bytes as Go does). A term on
  kind is written `+kind`, which no index can use, so it no longer ties
  with the key's index.
- **What the gate checks:** `TestPerRecordLookupsSeekOnTheirKeyNotOnTheWholeRoom`
  runs EXPLAIN QUERY PLAN on the nine statements (dependants by ref and
  by parent, the withdraw count, by ref, by parent, by thread, the
  thread's root, and the two withdraw updates) and requires every step
  over `records`, including the withdraw guard's subquery (alias `t`), to
  be a SEARCH whose constraint names the key; it reads SQLite's plan
  text, so it is tied to that wording. Results are checked against the old
  statements by `TestRefParentAndThreadLookupsKeepThePositionOrder` and
  `TestSplitLookupsReadTheOldRowsOnceWhateverTheStoredColumnsSay`, the
  withdraw guard by `TestAReplacementAndAWithdrawOfAHandledRecordDropNoText`,
  and room isolation by `TestLookupsByRefParentAndThreadStayInTheirRoom`.
  No test bounds time.
- **After (same laptop, same benchmark, one run each):** a send's p50 is
  0.26 to 0.27 ms at 0, 1,000, 10,000 and 30,000 messages; the first inbox
  page plus Seen takes 73 ms at 1,000 messages and 0.40 s at 10,000. That
  still grows with the unread backlog: the inbox walks the unread stream
  tails before it applies its limit. The "before" run overlapped other
  work on the machine (a test run and another agent's daemons), so its
  smaller figures may read a little high.
- **Why not an index:** an index of (room, ref, author, epoch, seq) in
  place of `records_ref` would have served the ORDER BY too. It would
  need a migration, which an older binary refuses ("database is version
  N, this binary understands M: upgrade a2p2"), so 0.2.3 could no longer
  open a database a newer daemon had touched; and in process it cost
  about 3 µs (2%) per record stored; building it on a room of 30,000
  records took about 40 ms. The sort needs neither.
- **Costs:** none on the wire or on disk. The plans now depend on how
  the statements are written, which the test guards on the bundled
  SQLite.
- **Still read room-wide (not changed):** the pull summary
  (`Reader.Summary`, every exchange), `applyAccess` when access changes,
  `Writers` (status), `MissingBetween` (alerts; the whole table), the
  replacements read by the file sweep, and the inbox's walk of unread
  stream tails.

**Revisit if** a SQLite upgrade changes a plan (the test fails), the store
starts running `ANALYZE`, or the pull summary or the unread walk shows up
in a measurement.

## D57. Separate request budgets, and a stored log that does not load sets one room aside (2026-11-25)

A security review showed two things. One hostile admin could fill the
32-request queue of the other admins with entries that wait for a second
admin, so a removal of that admin could not be proposed and was never
raised (F1). And any stricter receipt rule, or a reworded reason, would
stop the daemon starting after an upgrade, because a start failed when
one room's stored log did not parse or replay with the same reason text
(F71). The other fixes of the review add receipt rules, so this has to
ship first.

- **What changed.** The person's own requests (proposals, invite accepts,
  sends with files) keep a budget of 32 per room, checked again in the
  transaction that stores each one; a proposal to remove an admin passes
  it once per target. Requests raised to second another admin's entry
  have their own, counting only those whose entry still waits: 32 in
  all, and 4 per signing admin. Past its 4 a signer takes only a spare
  place, one left after every other admin of the room but this machine
  keeps the places of its 4 it has not used. The first removal of an
  admin per signer and target passes both and takes no place in them.
  A pending one whose entry stopped waiting
  is closed as failed (`ReasonNotWaiting`), and raised again if the entry
  waits again. Every waiting entry without a request is raised once there
  is a place: after each batch, after a decline or failure, and at start,
  removals of admins first, then in arrival order. A raise that fails
  after a decision or at start is retried at the room's next batch or
  pull and does not stop the start. Stored entries are decoded for form
  and signature only (`core.StoredEntryFromParts`). A batch that drops
  entries (over the held or branch bound) is decided again in a new
  machine from the rows it leaves, until that drops nothing more, and
  that decision is stored: otherwise a refused entry released by a
  settled fork, waiting for a parent and dropped over the held bound, left
  its dependant refused, and the next start set the room aside. The
  replay compares
  dispositions, not reason text, keeps the new reason in memory and
  leaves the stored text as it is. A room whose stored log does not
  parse, verify or replay is set aside: not served or pulled, every
  write to it refused (`not_allowed`: sends, replaces, withdraws, handled
  reports, sends with files, Seen and a change of its files setting;
  Seen across rooms leaves its keys `not_shown` and confirms the others),
  nothing deleted by setting it aside, listed in `status` (`quarantined`)
  and `a2p2 doctor` with a fix, and it may be left unless it is the local
  room. Its messages can still be read; a read still records what it
  showed, which marks nothing seen. Its reading frontier is unknown, so
  a stream without a cursor is held back and missing text is neither
  shown as expired nor marked expired by a prune. Other text past
  retention expires, and the file sweep drops only files whose text is
  withdrawn or past retention and staged sends past their staging life
  (`store.SweepRetained`); the stored member cache and wants decide
  nothing. Leaving a room runs its file deletions after the rooms lock is
  released (F68).
- **No wire, entry or schema change.** Old and new machines see the same
  entries. Requests are local rows. By reading the code, a database
  written by 0.2.0 to 0.2.3 opens, and they can still open one this
  version wrote, because no stored reason is rewritten at start. The
  tests edit a database this version made, and
  `TestDatabasesFromEachReleaseOpenReplayAndKeepWorking` (D61) opens the
  databases releases 0.2.0 to 0.2.3 wrote and checks that no room is set
  aside (F73). `status` gains one optional field.
- **Why not reason codes on disk.** Storing codes in `log.reason` would
  need a migration, and 0.2.x compares the text it wrote, so a downgrade
  would stop at start. Comparing dispositions alone removes the text from
  the check without that.
- **Costs.** Each batch where this machine is an admin reads the room's
  pending seconding requests (an index lookup) and looks at every entry
  that waits for a second admin, not only the new ones. While one waits,
  it also reads the id, entry and state of every seconding request the
  room ever had (no drafts), under the rooms lock. That is bounded by the
  log, which has no cap of its own. A flooding admin still costs the other people one decline per
  entry, and while the room has spare places it can hold up to 32 less
  4 for each other admin at a time. In a room of more than 9 admins,
  admins under their own 4 can fill the 32 together. A batch that drops
  entries decides the room's kept log again, so a peer that keeps the
  held bound full pays that for each such batch. A room set aside is
  unusable until the person runs the version that stored it (the
  repair), or leaves it and joins it again (not the local room).
- **Still open.** A "decline all from this machine" action and showing
  which admin signed each waiting entry need new UI and wording. A change
  of disposition under a new rule (a per-signer cap in `members`) sets
  rooms aside rather than adopting the new decision; adopting it needs a
  versioned re-decide (a rules version in `meta`). A stored record that
  fails a later record rule still breaks reading that room (audit r2-4
  item 2). The
  warning that a removal targets the only member this machine reaches
  (F21) is not built and is deferred out of this change; it needs pull
  state the scheduler owns. A quarantined local room cannot be left; the
  fix is to run the older version.

**Revisit if** a release adds a receipt rule or changes `members`
decisions, or a person reports a room set aside.

## D58. Text from other machines is escaped where it is shown (2026-11-25)

A security review showed that a room label chosen by a hostile room
creator went raw into the text a person approves for a membership
change. A carriage return, terminal escapes, a right-to-left override or
a newline could hide that a seconding request makes the creator's second
machine an admin. The page also drew labels, names, message text and
choices as sent, and the escape helper missed characters that draw as
nothing (zero width, tag characters, variation selectors). Rooms that
span owners make this a real attack now, so it is fixed before the CLI
and MCP work that will use the same helper.

- **What changed:** one helper, `internal/node/escape.go`:
  `EscapeDisplay` (one line), `EscapeDisplayLines` (approval text over
  lines), `EscapeDisplayText` (message text: keeps newlines, tabs, a
  joiner between two emoji, a joiner or non-joiner between two letters
  of a script that needs them, and one emoji selector after an emoji or
  keycap base), `EscapeChoice` and `ChoiceHidden` (a choice, shown
  exactly as it is sent or refused), `DisplayHidden` and the tables. The
  table is every Cc, Cf, Zl and Zp character, every other default
  ignorable code point and every variation selector, plus a few
  invisible ones of other classes; a test checks it against Go's Unicode
  tables, and another that `app.js` has the same tables and gives the
  same output. Request text escapes the room label and names. A
  seconding request says the other admin chose its name, shows the name
  the room records and names other members with the name shown (bounded).
  The local API's request gains `name_from` and `recorded_name`; no new
  call, tool, command, key or table. The page's approval is compared
  with the text it showed again under the decision lock. On the page,
  room labels, names, agent ids and message text are escaped; rooms
  drawn alike get their ids; a message that asks the person is drawn
  apart from requests with who asked and of whom; a choice its button
  cannot show exactly as sent is not sent.
- **Per position:** escape visibly wherever remote text is shown,
  including signed data, which must still be shown and still load; refuse
  only where this machine would send a remote string as its person's own
  words (a clicked choice). Nothing is dropped.
- **Why at display, not on receipt:** stored logs are signed and must
  still load, and a new receipt rule needs the replay change first. A
  later rule that refuses controls in new room labels can come after it.
- **Second review (2026-11-25):** the first rules mangled honest text
  and left gaps, so:
  - One-line positions (labels, names, file names, approval text, the
    page's download name) use the text rules without line breaks, so an
    emoji with a selector or joiner and a Persian or Indic joiner show
    as written.
  - The text rules also keep a right-to-left or Arabic letter mark
    between two right-to-left letters (a fixed table of code points, so
    a browser with an older Unicode agrees), a zero width space between
    two Thai, Lao, Tibetan, Myanmar or Khmer characters, one
    ideographic variation selector after a CJK ideograph, and the three
    tag flags Unicode recommends. In message text a CRLF is one line
    break.
  - The escape reads back: a backslash before `u{` or `x{` is written
    `\u{5c}`. Escaping is no longer idempotent, so the page escapes
    the daemon's sentences again without that rule.
  - The room label, names and agent ids in approval text sit between
    U+2068 and U+2069 (`Isolate`). The page draws each in a `bdi`
    element; the CLI puts one holding right-to-left text on its own line
    with `[n]` in the sentence. The page compares labels after NFC,
    collapsing white space and dropping the invisible characters the
    rules keep.
  - The CLI escapes entries, alerts, the room list, status, doctor and
    `net check`; the question alert is escaped in the daemon; MCP tool
    results escape message text with the text rules and labels, names,
    agent ids, choices and file names for one line. The tool
    definitions are unchanged. `--json` prints the stored strings.
  - The local API's `confirm` gains one optional field, `text`: the
    request's text as shown. With it the daemon refuses either decision
    (the page's `RequestChangedText`) when the text changed, and checks
    an approval again under the decision lock. `a2p2 confirm` sends it.
    An older client that leaves it out is decided by id, as before.
- **Third review (2026-11-25):**
  - `net check` printed members' addresses raw. The address book keeps
    any string of up to 255 bytes another member sends, and a probe's
    detail can quote it. The CLI now escapes every string of
    `net check` that is not its own words: addresses, details, names,
    labels, what members saw, their dial-back errors, the router's
    answers and the advice, as one line. The same sweep of `cmd/a2p2`
    escapes status's reach line, the terms of a send with files (agent
    ids, the parent's author, message ids), a join's answer, a fetch's
    reason and members' answers, a pending change's kind, doctor's
    error lines, keys cut short, unparsed time stamps and the last line
    of an error. `--json` is unchanged.
  - Labels compared as drawn collapsed only ASCII blanks. Now every
    Unicode white space character and U+2800 count as a space, in the
    page's `drawnLabel` and in `node.LabelKey`, which status uses. The
    shared table of test strings has the cases.
- **Costs:** text that had such characters reads with `\u{...}` in it,
  and a typed `\u{` reads `\u{5c}u{`. A joiner in Latin, Greek or
  Cyrillic text, a selector after punctuation, a right-to-left mark at
  the edge of a word or next to a digit, a joiner at the end of a word
  and any tag sequence other than the three flags are shown escaped.
  What the text rules keep can still carry about one bit per position
  nobody sees (an ideographic variation selector, about one byte per
  ideograph), inside emoji and those scripts only; escaping those would
  break them. A choice with a non-breaking space or two spaces in a row
  is refused. Approval text and its alerts carry U+2068 and U+2069, also in
  `--json`; a page open across the upgrade must reload before it can
  approve, since the request text changed. Lookalike letters from other
  scripts are not detected.
- **Still open:** names are not compared with this machine's own name
  when it is not yet recorded in the room, and the CLI's check for room
  labels shown alike (`node.LabelKey`) does what the page's does but
  does not normalise to NFC. Addresses are still accepted on receipt
  without a check of their form; that is a separate change.

**Revisit if** Unicode adds a format character (the table test fails on
a Go upgrade), a script needs a character the table escapes, or receipt
rules for labels land.

## D59. Inbound slots by pool and join limits by source group (2026-11-25)

A security review showed that any host that reaches the peer port could
hold all 256 inbound connections, with raw TCP or with TLS under keys it
made up, and lock members out of a listening machine (F4, F17). In the
room shapes a2p2 is built for, laptops that cannot accept connections
depend on one listening member, so that one host could stop the room.
The join route had the same flaw by address: one IPv6 /64 filled its
table and refused every joiner, and a budget of 60 polls a minute shared
by all sources refused every resume by hash (F6). Every refusal was an
Info line in an uncapped log (F19, F26). Fixed now because rooms span
machines of different owners and the listener is on the open network.

- **What changed.** The listener does each TLS handshake itself and
  counts connections in three pools: 256 for keys that are members of a
  room held here, at most 16 of them for one key; 32 for other keys; and
  64 still in the TLS handshake, cut at 5 s (the bound the 5 s header
  timeout set before). A connection is placed only after the handshake
  has finished, because crypto/tls calls `VerifyPeerCertificate` before
  it checks the client's signature, so a copied certificate would
  otherwise take a member's slot. Outside the members' pool, one IPv4
  address or IPv6 /64 holds at most 8 connections, one /56 16 and one /48
  32. When those are full, an IPv4 address or IPv6 /64 that completed a
  member's handshake in the last 24 hours (in memory, at most 1,024) may
  use 16 more handshake slots, at most 4 at once for one, counted in none
  of the shared groups, so no /56 or /48 count keeps it out. A
  connection there that does not prove a member's key is closed and its
  group is forgotten. The membership lookup after the handshake is cut at
  the same 5 s deadline; lookups running at once are bounded by twice the
  handshake slots. A connection that finds no slot is closed at once,
  instead of waiting in the operating system's queue. Membership is
  checked again for every request: a key that became a member moves to
  the members' pool, a key no longer a member is served as a stranger and
  its connection ends within 15 s, and a key added back within those 15 s
  is a member's again. A listener that fails for good closes the
  connections it accepted. Session tickets are off, since a resumed
  session presents no certificate. A stranger's connection serves one
  request at a time (a second one at once on HTTP/2 is refused 429 before
  its body is read), every answer to a stranger carries
  `Connection: close` (a GOAWAY on HTTP/2), and its connection lasts 15 s
  at most. Strangers have rate buckets of their own on every route.
  Refusals of strangers are logged at Debug, a member's at Info 5 at once
  and then one per 10 s per key. The join route counts per source address
  80 bodies read a minute, before the body is read, then 10 opens and
  accepts and 30 polls; the addresses of one IPv6 /64 together get 4
  times each. Polls naming unknown invites are counted per IPv4 address
  or IPv6 /64 (`node.SourceGroup`), 10 a minute instead of 60 a minute
  for everyone, charged after the lookup, so a poll for an invite held
  here is never refused by them. A joiner sends an accept answered 429 or
  503 again after its `Retry-After` (at most 60 s), three times in all,
  where it used to report an unexpected answer. The tables hold 4,096
  addresses or /64s each and forget the one heard from least recently, in
  constant time (`node.Recent`).
- **Costs.** One `SharesRoom` call, under the rooms lock, per handshake
  and per request. A stranger opens a new connection for each request: a
  joiner already did. A machine admitted by entries in its own request is
  served as a stranger until the log here names it; a waiting pull it
  started then is cut at 15 s, and its next request is a member's.
  Machines behind one IPv4 address share 8 handshake slots, and 4 kept
  ones once a member came from there; a handshake holds its slot about
  one round trip, so about 80 new connections a second at 100 ms. 0.2.3
  had no share per address and queued connections past 256 instead. A
  stuck lookup leaves a goroutine until the rooms lock frees. Invite
  lookups are bounded by the join route's budgets (40 a minute per
  address, 160 per /64), not by the budget for unknown invites.
- **Wire and storage.** No message, field or table changed. Old and new
  machines work together; a refused client sees a failed handshake or a
  closed connection and retries under its back-off. The handshake bound
  stays 5 s, as before.
- **Still open.** Hosts in 4 source groups (4 IPv4 addresses, 2 IPv6
  /56s, one /48) keep the strangers' pool full, which keeps joiners out.
  Hosts in 8 groups (8 IPv4 addresses, 4 /56s, 2 /48s) keep the shared
  handshake slots full, so members cannot open new connections, except
  from a source group known from a recent member handshake; existing
  member connections keep working. A host at a member's own IPv4 address
  or /64 can hold that group's 8 shared and 4 kept slots and so keep that
  member out. The known groups are forgotten when the daemon restarts.
  Sixteen member keys can fill the members' pool. A host with many /64s
  can make 160 invite lookups a minute in each, with no total bound but
  the connection slots; it refuses no one else. Joiners behind one IPv4
  address share its join budget, and those of one /64 share 4 times
  that; a host there that spends it delays them, and an older joiner
  still reports a 429 to its accept as an unexpected answer. At Debug
  each refusal is still a line, and `daemon.log` is still not rotated (another
  group's fix). Keys that share no room still update the "connected in"
  record (F25, another group's fix; the pool is known per request, so the
  check is one line in `guard`).

**Revisit if** members behind one NAT report refused connections, joins
through a busy admin machine fail under load, members report refused
connections during a flood from more groups than the limits stop, or the
rooms lock shows up in handshake or request times.

## D60. Installers approve no a2p2 tool unless the person asks (2026-11-25)

The security review of 2026-11-25 (F48) found that `a2p2 install codex`
wrote `default_tools_approval_mode = "approve"` under `[mcp_servers.a2p2]`
on every install, as D38 chose. That lets every a2p2 tool run
in Codex without a prompt: `send`, `reply` and `ask_human`, and any tool a
later a2p2 adds. A member of a room shared with another owner could ask a
Codex agent to post a file's contents or a summary of the repository, and
the person would not be asked. The team launcher did the same for every
headless member. In the same group: Pi's skill put message text inside
double quotes for bash (F16), install wrote any binary path into hooks and
MCP entries, a temporary one included (F50), and the Windows advice named
`C:\a2p2`, which every signed-in user can change by default (F24). Why now:
rooms between owners are the use the project is built for, and these are
the paths by which another owner's text reaches a send or a shell without
the person.

What changed:

- **No approval by default.** `a2p2 install codex` writes none, and
  removes the line earlier versions wrote. `--approve-tools` (a new
  install flag) writes it with its own marker, and the report and the dry
  run say that `send` is among the tools. Doctor has a `codex tool
  approval` line: a fault for the unasked line, a statement for any other.
  For the other harnesses the flag does nothing and says so. The edits and
  doctor use the `config.toml` the Codex CLI writes, in `$CODEX_HOME` when
  that is set. Each install changes the approval in one write that is made
  only if the file still holds what a2p2 read. A failed `codex mcp add`
  still removes the lines a2p2 wrote. When the person's own
  `default_tools_approval_mode` is in a2p2's table, in any spelling TOML
  allows (quoted, literal, escaped, spaced dots, an escaped header),
  `--approve-tools` adds none, since a second key would make the file
  invalid. When a line that could set something in a2p2's server is one
  a2p2 does not read in full (an inline table, or a key or header it
  cannot decode), it adds nothing, names the line and gives the line to
  add by hand, and doctor's line fails saying it cannot tell.
- **Guarded writes.** Every write to a file a harness or the person also
  edits (hook files, the three adapters' files, a harness CLI's file when
  an entry is put back, uninstall's restores and removals) reads the file
  again just before the rename and refuses, naming the file, if it
  changed; a backup made for that write is taken back. The Codex approval
  edit reads and edits again, up to three times. The instant between that
  read and the rename remains: no harness takes a lock a2p2 could share.
- **Team launcher.** A manifest with a Claude Code, Codex or Antigravity
  member must say `"approve_a2p2_tools": true` (a new manifest key).
  Only `up` asks, so `status`, `stop`, `logs` and `report` work for a run
  started without the key. The consent is saved with each member's
  command line, and the supervisor does not start a Claude Code, Codex or
  Antigravity member whose saved command line lacks it. The state
  directory is made the user's alone. Claude Code members are allowed the
  seven tools by name.
- **Pi's skill** writes the label out in every command and sends text only
  through a heredoc with a quoted end word. The end word has eight random
  hex digits chosen at install and kept on a reinstall, so a peer cannot
  write the line that ends it early. Doctor fails a skill that an older
  a2p2 wrote.
- **The binary.** Install refuses a binary in a temporary directory or Go's
  build cache, one that it or a directory above it lets every user change,
  one owned by another user, and a path with a control character. It
  checks the path as written, which the harness runs, as well as where a
  symlink in it leads now, and the existing part of a path that does not
  exist yet. A
  group-writable directory warns, since that group is often the
  administrators. Doctor reports the same. Directories install creates are
  0700. The Windows advice says how to make a folder the user's alone.
- **Codex `config.toml` edits** skip lines inside multi-line strings and
  arrays, so text that reads like a2p2's table is neither written into nor
  removed.

What it costs: a headless `codex exec` through the installed configuration
refuses a2p2's tools until the person passes `--approve-tools`, and an
interactive Codex session may ask before tools it used to run silently.
Existing team manifests are refused until they add the key. A person who
runs install from `/tmp` or with `go run` must copy the binary first. No
wire, record or database change.

Still open:

- Codex 0.171.0 has a per-tool `approval_mode` (seen in its binary's
  strings). Approving only `whoami`, `peers` and `inbox` that way would be
  a better default, but the key's form has not been run against Codex and a
  wrong one could stop Codex reading its configuration. Needs one real run
  in a scratch `CODEX_HOME`.
- On Windows the binary's access control list is not read. Doing it means
  sharing the SDDL reader in `internal/daemon` (acl.go, sys_windows.go)
  with `internal/harness`, with a rule for write, delete and write-DAC
  rights instead of the home's rule against any access, and a run on
  Windows.
- OpenCode's default agent and Pi run a2p2's `send` without asking, by
  their own design. a2p2 could write an OpenCode `permission` entry that
  asks, which edits a key the person owns; not done.
- A model that ignores the skill can still put text in double quotes, or
  copy the skill's own end word into the text.
- An agent can pass `--approve-tools` itself; a flag cannot show that the
  person chose it.
- Doctor's `a2p2 binary` line checks the binary that runs doctor. A
  harness entry that names another binary already fails its own line
  ("points at a different a2p2 binary"), and the fix is a reinstall,
  which checks that binary.

## D61. Pinned actions, a write job without repository code, and checks the installer can opt into (2026-11-25)

The 2026-11-25 security review found that the release job ran actions
named by moving tags while it held `contents: write`, `id-token: write`
and `attestations: write`, and that its checkout left the token in
`.git/config` (F15). A moved tag of `actions/setup-go` could change the
build, and the attest step would then sign the result as this
repository's. It also found that the installers trust a `SHA256SUMS` from
the same release page as the binary, and that `verify-release.sh`
checked only the binary of the machine that runs it (F49), and that no
test opens a database written by an older release (F73). The Windows
install advice pointed at `C:\a2p2`, a folder that every local user can
write when it is made by hand. These are fixed now because the next tag
would run the workflow, and the fixes need no protocol change.

- **Workflows:** every action is pinned to the full commit SHA of the
  release its major tag named on 2026-11-25, with that release in a
  comment: `actions/checkout` v4.4.0, `actions/setup-go` v5.6.0,
  `actions/attest` v4.2.2. Every checkout sets `persist-credentials:
  false`. The release workflow is split: `build` (read-only token,
  checkout, Go, `make release`) hands `dist/` to `publish` as a workflow
  artifact; `publish` holds the three write scopes, checks out nothing,
  checks that the files are exactly the six binaries, `SHA256SUMS` and
  the notes and that `SHA256SUMS` has exactly one well-formed line for
  each binary, checks them against it, attests them and creates the
  release. That adds two GitHub-owned actions,
  `actions/upload-artifact` v7.0.2 and `actions/download-artifact`
  v8.0.2 (which fails on a digest mismatch), both pinned.
  `scripts/workflows.sh`, run by `make check` (`make release-test`),
  reads every `.yml` and `.yaml` workflow and refuses a tag or branch
  pin, a checkout that keeps its token, a write scope at the top level,
  and a write job (quoted values included) that checks out code or sets
  up Go. It reads lines, not YAML, so it also refuses the syntax it does
  not follow: tabs, quoted keys, flow mappings, anchors and aliases, a
  `uses:` that does not start its line, a job not at two spaces. It
  skips the text of block scalars. No new workflow or trigger.
- **verify-release.sh** downloads each of the seven expected files by
  name, requires the download to hold exactly those, requires
  `SHA256SUMS` to be exactly one `<sha256>  <name>` line for each binary
  and no other line, and checks the checksum and the attestation of each
  binary with `--source-ref refs/tags/<tag>` and
  `--deny-self-hosted-runners`, before it runs this machine's binary.
  `scripts/release-test.sh` runs it against a stub `gh` and a fake
  release, and runs the publish job's file check, taken out of
  `release.yml`, on fake `dist/` directories.
- **Installers:** `A2P2_VERIFY_ATTESTATION=1` (a new setting of both
  scripts, default off) makes the installer run `gh attestation verify`
  pinned to this repository, `release.yml`, the tag it downloads
  (`--source-ref`) and a GitHub-hosted runner, and install nothing
  unless it passes. Without the tag, someone who can edit releases could
  put an attested build of an older tag, and its `SHA256SUMS` line, into
  a newer release. With the setting and no `A2P2_VERSION`, the installer
  asks `gh` once for the latest tag and downloads from that tag; it
  cannot rule out an older release marked as latest. The setting is off
  by default because `gh` must be installed and signed in, and the
  latest release (0.2.3) has no attestation; a check that falls back to
  "skipped" on failure would not stop a swapped file. Without it the
  installer now says the checksum does not show who built the file.
  `install.sh` refuses an install directory, or one above it, that every
  user may write without the sticky bit, that another user than root
  owns, that is a symbolic link another user owns, or that a group may
  write (mode bits, no sticky bit) when the group has a member other
  than the user and root. Members are the group's listed members and the
  users whose primary group it is, from `getent` (on macOS without it,
  `dscacheutil` and `dscl`). On macOS the groups `everyone`,
  `localaccounts` and `netaccounts`, and any group `dsmemberutil` says
  the account `nobody` is in, hold accounts those lists do not name, so
  they are refused. A group it cannot list gets a note and the install
  goes on, so a group-writable `~/.local/bin` whose group holds only the
  user, or a Homebrew prefix that the group `admin` of only the user
  and root may write, installs. Accounts of a network directory that the
  system does not list, nested groups and access control lists are not
  read. It takes trailing slashes off each path before it looks at it, so
  `link/` is judged as the link. It walks the
  path as written, and each link it meets adds the path the link names
  (`readlink`, or the resolved path from `cd -P` without it), walked the
  same way. It makes missing levels with mode 0755 and
  checks the path again after. `install.ps1` refuses a junction or
  symbolic link in the path, a directory owned by an account other than
  you, SYSTEM, Administrators or TrustedInstaller, one whose access list
  it cannot read, and one another account may change. It makes a missing
  directory with its own access list already in place (you, SYSTEM,
  Administrators, nothing inherited), checks it is a real folder, still
  yours and empty, and checks the path again before it copies the file.
- **Compatibility test:** `scripts/release-db.sh vX.Y.Z` runs
  `internal/node/testdata/releases/gen_test.go.txt` in a copy of the
  tag's source (git archive; no daemon) and writes three machines'
  databases as SQL text with a manifest. A run holds a lock and replaces
  the committed set only with a complete new one. Databases from v0.2.0
  to v0.2.3 are committed (about 380 KB of text).
  `TestDatabasesFromEachReleaseOpenReplayAndKeepWorking` requires all
  four and opens each with the current code: logs replay, records,
  members and admins match the manifest, alice's unread messages and
  bob's read state are kept, and a and b each send to the other. All
  four were written at schema 14, the current one, so no migration runs
  yet. A trial rule that refused the label `bee` made it fail, as an
  F71-type change would.
- **Costs:** two more actions in the only unattended workflow; a release
  takes one more job and an artifact kept for seven days, so the publish
  job can be re-run alone within that time. Moving an action now
  means changing a SHA by hand. A step in `make check` of a few seconds.
  Each release adds a directory of test data. An install into a
  directory another user can change, which worked before, is refused.
- **Still open:** a compromised pinned action or Go toolchain in the
  build job still produces a binary that `publish` attests; an
  attestation names the workflow, not the honesty of its inputs. No
  dependabot or other update path for actions and modules: it would open
  pull requests that run CI, and the owner keeps hosted CI small.
  Repository settings the files cannot show or set: immutable releases,
  a protected `release` environment with required reviewers for
  `publish`, and the tag rule INSTALL.md describes. `install.ps1` has not
  been run on Windows. No test runs an old binary against a new one over the
  network, and the release-db step after a tag is by hand.

**Revisit if** an action is moved to a new major release, GitHub offers
immutable releases or a required environment here, the attestation check
can be done without `gh`, or a mixed-version chaos run is added.

## D62. Replaced text is marked, answers are tied to the text shown, and peer hints are bounded (2026-11-25)

A security review showed that the author of a message could replace it
after a reader answered, and every view but the inbox then showed the
new text above the answer as if it was the text answered (F30). A
handled report was accepted for a version replaced since the read (F67).
Any member could make this machine keep a stream row, for the life of
the room, for each epoch it named in an unsigned pull report (F9). A
prune failed outright past about 32,000 undecided (room, author) pairs
(F32), and an inbox read could leave receipts for a room left while it
ran (F70). Rooms that span owners make the first three real now.

- **What changed.** Message entries carry `Edited` (local API `edited`,
  omitted when false) when their text is the author's replacement, in
  the inbox, thread, branch, history, `recent` and `skipped` views.
  `Handled` needs, unless the message was withdrawn, that a read showed
  the agent the version reads show now: the newest replacement held
  with text, or the message itself while its text is live. While a
  replacement's text is missing here it is refused, as the inbox and
  thread wait for it; an expired message with no live replacement is
  not checked, and neither is one the agent confirmed no text of, since
  then every view shows only the expired notice. Each refusal is `not_shown` with its own sentence. The
  delivery model's `Handle` has the same rule, so the differential tests
  hold the two together. `Draft.ReplyVersion`, when set, refuses a reply
  by the same rule. `Tx.NoteKnown` keeps at most `MaxHintedStreams` (32)
  streams that hold nothing per author on one peer's word, lowest epochs
  first; a higher report that takes such a stream over from another
  peer counts against the peer that takes it. Such a stream is deleted
  when the peer that set its known position stops naming it, or leaves
  its author out of a whole report (`Tx.Observe`); the model drops those
  hints too. A prune expires live text in one statement and missing text
  with one statement per (room, author) pair not left undecided that has
  missing text past retention, so it binds no variable per undecided
  pair. Entries carry the room's generation their read saw, and
  `Store.ShowAt` writes no receipt for a room left, or left and joined
  again, since the read.
- **No wire, record or schema change.** The record format has no field
  for the version a reply answers, and adding one would change signed
  records, so the reply check is local and optional. Rows written before
  the bound load as before; a peer's next report lowers or deletes them
  as it would have. 0.2.0 to 0.2.3 open the database unchanged.
- **Costs.** An agent shown a message and not yet its replacement must
  read again before `ack`, and waits while the replacement's text is
  missing. An honest peer that holds more than 32 streams of one author
  that this machine holds nothing of gets the rest listed as waiting only
  as the first arrive. A prune with undecided pairs runs one more query
  and one statement per pair with missing text past retention; the test
  with 40,001 undecided pairs has one such pair, so the time for many
  pairs to update is not measured.
- **Still open.** (The CLI, the MCP results and the page show
  `edited` since the merge of this wave; see D65.) The local API's send, the page's reply, the MCP reply
  tool and a staged reply with files do not pass `reply_version`, so no
  client reply is tied to the version it drew yet. A signed reply and a
  handled report name only the message, not the version answered. F31 is
  deferred: the author's clock still orders every view and starts
  retention. A member can date a message before an answer given after
  it, or date a reply before the message it answers, so views show them
  out of order. A member can also date a message so far back that its
  text expires minutes after it arrives, since expiry counts from the
  sender's clock, not from when this machine got it. Fixing that needs
  either an arrival time per record (a migration, which 0.2.3 would then
  refuse to open), a tighter future bound in P1, or an order clamped to
  the parent's and the previous record's time in every view and the
  model; that choice is the owner's. Hint rows set by a peer since
  removed stay until another peer reports the stream (at most 32 per
  author and peer).

**Revisit if** a record format change is due (carry the version a
reply answers), or a schema change lets records keep their arrival time.

## D63. What may be staged, and what one author can reserve (2026-11-25)

A security review found four gaps in attachments (F10, F23, F33/F40,
F35). Rooms now span owners, so text from another owner's member can
steer an agent into naming a path, and that member's machine can ask
for files. Each gap needs only that.

- **What changed.** Staging opens each path once through
  `store.OpenSource`. A send is refused while 32 of this machine's
  requests wait in the room, before any path is touched. A path is then
  checked by name before anything touches it: on Windows only a drive
  letter, a colon, a separator and plain names (no UNC, `\\?\`, `\\.\` or
  `\??\` form, no colon after the drive, no wildcard, no device name in
  any component, with COM0, LPT0, the superscript names, CONIN$, CONOUT$
  and CLOCK$ added); on Unix nothing under `/dev`, `/proc`, `/sys`,
  `/net` or, on macOS, `/Network`, `/home` and their
  `/System/Volumes/Data` forms. The path is walked one component at a
  time with `os.Root`, each looked up in the directory opened before it
  and opened only if it is still the entry looked at (`os.SameFile`), so
  a directory or link swapped in after its check is refused, and a
  lookup never leaves the directory it was checked in. Every link on the
  way is read and its target checked by name before it is looked at.
  Only a regular file is opened. The handle's reported path passes the
  name check again; on Windows a `\\server\share` handle path is taken
  only for a path that began with a drive letter `GetDriveTypeW` reports
  as a network drive, as 0.2.3 staged such files. The walk refuses a
  path through the home other than into `files/`, and a file with
  several names is compared by identity with every file in the home
  outside `files/`. Approval checks a pending request again by its path
  and by the bytes of the home's small files (the keys), so one staged by
  an older binary is not sent (reason "a staged file is one of a2p2's own
  files"). Sizes, the path shown and the staged bytes come from that
  handle. The same bytes listed twice are listed once instead of refused.
  A queue that fills while staging is refused with the one staging
  refusal. At most four sends stage files at a time. A staged file with
  several names says so in the request text (draft field `links`). A
  request for a file from its own author starts no carry job, and live
  carry or `keep` wants of one author's files reserve at most a quarter
  of the room budget, each counted by the bytes not yet received
  (`core.AuthorWantShare`); a want this machine's agent asked for is not
  capped. When a want lapses, or goes because an edit made another
  version the one to keep, its part is dropped in the same transaction:
  only the author could finish it, against a live want. A part reserved
  no later than the lapse of a want that 0.2.3 left lapsed is dropped
  too; one reserved after it is an agent's fetch and stays. The delivery model has these rules. No
  wire, record, schema or tool change.
- **Why links are followed, not refused.** The links are read and
  checked one by one, the person sees the resolved path, and the open
  follows none. Refusing every link would break honest paths through
  `/tmp` on macOS or a linked checkout and adds little, since the path
  shown is the file opened. The owner can choose to refuse them.
- **Compatibility.** New rules apply to new requests only. Stored wants
  over the quarter keep their reservation, take uploads and lapse as
  before (`TestWantsAnOlderBinaryReservedOverTheAuthorsShareStillLoadAndLapse`,
  `TestLiveWantsAnOlderBinaryIssuedOverTheAuthorsShareStillWorkAndLapse`).
  A pending request staged by an older binary is approved as before,
  unless its file is one of a2p2's own. An older binary reading a draft
  ignores `links`; one that finds a part dropped at a lapse sees what a
  failed upload leaves. Checked once by hand: v0.2.3's store opened a
  database this code wrote (a pending send with `links`, a carry want
  with a part), read the draft, reconciled and swept it. No file was
  moved between an old and a new binary over the network. Machines from 0.2.0 to
  0.2.3 see only a 404 where they saw 202 for an author's own request,
  which they already handle as "not held".
- **Costs.** An agent that names a file under the home, or a symbolic
  link into it, gets the one staging refusal. A path through a macOS
  `/home` or `/Network` mount is refused. On Windows a path through a
  junction or a `\??\` link target to anything but a local drive is
  refused. A carrier asked for many large files of one author that
  never connects in carries a quarter of its room budget at a time. Per
  path component, one directory opened, one `lstat` and one `fstat`; a
  walk of the home only for a file with several names; at approval, a
  hash of each small file directly in the home.
- **Still open.** The Windows walk (`os.Root` there), link reading,
  handle path and drive type were written from documentation and built,
  not run. A network file system the person mounted, or a drive letter
  mapped to a share, is read as local, so a path on it contacts the
  host the person chose; refusing them is an owner's choice. A request
  an older binary staged from a hard link to the database is not caught
  at approval (its bytes are no longer the database's); it expires
  within a day. The room usage in local status, which agents' tools do
  not show, still counts equal files once. A cloud placeholder file can
  download its bytes when read. Below the request limit an agent can
  still learn roughly how large a file is, at the cost of requests the
  person sees. Several machines of one owner can each take a quarter of a
  carrier's room budget; a per-owner share needs owner sides.

**Revisit if** the Windows paths are run, a harness sandbox gives agents
the socket without file access, or owner sides exist.

## D64. A join into a held room counts only from a member, and a joined room keeps the label approved (2026-11-25)

A security review showed two flaws in the joiner (F7, F12). A machine
that is not a member of a room this machine holds, such as a removed
admin that has not heard of its removal, could send an invite for it.
After one approval its answer's entries were applied, a removal it
signed late among them, and the answer opened the `not_caught_up` gate
even with no heads. And the label of a joined room came from the signed
create entry without a comparison with the preview the person approved,
and could be `local`, so another owner's messages read as this
machine's local room. Rooms now span owners, so both are fixed now,
after D57, which lets a receipt rule land without stopping a start.

- **What changed.** For a room held here, a preview must name the create
  entry and the label held here; a different label is refused and named
  as a change. Each batch of the answer counts only as a pull's does
  (T4): if the admin machine is a member here before the batch or after
  it (`Rooms.receiveJoin`), or the whole batch is refused. The same test
  applies to a join that pinned the room in an earlier run, as soon as
  this machine is a member of it here by any path; the test and the
  write are one step under the rooms lock. Pages are kept in memory until
  they make the admin machine a member, up to 1,024 entries or 4 MiB,
  not counting entries held here already. Otherwise nothing is stored
  and the join ends refused with `ReasonJoinServerNotMember`. A page over
  256 entries or 3 MiB, or one marked `more` that holds nothing or does
  not move past the last `next`, ends the join (`ReasonJoinBadPage`), so
  a hostile admin machine cannot keep a resume polling at once forever. A join lifts the gate only
  when its last page lists heads, all accepted here, and the admin
  machine is a member here, as `caughtUp` does. For a new room, the
  create entry's label must equal the preview's, may not read as `local`
  (`LabelKey`, so any case and surrounding blanks) and may hold no
  character the display would escape (`DisplayHidden`: controls,
  direction marks and invisible characters outside the places honest
  writing needs them: a joiner inside an emoji, or a joiner or
  non-joiner between letters of Arabic, Indic and other scripts that
  need one). The rule runs at the
  preview and again on the answer, so a join approved under an older
  version is also refused. A join an older version recorded for a room
  held here, or pinned without comparing the labels, under a label other
  than the one held, is refused before any entry of it is applied, and
  the room stays. The preview reads the room and its generation in one
  step, so a pin cannot slip between them. A stored room labelled
  `local` that is not this machine's local room is shown with its id
  (`Rooms.ShownLabel`, which since D65 asks `ShownLabels`, so it reads
  `local#` and the first 8 hex of the id, as agents see it) in room
  info, quarantine, inbox labels, unread
  counts, levels, request texts (both where they are shown and where an
  approval compares them) and `ResolveRoom`, so `local` resolves only to
  the local room. An invite for such a room is refused at the preview:
  preview terms stay raw, and the room keeps working by pulls.
- **Where the rule lives.** In the joiner, not in `core`: `core` cannot
  use the display tables of `node`, and a create entry is parsed again
  on every pull of a room already held, where a new rule would mark it
  invalid. Only a join starts a room from another machine's create entry.
  `PinRoom` does not run the rule; tests use it to place a room as an
  older version stored it.
- **No wire, entry or schema change.** Stored labels and entries are not
  checked or changed. A database written by 0.2.0 to 0.2.3 opens, rooms
  labelled `local` included, and 0.2.3 opens one this version wrote.
  Older admin machines send the label they hold, which is the create
  entry's, so honest joins are unchanged.
- **Costs.** A machine that holds a room and does not know the admin
  that invites it, whose own admission comes after 1,024 entries (or
  4 MiB) of its log that this machine lacks, cannot join through it: it
  pulls from a member first, or leaves and joins again. While pages
  wait, they are held in memory and checked again together under the
  rooms lock; the limits bound that work. A room an honest older machine labelled `local`, or
  gave a label with such a character, cannot be joined by this version;
  rooms cannot be renamed, so it must be made again under another label.
- **Still open.** Lookalike letters (`lоcal` with a Cyrillic `о`) are not
  detected. A room created here under the label `local` is only shown
  with its id; refusing it at `room create` and a `local: true` flag in
  results belong to the surface's wording (group G). The server's own
  preview still sends the label it holds raw; the joiner checks it. A
  pull's answer still writes a non-member's log entries before it checks
  membership (`Exchange.apply`); the fix belongs with the pull code. A
  hostile admin machine can still send pages that move on without end;
  only the room's own bounds limit them. Joins between this version and
  0.2.0 to 0.2.3 machines, and 0.2.3 reopening a database this version
  wrote, are argued from the unchanged formats, not run in a test.

**Revisit if** rooms gain a rename, a second built-in label is reserved,
or a joiner reports a refused join of a room it holds.

## D65. What an agent reads says who wrote it and which room it is (2026-11-25)

A security review and a council of models found that an agent could not
tell this machine's mail from another machine's. MCP entries left out the
author's machine key and gave 8 hex characters to compare. `ask_human`
said the answer comes "from human", while in a shared room any member
machine can post as `human@<its id>`. Short ids are 32 bits, and nothing
marked two machines that share one. A room created elsewhere could be
labelled `local`, the room the `send` tool calls "this machine only"
(F12, F55). An agent could register as `Human`, and `a2p2 mcp --harness
human` as the person (F42). A member could take this machine's name or a
colleague's (F44). Rooms shared between owners make each of these a way
to pass another owner's text off as the local person's.

- **What changed.** All of it is display, in `node.Surface`, the local
  API and the shim; nothing stored or accepted changes.
  - Every entry carries `Here`, set by comparing the author's full key
    with this machine's. The local API writes it as `from_this_machine`
    on entries and `unread` first lines; MCP results write it on every
    message (inbox, thread, recent, skipped, peek), never left out.
  - MCP results add `person` to a message labelled `human` in any case:
    "yours: the person on this machine" only for exactly `human` written
    here, "another machine's person, not yours" otherwise (or "not
    yours: a look-alike label on this machine").
  - `ask_human` returns `answer_from`, the person's id by this
    machine's full key, so it stays the same when a machine with the
    same short id joins or leaves. Its note says the answer is the reply
    whose `parent` is the question, with `from_this_machine` true and
    `person` yours. A `human` addressee is this machine's person by its
    full key. `person` says where and under which label a message was
    written: any program run by the same OS user can send as `human`
    through the local API, so it does not prove who typed it.
  - The surface compares the short ids of this machine, every member of
    every room held and the authors and addressees about to be shown. A
    key whose short id another has is written in full in `from`, `to`,
    writers and status's `human`, and marked `id_shared` (entries,
    members, peers, status, and in `peers` and `whoami`). An addressee
    whose short id names two members is refused with a sentence that
    says so. An agent id may name this machine by the full key, as
    status gives `human` then. `unread` and `peek` lines, `room/list`
    members, `fetch` authors and holders, writers and local agents in
    `peers` carry the same marks; writer ids are never cut. One snapshot
    of the memberships serves a whole `unread`. The ids a `send` returns
    are drawn after it commits and never turn it into an error.
  - Agent-facing labels (`node.ShownLabels`: entries, send and reply
    results, levels, unread, and the shim's whoami and peers) show only
    the local room as `local`. Another room whose label reads as `local`,
    two rooms that read alike (`LabelKey`), or a label that reads as
    another room's id get `#` and the first 8 hex of the id, then `#` and
    the whole id, then the bare id, each step repeated until no two rooms
    read alike. A room argument may be the id, the shown label (escaped
    or not), the stored label or the stored label with `#` and 8 or more
    hex of the id; a name that could mean two rooms is refused, so a
    label shown before a room arrived never picks the new room. `local`
    always names the local room.
    `room create` refuses `local` in any case. Room lists and status keep
    the stored label: the page and the CLI draw rooms that read alike
    their own way (D58). MCP room references gain `local: true` for the
    local room only.
  - The `human` label is reserved in any case; only harness `human` with
    label `human` registers it. `a2p2 mcp` refuses a harness named
    `human` in any case.
  - In `peers`, a machine name that another machine in the result also
    uses, or that reads as another machine's id, is shown as
    `<name>#<machine>`, this machine's included, and this machine is
    marked `this_machine`. Names are compared as drawn, so a machine with
    no name counts under its id.
  - The person's questions exclude the person's own posts by the full key,
    not by comparing the short id.
- **No wire, entry, record or schema change.** Old machines see the same
  bytes. A database written by 0.2.0 to 0.2.3 opens as before, and one
  this version used opens in 0.2.3, since nothing new is stored. The
  local API and MCP results gain fields; old clients ignore them. The
  seven tool definitions are unchanged (3,991 bytes).
- **Costs.** Each message in a tool result is about 26 bytes longer
  (`"from_this_machine":false,`), and a `human` one about 50 more. Every
  read that returns entries, and status, reads the room list once more
  and walks every room's member list in memory. An agent registered as
  `Human` before this version can no longer register under that label.
  A room that reads like another is addressed by its shown label or id;
  a name that could mean two rooms is refused where it used to pick one.
- **Still open.**
  - Refusing to admit a key whose short id matches this machine's or a
    current member's belongs to the join and membership code (group D:
    `node/joiner.go`, `node/invite.go`, and the request text of
    `node/requests.go`): the admin's request should say the short id is
    taken and the join should be refused there, applied only to new
    requests. Until then such a machine is shown in full, not refused.
    The approval side needs, in group D's files: the join, add and admin
    requests show that the key's short id is already held by this
    machine or a member, and the check runs again under the approval
    lock before signing. It applies to new requests only (D57: a stored
    entry still replays), with a test that a database holding such a
    member still opens with the room working. No scheduler or pull
    change is needed.
  - The page and the CLI did not show `from_this_machine` or `person`
    (groups E and F). Since the merge of this wave they mark, beside
    the author, a message this machine wrote and one from `human` on
    another machine as another machine's person, and print or draw
    `edited` (D62). The default mailbox label is still the
    directory's base name (F39). The CLI acts as the person by default
    (F11, group F).
  - Per-message room ids are not added to tool results: labels in one
    result are unique, and its `rooms` list gives each label's id
    (`TestInboxEntriesAreCompact` holds entries small).
  - A name taken by a machine that is not yet recorded in any room this
    machine holds is not detected. Lookalike letters are not detected.

**Revisit if** a later release adds a receipt rule for room labels or
names, the short id is lengthened, or a harness shows that models still
take another machine's `human` for their person.

## D66. The CLI takes the person's identity only where a person is likely (2026-11-25)

A security review found that the CLI acted as `human@<own id>` whenever
`--as` was left out, and that `--as ""` counted as left out (F11). That
id is the one other members and agents, other owners' included, are told
is this machine's person. So any agent with a shell, or a Pi skill whose
`$ME` was empty in a fresh shell, read as the person, which took the
person's questions off their list, and could answer one as the person
when a room message told it to. Rooms now span owners, so this is fixed
now with the rest of the review. `room leave` (F38), the UI token in
`status --json` (F41) and the advertised addresses (F51) are in the same
pass.

- **What changed.** An explicitly empty `--as` is a usage error, as is
  `--as` with `--person`. Without either, in a shell whose environment
  shows a harness's agent (`CLAUDECODE` and `AI_AGENT` seen in a Claude
  Code 2.1.332 shell; `CODEX_THREAD_ID`, `CODEX_SANDBOX` and
  `GEMINI_CLI` read from those binaries, and only simulated in the
  tests), the CLI registers as that harness with the working directory,
  so it gets `<harness>-<directory>`, the id the MCP shim gives the
  agent when it runs in the same directory under the default label (not
  when `A2P2_NAME` or `--name` sets another), and says so on stderr. Otherwise `send`, `ack`, `inbox` when it
  marks entries seen and `inbox --level` act as the person only with
  stdin a terminal. `a2p2 ui`, whose address carries the page's token
  and so the person's identity on the page, is refused in an agent's
  shell and away from a terminal. `--person` overrides each refusal.
  Used away from a terminal or in an agent's shell, the CLI then sends
  `unattended` with `register` (one optional field of the local API,
  the command's name), which the daemon logs as a warning; at a
  terminal outside an agent's shell nothing is logged.
  Reads that mark nothing (`--peek`, `--thread`, `history`, `fetch`)
  are unchanged. `room leave` asks at a terminal for a typed `yes`,
  shows the room's id, isolates a right-to-left label as approvals do,
  says when this machine is the room's only member or only admin, and
  sends the daemon the id shown, never the label typed. A room set aside
  is found by its id or label in `status`; a room it cannot look up is
  not left. `status --json` prints the daemon's compact answer without
  its `ui` member, the other members in order with their bytes. `net
  on` says that every member and invite holder gets the advertised
  addresses, and lists `config.json`'s `advertise` when it is set. The
  demo's file step now sends and fetches as its agents.
- **Costs.** A script that sent, acked or read as the person without a
  terminal stops working until it adds `--person` or `--as`. A person
  who runs `a2p2` inside an agent's shell (`!` in Claude Code) acts as
  that agent unless they add `--person`. The stderr note adds a line to
  every agent call. An older daemon still running after an upgrade
  ignores `unattended`, so it logs nothing.
- **Still open.** None of this is a boundary: a program running as the
  user can pass `--person`, unset the variables, drive a terminal or
  call the socket. The variable list covers the harnesses named; OpenCode,
  Antigravity CLI, Hermes and Pi set nothing a2p2 knows, so their agents
  are caught only by the terminal check (Pi's skill passes `--as`). The
  Codex and Gemini variables were read from the binaries, not seen in a
  run. The log line is a record for a person who looks, not an alert.
  `net on` does not yet list the detected IPv4 addresses it will
  advertise: that needs `daemon.FamiliesText` to name them
  (daemon/netaddrs.go, another group's file). The UI token is still
  returned by the socket's `status` to any caller; the one-time code of
  F14 replaces it. `room leave` sends the id it showed, but if the room
  is left and joined again under the same id while the prompt waits, the
  new copy is the one deleted: refusing that needs the generation shown
  to be checked under the leave lock in `internal/node`, outside this
  change.

**After the second review** (two reviewers of another model family):

- `--person` away from a terminal was recorded only in the rotating
  daemon log. The daemon now also counts it in memory, and `status`
  carries `unattended` (`count`, `last`, `command`; one optional field
  of the local API, absent when there were none and from older
  daemons), which `status` and `doctor` print. It is needed now because
  the log line alone is seldom read; it is not stored, so the count
  starts again at each daemon start, and no schema changes.
- A person who piped text in at a terminal (`echo hi | a2p2 send`) was
  refused, since only stdin was checked. A person is now taken to be
  there when stdin or stderr is a terminal; a harness's tool captures
  both.
- In a recognised agent's shell the CLI used the default label, another
  mailbox than the agent's tools when their label was set. It now uses
  `A2P2_NAME` when the shell has it, and its stderr line, printed at each
  such call, says which id it used and that `--as` picks another. A label
  set only in the harness's settings is not visible to the shell, so that
  case still needs `--as`.
- `a2p2 ui --json` printed the daemon's whole status, the address after
  it, and opened the browser; `ui` now refuses `--json`. `--print` still
  needs a terminal or `--person`, since whoever reads a printed address
  can trade its code as a browser would: printing it is acting as the
  person. Over SSH, `ssh -t` gives the command a terminal; with
  `SSH_CONNECTION` set, `ui` opens nothing on the remote computer (whose
  browser would use the code up) and names the port to forward. A
  browser that cannot be started is now said, after the address.
- Declined: `room leave`, `confirm`, `net`, `join` and `room files` do
  not also refuse in a recognised agent's shell. They ask for a typed
  answer at a terminal, which an agent's tool does not have, and a
  person who runs them with `!` in Claude Code would be refused.
- Still open: an agent whose harness sets none of the variables is
  taken for the person, and acts as the person if its shell has a
  terminal (SECURITY.md says so). Reads that mark nothing still read the
  person's mail without a terminal.

**Revisit if** a harness documents a variable for its agents' shells, an
OS-separated approver arrives, or F14 changes how `a2p2 ui` gets its
address.

## D67. A one-time code opens the page, the daemon log is rotated, and a home under a shared directory is refused (2026-11-25)

The security review of 2026-11-25 found that the page's address carried
its token for the daemon's whole run (F14). `a2p2 ui` hands that address
to `xdg-open`, and a browser it starts keeps it in its command line,
which other users of a Linux machine can read; every OS user can reach a
loopback port, so such a user could read every room and decide requests
on the page. It also found that `daemon.log` was never rotated while
another machine can make the daemon write to it (F19), that a daemon
whose page port was taken said nothing while a tab left open from an
earlier run would draw whatever the new listener answered (F47), and that
the home was checked only at its own path (F53). Why now: rooms between
owners and shared machines are the use the project is built for.

What changed:

- **Page sign-in.** No new local API call: `status`'s `ui` is the page's
  address with a new one-time code (`#c=`) at each call, good once and
  for 30 seconds. The page trades it with `POST /api/session` (a new
  page route, exact `Origin`, JSON) for a session kept in the tab's
  `sessionStorage`. Sessions are in memory for the daemon process, across
  networking decisions, at most 32; untraded codes at most 64, oldest
  first. Both are looked up by their SHA-256. A traded code is kept apart
  from the untraded ones for as long as its session lives, so a code
  traded twice ends the first trade's session and logs a warning however
  late the second trade comes, and a race won by another user is seen,
  not silent. Once that session has ended (by a second trade, or pushed
  out by 32 newer ones) a further trade is refused as unknown. The
  long-lived page token is gone. A 0.2.3 CLI opens the new address and
  works. The new CLI with a 0.2.3 daemon still running gets the old
  address: `a2p2 ui` does not open it and says to run `a2p2 stop`;
  `--print` prints it with the same advice on standard error. The page's
  server also has a 30 second limit on reading a whole request.
- **Run check.** `/api/state` carries `run`, a random value the daemon
  keeps with the presenting session and gives with it. Each session has
  its own, not derived from the token (a listener on the port receives
  the token), so a user who traded a code, their own or the person's
  first, learns no run of the person's sessions. The page signs itself
  out at a state with another run. After any state that fails (a 503 between runs, a refused
  connection) it makes no call but the state until a state names its run
  again, and before each POST and each download it reads a fresh state.
  So what the person types, and a decision, reach another user's listener
  only if it took the port between that check and the request, one
  loopback round trip. The run is no secret from the daemon's own user,
  and the page still shows what it drew before the daemon stopped.
- **Port taken.** The fallback to another port is logged at Warn.
- **Log.** The running daemon, which holds the home's lock, is the only
  one that rotates `daemon.log`: at start and every 10 seconds, once it is
  over 10 MiB, its last 10 MiB are copied to `daemon.log.1` and the file
  is cut, since the daemon writes it through an inherited handle opened
  for appending. If the copy fails (a full disk) the file is cut anyway.
  Lines written during the copy may be lost.
- **Home.** On Unix, `CheckHome` (the daemon and every client) refuses a
  home under a directory owned by another user than this one or root,
  or one others can write without the sticky bit, or one its group can
  write unless the group is this user's primary group and named as the
  user (a private group). The path is walked one name at a time without
  following links; a link must be this user's or root's, and its target
  is walked from the directory that holds it, so every directory a lookup
  passes through is checked, in a chain of links too. The path is walked
  as given, not cleaned, so a `..` after a link goes where the kernel
  goes.

What it costs: an address printed with `--print` must be opened within
30 seconds (stderr says so); opening one link in two tabs signs the first
out; a home under a group-writable directory of another group now fails
to start, with the directory named, and on macOS that includes one
writable by `staff`, the primary group of every account. A user of the
same machine who reads the code and trades it before the browser does
still gets a session until the browser's trade ends it, and keeps it if
the browser never trades the code.

Still open: a client does not check which user serves the socket it
dials (getpeereid, `SO_PEERCRED`); that needs a dial hook in
`internal/localapi`. The home is checked when a client connects, not at
every dial. The MCP shim does not warn when the daemon runs another
version than it does (F72); `status` carries the daemon's version, and
only `a2p2 ui` checks it so far. On Windows the directories above the
home are not checked.

**After the second review** (two reviewers of another model family):

- **Home.** A stock macOS account whose home sat under a directory its
  `staff` group could write, or a Linux account whose group could not be
  named (a static binary and a directory-service account), was refused,
  so a database left by 0.2.3 there could not be reached. Now a
  directory above the home is refused only where another user can
  replace the home: another owner (root aside), others' write without
  the sticky bit, or write by a group this user is not in. Write by a
  group this user is in is accepted with a warning (the daemon's log at
  start, `status`, and a `warn` line in `doctor`, which does not fail),
  since a2p2 cannot tell whether anyone else is in the group; this
  user's own private group gives none. A test starts a daemon on an
  existing home with messages under such a directory, then refuses it
  under a 0777 one and finds the same messages after the `chmod`.
  After the third review, a directory counts only if another user can
  reach it: under a directory of this user's or root's with no search
  bit for others (`~/Documents` on macOS is 0700), a 0777 directory is
  neither refused nor warned about, since 0.2.3 did not check the
  directories above the home and a database there would otherwise be
  cut off. Each class is followed on its own (a group's members by the
  group bit of that group's directories, everyone else by the other
  bit); ACLs are not read.
- **Page.** A code is single-use already: a second trade is refused and
  ends the first's session, and re-running `a2p2 ui` gives a new code
  and leaves earlier sessions alone. On Linux the daemon now finds the
  user of the client socket in `/proc/net/tcp` and refuses a trade from
  another user before looking at the code, so that user can neither use
  nor spend it. After the third review the socket is found by its whole
  four-tuple (both addresses and both ports, IPv4, IPv6 and IPv4-mapped
  IPv6 forms, in `tcp` and `tcp6`): matched by ports alone, another user
  could hold a socket on 127.0.0.2 with the same two ports as the
  person's browser (loopback is all of 127.0.0.0/8) and be taken for the
  person, or make the person be taken for them. The user counts as known
  only when exactly one connected socket (TIME_WAIT and LISTEN lines
  aside) matches; a table that cannot be read (a missing `tcp6` aside),
  a line that does not parse, or two matches make it unknown, and no
  match is read again twice, since a table read in chunks while sockets
  change can miss a line. Root is not refused, as it can read the home
  anyway. **Unknown allows the trade**, as before the check existed: the
  check must never lock the person out of their own page, and where it
  cannot be made (macOS and Windows have no such table, and a sound
  lookup there would need a new dependency or a privileged call; a
  container or a cut-down `/proc` on Linux) there is nothing to tell an
  honest browser from another user's. So the check is defence in depth
  on Linux only. The race between accept and the read is harmless: the
  client's socket lives while its connection carries the request, a
  closed one cannot receive the session, and a four-tuple in use cannot
  be taken by a second socket. `/proc/net` is the daemon's own
  (`/proc/self/net`), which hidepid does not hide. The uid comes from the
  kernel's table, never from the request. A session is not tied to one connection, since a
  browser opens several. Every answer to a session carries its run in an
  `A2P2-Run` header, so a listener that took the port between two states
  can no longer have a history, thread or file drawn; the page signs out
  and also forgets its session in the tab's storage. 1,024 untraded
  codes are kept instead of 64, since every `status` call (the CLI's
  connect, the MCP shim) makes one. With a daemon before 0.2.4 running,
  `ui --print` no longer prints its long-lived token address either.
- **Still open.** Where the user at the other end is not known (macOS,
  Windows, and on Linux a `/proc` that cannot be read or a lookup with
  no single match), a local user who can read another process's
  arguments during the code's 30 seconds, and trades it before the
  browser does, holds a session until the browser's trade ends it, or
  keeps it if the browser never trades. On macOS and Windows another
  user cannot normally read the arguments of the person's processes; on
  Linux they can unless `/proc` is mounted with hidepid, and there the
  check refuses them whenever the lookup succeeds, which is the normal
  case. A duplicated tab
  shares the session, as browsers copy the tab's storage. `status` still
  makes a code for any caller of the socket.

**Revisit if** a browser can be handed an address without a command
line on Linux, or the local API gains a call that makes codes only for
`a2p2 ui`.

## D68. Port mapping checks what a host answering as the router says (2026-11-25)

A security review (F61 to F66) showed that a device on the same network
as a machine that ran `net on --map-port` could answer as the router. One
datagram that holds nothing of the request made a2p2 skip PCP and forget
the PCP mapping it may have made. A lifetime of 136 years stopped renewal
for the run. A multicast external address was saved, and then no later
start mapped or removed anything. A private address went first in every
invite. A UPnP listener on port 65534 or 65535 could ask for port 0 and
leave the same unusable record. Rooms now span owners, so a wrong first
address in an invite reaches other owners' joiners.

- **What changed.** NAT-PMP's version answer to a PCP request counts only
  if no PCP answer with the nonce comes in any of the tries, and the PCP
  request stays recorded, to be deleted later; a PCP deletion that gets
  only the version answer in all its tries counts as done. A new PCP
  request with no answer at all is sent one deletion, not waited for,
  before its handle is let go; a renewal with no answer is not, since its handle is
  the mapping held. A lifetime longer than asked counts as asked, in the
  client and again in the keeper. A grant whose external address is not
  a public IPv4 address (private, 100.64.0.0/10, loopback, link-local,
  multicast, 0.0.0.0/8, 240.0.0.0/4, or any IPv6 address, since every
  request is IPv4 and IPv6 can encode a private IPv4 address) is not
  taken: status says why, and what it may have made is removed after the
  attempt. The address given to the reacher and to invites is checked
  again, is withdrawn when the keeper's run ends (listener stopped, `net
  off`, stop) whether or not the router confirmed the removal, and is
  withdrawn when a renewal is answered with an address that cannot be
  used; the next renewal is then five minutes later, so a router that
  answers at once is not asked every second. After any failed attempt the keeper removes what it holds but the
  live mapping, so failures do not pile up. A keeper adds at most 8
  mappings to remove (a record still loads with up to 16) and never sends
  a request whose handle the record could not load, so the record always
  loads. UPnP never asks for a port past 65535. Only a default gateway
  with a private IPv4 address is asked for a mapping; one an earlier
  version asked (a public or shared address on the local network) still
  gets the deletions of what the record holds for it. The `net on
  --map-port` text says a router may keep a mapping past 2 hours after a
  crash and that its answers are not authenticated. The daemon's port
  mapping code moved to `internal/daemon/mapport.go`, so removing the
  feature is deleting that file and `internal/daemon/portmap`, and fixing
  a few calls in `daemon.go`, `RunOpts` and `defaultGateway`.
- **What did not change.** Port mapping was already off unless a person
  asks for it: the CLI (`net on --map-port`) asks at a terminal each
  time, and the daemon takes it only from the local API's net call. Any
  later `net on` without it removes the mapping; a recorded decision
  with it still applies after a restart. No wire, record, schema or `portmap.json`
  format change: a record written by 0.2.0 to 0.2.3 loads, and one written
  now loads in them. Documentation addresses still pass the public check;
  nobody holds them and the tests use them.
- **Costs.** A router that speaks only NAT-PMP is now taken as one after
  every PCP try (1.75 s) instead of at once, on each mapping, renewal and
  PCP deletion. A router that does not answer PCP gets one more datagram,
  a deletion, per attempt. A router that reports an IPv6 external
  address gets no mapping. A router behind another NAT whose external address is private
  or shared now gets no mapping where it got a useless one. A gateway with
  a public or shared address is never asked. A failed attempt sends a
  deletion for what it may have made.
- **Still open.** A device that relays a public port it mapped for itself
  to this machine's listener passes the dial-back, and the address is then
  advertised to every member, and so does a wrong address that one
  member falsely says it reached; detecting either needs the listener to tell
  which inbound connection was a member's dial-back and from what source,
  in `internal/peer` and `internal/node` (`dialback.go`, `reach.go`). The
  unconfirmed mapped address still goes first in invites, because a room's
  first invite comes before any member can confirm it; the owner chooses
  between that, putting it last, and leaving it out until confirmed. A
  forged NAT-PMP refusal still makes a2p2 forget a NAT-PMP mapping (it
  lapses with the router's lease). A version answer forged on every try
  of a PCP deletion, with every real answer lost, still lets a2p2 forget
  a PCP mapping, and a PCP request with every reply and the deletion lost
  is forgotten too; each lapses with the router's lease. Keeping them
  instead would fill the keeper on a network without PCP. A permanent UPnP lease, asked for only
  on error 725, is still unbounded after a crash. The SSDP answer's source
  port is not checked (F65): the UPnP architecture does not fix it, and a
  device that can spoof the gateway can do more by ARP.

**Revisit if** the owner decides on invites or permanent leases, a
dial-back can tell a relayed connection, or port mapping is removed.

## D69. The team launcher keeps its state from other users and its options from manifest values (2026-11-25)

A security review found three gaps in `scripts/team` (F74 to F76). The
state directory was checked only by its owner, so another user who made
a directory above it, such as `/tmp/a2p2-team-example` from the shipped
example, could rename it and put their own `launch.json` in its place
before the supervisor read it. The `allowed_tools` check let through
`Read,Bash` and `Bash(sh:*)`, which give unscoped shell access. And
`team status` and `team logs` printed C1 controls and bidi overrides.
Rooms now span owners, so text a member copies from a peer reaches the
operator's terminal.

- **What changed.** `up` refuses a `state_dir` or `a2p2_home` on a path
  where a directory, symlinks followed, belongs to another user than this
  one or root, or lets group or others write without the sticky bit, or
  where an entry in such a sticky directory belongs to another user.
  Group write does not count when the group is this user's alone: its
  primary group, with no other member and no other account whose
  primary group it is, as Linux systems with the umask 002 give. It
  refuses an existing state directory that group or others may write
  instead of making it 0700, since files may have been planted. It makes
  each missing directory 0700 whatever the umask, and checks the home's
  path again after making it, before the first `a2p2` command. `status`,
  `stop`, `logs`, `report` and the supervisor refuse such a state
  directory too, before they read or write in it, and the reports and the
  roster are written through a new file renamed into place, never through
  a symlink. The supervisor runs from the copy of the manifest `up`
  checked and saved as `manifest.json` in the state directory, not from
  the manifest file. Each `allowed_tools` entry must be one tool with at
  most one rule in parentheses, matched whole, with no `,`, no space
  outside them and no control, format character or space other than
  U+0020; a `Bash` rule whose command, compared without case, is a shell,
  `env`, `xargs`, `sudo`, an interpreter, `find`, `make`, a wildcard or
  quote, or `git` with no subcommand or a wildcard in its place, is
  refused. A model, or a headless member's prompt, that starts with `-` is
  refused; the prelude is read once, and each prompt is checked again with
  the real ids as it is saved for launch. The kickoff and notices go to
  `a2p2 send` after `--`. `team status` and `team logs` escape by the
  daemon's rules, and `logs` escapes each log's path, reads logs line by
  line and `-n` from the end. `team logs --raw` (a new option of the
  launcher, not of a2p2) prints the bytes.
- **The daemon's helper, ported.** The launcher is Python with no
  dependency and cannot call `internal/node/escape.go` (D58). It holds a
  port of `escapeText` with the same tables and text rules, as `app.js`
  does: a test checks that its tables equal the Go ones and that it gives
  `EscapeDisplay` and `EscapeDisplayText` output for every row of
  `internal/node/testdata/escape_cases.json`. Status shows one line, so it
  turns line breaks and tabs into spaces first; logs keep lines.
- **Costs.** A state directory or home under a group-writable directory
  that worked before is refused when another user is in the group or has
  it as primary group, as `staff` on macOS; the error says to run
  `chmod g-w`. Accounts that `pwd` cannot list, as some directory
  services give, are not seen when the group is judged private.
  An existing state directory from an earlier run that is now refused can
  no longer be read or stopped with `team`; its supervisor, if alive,
  still ends with its members. An honest `Bash(git:*)`, `Bash(git *)` or
  `Bash(make:*)` is refused; name the subcommands.
- **Still open.** Each member's prompt is an argument on its harness's
  command line, so `ps` shows it to every local user. Passing it on
  stdin (`claude -p` and `codex exec -` read it) changes how each harness
  is started and needs a real run of each first (AGENTS.md). Without
  `-m` the launcher reads `./team.json`, and `status` and `report` run
  the `a2p2` it names; the README now says a manifest is code, and
  dropping the default is the owner's choice. The `allowed_tools` deny
  list cannot know every program that runs another. A rule such as
  `Bash(go test:*)` runs code a member that may edit files can write; the
  README says so, and a key to consent to such rules was not added, since
  naming the rule in the manifest already is that choice. How Claude Code
  parses `--allowedTools` is checked against its documentation, not a
  real run.

**Revisit if** a harness is started with its prompt on stdin, Claude Code
changes how it splits `--allowedTools` or matches a rule, the daemon's
escape rules change (the parity test fails), or the launcher gains a way
to call the daemon's escape.
