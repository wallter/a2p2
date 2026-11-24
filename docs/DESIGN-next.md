# Design for the next version (draft, not built)

**Status: draft for review, revision 2, 2026-11-23; nothing here is built.**
Revised after `docs/research/decisions/design-next-review-sol.md`.
`docs/DESIGN.md` describes the code as it is (v0.2). This file describes what
stage 1 of `docs/REQUIREMENTS.md` section 9 will build. It replaces
`DESIGN.md` when that code lands.

Notation: **[M4]** is rule 4 in `internal/model/members/FINDINGS.md`,
**[R7]** is rule 7 in `internal/model/delivery/FINDINGS.md`. Requirements are
cited by id (D5, T9). **Not modelled** marks a design choice no model test
covers; reviewers should attack those first. **Being modelled** marks a rule
the models are being extended to check; it is not settled until they pass.

## 1. Summary

What changes from v0.2: records are signed and verify via any relay (D8);
members pull from any member, and the outbox goes (D2); each daemon start
writes a new stream [R2]; the member list is a signed log with several
admins [M1–M12]; text expires after 30 days, leaving a signed stub [R6];
withdraw, replace, handled and replies are records [R7–R9]; reading walks
per-agent cursors [R7]; invites are short and confirmed by a person (T3,
P3); a machine that cannot accept connections opens every exchange (N1).

Unchanged: one binary and daemon per machine, `A2P2_HOME`, the local socket
and its trust boundary, the Ed25519 key and pinned mutual TLS 1.3, HTTP+JSON,
the stateless MCP shim with CLI `--json` and socket parity (A1), peer text
labelled as data, never instructions (T8), harness install and hooks, the
loopback web UI, no bodies in logs, and the `local` room (one member).

## 2. Records

A record is a signed header plus an optional body [R1]. Four kinds: `message`,
`handled`, `withdraw`, `replace` (delivery FINDINGS, "Record"). The v0.2
kinds `question` and `answer` go: `ask_human` sends a message addressed to
the agent `human` on this machine, and the answer is a reply from `human`.

| Field | Meaning |
|---|---|
| `v`, `room`, `author` | 2; room id; author node id |
| `epoch`, `seq`, `prev` | random 64-bit stream id per daemon start [R2]; position from 1; id of `seq - 1` |
| `links` | seq 1 only: heads of the author's history held, `{id, epoch, seq}` [R2] |
| `version` | `replace` only: one above the text it replaces [R7] |
| `kind`, `agent`, `to` | kind; agent label; addressees `{node, agent}`, empty for everyone |
| `ask_handled`, `ref` | room-wide post asks for reports [R9]; message a non-message concerns |
| `parent`, `thread` | message answered; thread's first message (D10) |
| `created`, `body`, `size` | author's clock (RFC 3339 UTC); SHA-256 of the body; its length |

**Not modelled: encoding.** The author encodes the header once as compact
JSON; the Ed25519 signature covers those bytes exactly as sent. The id is
the first 16 bytes of SHA-256 over `"a2p2 record v2\x00"` followed by them,
as 32 hex. Wire form: `{"h": base64(header), "s": base64(sig), "b":
base64(body)}`, `b` omitted when no text is sent. Headers are kept byte for
byte. Refused: a duplicate JSON key, an unknown `v`, a body whose length or
hash does not match, a `ref`, `parent` or `thread` in another room. Unknown
fields are kept and ignored. The body is JSON `{"text", "choices"}`. Full
node ids in `to` stop a key ground to an 8-hex prefix claiming another
agent's handled rights.

Limits: body 16 KiB, header 4 KiB, 32 addressees, 8 links, labels 64 bytes,
64 member machines per room. **Not modelled:** with more than eight heads, a
new stream links the eight newest by `created`.

```json
{"v":2,"room":"r_9f2c4e1a0b7d4c3e8a5f6b2d1c0e9f8a",
 "author":"ab12cd34…(64 hex)","epoch":"5b1e0c9a7f3d2e41","seq":3,
 "prev":"0c6f…(32 hex)","kind":"message","agent":"claude-api",
 "to":[{"node":"cd34ef56…(64 hex)","agent":"codex-web"}],
 "parent":"7a91…","thread":"7a91…","created":"2026-11-23T19:00:00Z",
 "body":"e3b0…(64 hex)","size":58}
```

