# Decision log

Append-only. One entry per decision: what, why, and what would change our mind.
Supersede an entry by adding a new one that names it; do not rewrite history.

## D1. Build a small new core; use existing projects as reference only (2026-11-22)

Three independent static audits (Codex on open-agent-bridge and
agents-connector, Antigravity on agentmesh, Claude on agentmesh) reached the
same verdict. No candidate has authenticated rooms or paired cross-machine
transport; the strongest local experience (open-agent-bridge, ~22k lines of
TypeScript) depends on private harness sockets and has client-side-only room
filtering; all have under ten GitHub stars and a single maintainer. Scores and
evidence: `research/01-…`, `research/02-…`.
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
(`research/03-…`). Waking idle sessions is harness-specific and partly
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

Supersedes the pid-matching idea in `research/03`. Label defaults to
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
the first stored row. Full review and dispositions: `research/05-code-review.md`.

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
embed a daemon, and one chose MIT for brevity
(`research/09-deliberation-on-owner-decisions.md`, Q8). Contributions are
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
existing system to adopt (`research/08-synthesis-and-recommendation.md`). The
v0.2 push-from-the-author delivery (D3, D13) will be replaced, not extended:
nothing has been released, so there is nothing to stay compatible with. Order
of work is in `REQUIREMENTS.md` section 9: executable scenarios first, then
the design, then the build.

## D26. Rules are settled in executable models before the design (2026-11-23)

Hand-written rules for several admins were shown unsound twice by reviewers,
so step 0 of the plan became two pure-Go models with exhaustive small
scenarios (`research/prior-art/07-requirements-and-verification.md`):
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