A receiver stores a record only if it verifies, is the next position of its
stream with matching `prev`, and (seq 1) its links are held [R4]; a second
record at a held position is a fork and is refused. A withdraw or replace
must share `ref`'s author machine; any agent there may issue it (T2).

**Being modelled (delivery, membership changes): who may author.** A machine
stores a record if its author is, or has been, a member according to the log
as this machine resolves it, and takes records only from peers that are
members now. Records by an author who is no longer a member are shown marked
"former member". If an author's admission turns out never to have counted,
its records are hidden and dropped. So a copy held only by others, or a late
withdraw, survives its author's removal; a member willing to relay for a
removed machine can still pass on what it writes, marked.

**Expiry** follows [R6]: text expires 30 days after `created`, by the
holder's clock, recorded and never undone. **Not modelled:** a record whose
`created` is more than 24 hours ahead of the receiver's clock is not
accepted yet; by [R4] its stream waits there until the clock catches up.

## 3. Room log

An entry has `v`, `room`, `author`, `seq`, `prev`, `parents` (ids of the
entries the author had seen), `kind` (`create`, `add`, `remove`, `admin`),
`target`, and for `create` the room label [M12]. An `add`, or an `admin`
naming a machine that is not a member, also carries `history` (default
false) and `from`: the admitter's heads, per author, of the records it held
when signing. Entries are encoded, signed, identified (domain
`"a2p2 entry v2\x00"`), stored byte for byte and refused exactly as headers
are in section 2.

**Not modelled: `from`.** The model admits from per-stream positions [R10];
with gap-free prefixes closed under links (Q8) those are the records at or
before the admitter's per-author heads. A record whose relation to `from`
cannot be decided is treated as before it. When effective grants differ on
`history`, the more restrictive wins.

Entries go through `members.Machine` [M11]: held until their parents are
present; a second entry by one key at one position is refused with its
dependants and kept as evidence until the forking key is removed.
`members.Resolve` gives members, admins and a reason for every entry
without effect (A4) by [M3] to [M10], with no timers [M12, T10]. `admin`
also grants membership; removing an admin removes the machine, and a later
add brings it back [M8]. Entries are passed on in causal order, evidence
after them. The `members` table is a cache derived from the log.

**Being modelled (members, fork evidence).** Evidence that a key signed two
entries at one position counts as that key's vote for its own removal, so
one other admin can settle a fork. This would close members gap 1.

**Not modelled: pull before signing.** A daemon signs no log entry for a
room until, since it started, it has pulled that room's log from another
member. Creating a room and rooms where this machine is the only member are
exempt. This narrows the restore problem; it cannot detect a restore.

`a2p2 room list` and the page show members, admins, changes waiting for a
second admin [M4], entries without effect with reasons, and fork evidence;
an unsettled fork is an alert (A3).

## 4. Pull

One endpoint, `POST /p/v2/pull`, over the existing pinned mutual TLS, per
room. It is the model's Pull [R3, R5], carried both ways in one round trip.

| Request | Response |
|---|---|
| `room`; full `summary` [R3]; `log`: the requester's log heads and entries the server seems to lack; `offer`: records for the server; `addrs` (hints); `wait_ms` up to 240,000 | `log` entries the requester lacks; `page` [R3]; the server's full `summary`; `peers`: other members' addresses (hints); `more` if cut at a limit |

The server applies the request's log entries first (from a non-member, only
if they make it a member), so a newcomer presenting its own `add` is served
at once (T4); it then requires the TLS node id to be a member, else 403
(T5), and stores the `offer` under [R4]. A page puts wanted text first, then
later records with the text the requester may read [R3, R10], spending no
slot on what it cannot store (Q12). With `wait_ms` the server holds the
response until it has something new. **Not modelled:** membership is checked
again before each page is built and before a held response is released.

**Being modelled (delivery, offers).** A machine builds an offer only for a
peer it resolves as a member at that moment, honouring its history
permission and its last summary, so no slot goes on what that summary shows
it does not want. Offers are how an N1 machine passes on what it writes. A
machine that has not heard of a removal can still hand text to the removed
machine: the existing "Removal is not instant" limit.

The requester applies log entries, then the page [R4]. Each side keeps the
other's summary as a dated observation, the only source of copy reports
[R5].

**Not modelled: when and from whom.**

| Trigger | Action |
|---|---|
| Start or wake | Short pulls of every room from two members |
| Steady state | A waiting pull per room to each of two *anchors*: the most reliable member, and one that rotates at each wait through every eligible member and each of its addresses (N2), so groups that favour their own anchors still meet |
| Local write or new records | Re-issue waiting pulls at once, with offers |
| A peer holds what we lack | Pull from it at once; repeat while `more` |
| Failure | Back off 1 s doubling to 5 min with jitter; next member |

Standard-library HTTP/2 (`ForceAttemptHTTP2`) lets
waiting pulls to one peer share a TLS connection. Limits: request 1 MiB;
response 4 MiB or 512 records; four other concurrent requests per peer, then
`429`; the 256 inbound connection cap stays.

**Cost (estimates only).** In a 64-machine room about 130 transfers of
about 2 KB deliver 63 copies of a 1 KiB message; each full summary is about
27 KB after 30 days (section 10), more as JSON. Step 8 measures JSON and
base64 overhead, summaries, duplicate offers, 100 rooms, anchor load, and an
outbound-only machine behind a relay end to end (N1).

## 5. Storage

**Not modelled: schema.** Migration 2 adds the new tables and columns;
migration 3 (step 8) drops `messages`, `deliveries`, `marks` and
`rooms.owner`. No data is carried over (nothing was released). One
`DECISIONS.md` entry lists the tables and why each is needed now.

| Table | Holds | Key and indexes |
|---|---|---|
| `rooms` | id, label, create entry id, left (tombstone) | id |
| `log` | entry bytes, signature, state (accepted, held, refused), arrival batch | (room, id); (room, author, seq) |
| `members` | cache derived from `log`: node, admin, name, addresses, last answered | (room, node) |
| `records` | header bytes, signature, body or null, reason [R6], parsed header fields | (room, id); unique (room, author, epoch, seq); (room, ref); (room, parent); (room, thread) |
| `streams` | contiguous position; highest position known and when (gaps) | (room, author, epoch) |
| `observed` | each peer's last summary and its time | (room, peer) |
| `agents` | as today | id |
| `cursors` | per agent and stream: start and cursor [R7, R10] | (agent, room, author, epoch) |
| `reads` | confirmed keys, the text version confirmed, per-author heads [R7] | (agent, room, key) |
| `invites` | secret hash, room, history, role, expiry, bound key, entries returned | secret hash |
| `requests` | what a person is asked to approve, and the answer (P3) | id |

One transaction each: a send; applying one response or offer; a Seen;
signing a log entry; a pruning pass. On start `members.Machine` is rebuilt
by replaying `log` in arrival batches, which is deterministic.

Pruning runs hourly and before every read, page and offer: text past expiry
by this clock becomes `expired`, never undone; withdrawn text is deleted
when the withdraw is stored [R6]. Signed headers and read state are kept for
the room's life in stage 1 (section 10).

## 6. Reading

The inbox is the model's walk from each agent's cursors [R7, R8]: winning
text, `expired` and withdraw or replace notices, `late`, and what it waits
for. Only Seen moves a cursor, confirming exactly the keys and versions the
inbox returned; the hook prints counts only. Replies carry `parent_state`.
The thread view lists held messages with that `thread`; the branch view
follows `parent` down and misses replies below one not held (G3). Neither
marks anything. Only an agent whose Seen confirmed the text may report it
handled [R9]; copy reports are dated observations [R5].

Reasons shown (A4) in `a2p2 trace`, the page and tool results: *missing*
(being fetched, expired, withdrawn, before admission); *refused* (does not
verify, not the next position, fork, author never a member, dated ahead,
peer not a member); *pending* (needs a second admin, waits for a person,
waits for an entry it had seen); and every `members.Resolve` reason.

MCP tools, still seven:

| Tool | Before | After |
|---|---|---|
| `whoami` | label, node, rooms, mode | unchanged |
| `peers` | rooms, members, agents, reachability | adds admins, pending changes |
| `send(text, room?, to?)` | post | adds `reply_to?` (reply with your own `to`, D10), `ask_handled?` (D5), `replaces?` (own message: replace, or withdraw with empty text, D9) |
| `inbox(room?, wait_seconds?, limit?)` | unread; shim confirms | adds `thread?`: a root gives the thread, other ids the branch; marks nothing |
| `reply`, `ack` | mark handled | only where [R9] allows |
| `ask_human` | question kind | message to `human` here |

Copy and handled reports stay in `a2p2 trace` and the page; a parameter for
them does not fit the budget. Four parameters at about 30 tokens take the
definitions from about 800 to about 920; `TestToolsGolden` asserts A2's
1,000 with the same estimate as the 800 figure.

**Not modelled: alerts (A3).** New local call `alerts(wait_ms)` and CLI
`a2p2 alerts --wait`, needed because A3 asks for a way to wait for an alert:
it returns when a request waits for a person, a fork is seen, or text has
been missing for a day.

## 7. Trust changes

**Not modelled: pairing (T3).** `a2p2 room invite <room> [--history]
[--member|--admin]` on an admin machine prints a token such as
`a2p2:7K3Q-M9XD-2HTW-PB4R-C8NV-QF6J-W1ZA-5TME@203.0.113.7:7443`: 32
base32 characters (160 bits): an 80-bit secret and the first 80 bits of the
admin key's fingerprint, plus addresses. Single use, 15 minutes, only the
secret's hash stored. `a2p2 join` (token on stdin) dials, checks the TLS key
against the fingerprint, and asks the person to confirm room label and
fingerprint before sending the secret. The admin machine binds the invite to
the joiner's key and raises a request recording exactly what is to be
approved: room, key fingerprint, role, history. On approval it revalidates
(still an admin, invite bound to this key, target not a member) and signs.
The join is idempotent per key: a retry after a lost answer returns the same
entries. The joiner polls with back-off until approval or expiry. No choice
means no history (T3).

**Not modelled: requests for a person (P3), narrowed.** Confirmation is a
separate step the agent tools do not offer: on the local page or by `a2p2
confirm <id>` in a terminal (new: always-on machines are often headless),
recorded, naming exactly what is approved. It gates signing log entries,
turning networking on and sending a join secret. A same-user agent that sets
out to bypass it can; an OS-separated approver may come later.

| Change | Entries | Confirmed on |
|---|---|---|
| Add a machine | one `add` by any admin [M3] | that admin and the joiner |
| Remove a non-admin | one `remove` [M3] | that admin |
| Make an admin, one admin so far | one `admin` [M4] | that admin (and a new joiner) |
| Make an admin, more admins | matching `admin` from two [M4] | both |
| Remove an admin (own vote not counted) | matching `remove` from two [M4, M5] | both |
| Remove a machine being made an admin concurrently | matching `remove` from two [M4] | both |

`a2p2 room admin <room> <machine>` is new, needed to replace a lost admin
(T9); the other admin sees the first entry as a request ("needs a matching
entry from another admin", [M4]). `a2p2 room list` states the section 6
limits that apply to the room's admin count.

**First three admins (T9 default).** Invites for the second and third
machines default to `--admin`. The second is one `admin` entry; the third an
`add`, so it joins at once, plus `admin` entries from both admins, confirmed
on both and on the joiner. `TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations`
covers only how these entries resolve; step 5 adds a bootstrap test for
pairing and confirmations. A daemon never co-signs on its own.

**Leaving.** A vote on one's own removal does not count [M5], so `a2p2 room
leave` (new: otherwise a machine cannot stop carrying a room) is local: it
writes a tombstone, cancels in-flight pulls and deletes the room's data, so a
late response cannot recreate it. Rejoining needs a new invite; the machine
stays listed until an admin removes it.

## 8. Failure handling

| Fault | What happens | Model test or gap |
|---|---|---|
| Sleep | Others carry on; it pulls on wake | `TestRelayDeliversAndReportsHandledWithoutAuthorAndReaderEverAwakeTogether` |
| Crash, lost reply | Send durable on return (D1); unconfirmed entries re-shown; next pull repeats | `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Disk lost | New key, added again; others repair its records | `TestDiskLostOnOnlyOtherHolderRecordSurvivesOnAuthorAndRepairContinues` |
| Restore, author | New stream, no fork; older records may arrive late, marked | `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork` |
| Restore, reader | May be re-shown; handled not repeated | `TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped` |
| Restore, admin | Pulls the log before signing; a fork is an alert | `TestBackupRestoreForkIsSettledByRemovalOfTheForkingAdmin`; gap 1 |
| Clock back / forward | Expired stays expired / expires early; future-dated records wait (not modelled) | `TestTextExpiredHereStaysExpiredWhenTheClockIsSetBack`, `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects` |
| Removed machine | 403 once known; its records kept, marked (being modelled) | `TestConcurrentAddAndRemoveOfSameMachineRemovalWins` |
| Forked key | First branch or record kept, evidence passed on, alert | `TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember`; Q8 |
| Relay alters or strips | Refused, or text repaired | `TestRelayThatAltersARecordIsRefused`, `TestRelayThatStripsTextAndClaimsWithdrawalCannotLoseIt` |
| Peer withholds | Other streams progress; rotation reaches other holders | `TestARefusedStreamDoesNotStarveOthersUnderAPageLimit`; G4 |

## 9. Package plan

| Package | Change |
|---|---|
| `internal/core` | Records and entries, encoding, signing, limits; the envelope goes with its last caller |
| `internal/members` | **Added**: `internal/model/members` moved with its tests, behind an adapter |
| `internal/model/delivery` | Stays as the oracle; not imported by the daemon |
| `internal/store` | Migrations 2 and 3; each method one transaction |
| `internal/node` | Send, pull, offers, inbox, Seen, amendments, pruning, scheduler, invites, requests; outbox and owner refresh go; `Transport` is `Pull` and `Join` |
| `internal/peer` | `/p/v2/pull`, `/p/v2/join`, HTTP/2; `/p/v1/*` go |
| `localapi`, `mcpshim`, `web`, `cmd` | Section 6 tools, `alerts`, `confirm`, `room admin`, `room leave` |

The membership model is pure and small, so the daemon runs the code the
searches ran, through an adapter: the id function is injected, so the
model's hash and the signed-bytes id are one path through `Machine.Receive`
and `validate`; `Offer` returns entries in causal order, not by id; `core`
checks signature and id first. The delivery model keeps maps the daemon
keeps in SQLite, so it stays an oracle: a differential test runs its
schedules against real nodes on the in-memory network, ids and clocks
normalised. Encoding, scheduling and pairing get their own tests.

Steps, each ending with `make check` passing and with `TRACE.md` rows
updated in the step that moves or deletes a test they name:

1. Models: who may author, fork evidence, offers against stale membership
   and history permission. No product code.
2. `core`: records and entries beside the envelope, so callers compile;
   fuzzed decoder.
3. `internal/members` promoted with the adapter.
4. `store`: migration 2. `node`, `localapi`, `mcpshim` and `cmd` converted
   together for the `local` room.
5. Room log, invites, requests, `/p/v2/join`; owner code deleted; bootstrap
   test (pairing, confirmations, add then admin).
6. Pull, offers, scheduler; outbox, `/p/v1/deliver` and the envelope deleted
   with their last callers; tests over real TLS.
7. Tools, CLI, alerts, web page.
8. Migration 3 and a migration test; differential test; the measurements of
   section 4; fault matrix rows in `TESTING.md`; this file replaces
   `DESIGN.md`.

## 10. Open questions and known gaps

Skipped KNOWN GAP tests:

| Test | Design response |
|---|---|
| `TestAuthorLearnsOfCopiesOnMachinesItNeverMet` (G1) | Accept: copies only from direct contact |
| `TestNewcomerWithoutHistoryIsNotHandedPostsItsAdmitterHadNotSeen` (G2) | Accept as the stated T3 exception |
| `TestBranchUnderAMessageIncludesRepliesBelowAMissingReply` (G3) | Accept for stage 1; the thread view lists them; later, an ignorable ancestors field |
| `TestWithholderThatUnderstatesWhatItHoldsIsDetected` (G4) | Accept; rotation narrows it |
| `TestCopyReportsDoNotOutliveAHolderRestoredToBeforeTheCopy` (G5) | Accept; reports carry dates |
| `TestForkInTwoAdminRoomIsNeverSettled` (members gap 1) | Being modelled (fork evidence) |
| `TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval` (members gap 4) | Accept; no rule can refuse it |

Members gaps 2, 3 and 5 are passing tests of stated limits.

Not settled:

1. P3 is narrowed (section 7); an OS-separated approver may come later.
2. Stream state grows until compaction is designed: 1,920 streams and a
   27 KB summary per 64-machine room after 30 days
   (`TestStreamCostForSixtyFourMachinesOverThirtyDays`), more after. Checkpoints are designed and modelled after step 8.
3. The rules being modelled may change sections 2 to 4.
4. Waiting pulls reach 200 HTTP/2 streams at 100 rooms; one pull covering
   every shared room may be needed.
5. Posting policy (T6) is deferred to stage 3, as in `TRACE.md`; it needs an
   entry kind the members model lacks [M12].
6. Section 4's costs are estimates until step 8.
