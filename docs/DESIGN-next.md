# Design for the next version (draft, not built)

**Status: draft for review, 2026-11-23; nothing here is built.**
`docs/DESIGN.md` describes the code as it is (v0.2). This file describes what
stage 1 of `docs/REQUIREMENTS.md` section 9 will build. It replaces
`DESIGN.md` when that code lands.

Notation: **[M4]** is rule 4 in `internal/model/members/FINDINGS.md`,
**[R7]** is rule 7 in `internal/model/delivery/FINDINGS.md`. Requirements are
cited by id (D5, T9). A paragraph marked **Not modelled** is a design choice
that no model test covers; reviewers should attack those first.

## 1. Summary

What changes from v0.2:

1. Records are signed by the author's node key and verify via any relay (D8).
2. Members pull from any member; the outbox and push from the author go (D2).
3. Each daemon start writes a new stream, so a restore never reuses a position [R2].
4. The member list is a signed log with several admins [M1–M12]; the owner role goes.
5. Text expires after 30 days by the holder's clock; a signed stub stays [R6].
6. Withdraw, replace, handled reports and replies are records [R7–R9].
7. Reading walks per-agent cursors and marks late and missing items [R7].
8. Invites are short, single use, and confirmed by a person on both sides (T3, P3).
9. A machine that cannot accept connections opens every exchange itself (N1).

Unchanged: one binary and daemon per machine, `A2P2_HOME`, the local socket
and its trust boundary, the Ed25519 key and pinned mutual TLS 1.3, HTTP+JSON,
the stateless MCP shim, harness install and hooks, the loopback web UI, no
bodies in logs, and the `local` room (a room whose only member is this
machine).

## 2. Records

A record is a signed header plus an optional body [R1]. Four kinds: `message`,
`handled`, `withdraw`, `replace` [Record section of delivery FINDINGS]. The
v0.2 kinds `question` and `answer` go: `ask_human` sends a message addressed
to the agent `human` on this machine, and the answer is a reply from `human`.

| Field | Meaning |
|---|---|
| `v` | 2 |
| `room` | room id |
| `author` | author node id, 64 hex |
| `epoch` | random 64-bit stream id, chosen at the first write after each daemon start [R2] |
| `seq` | position in the stream, from 1, no gaps |
| `prev` | id of `seq - 1` (seq > 1 only) |
| `links` | seq 1 only: every head of the author's history held, as `{id, epoch, seq}` [R2] |
| `version` | `replace` only: one above the text it replaces [R7] |
| `kind`, `agent` | kind; author's agent label |
| `to` | addressees as `{node, agent}` with the full node id; empty means everyone |
| `ask_handled` | a room-wide post asks for handled reports [R9] |
| `ref` | the message a handled, withdraw or replace concerns |
| `parent`, `thread` | the message answered and the thread's first message (D10) |
| `created` | author's clock, RFC 3339 UTC |
| `body` | SHA-256 of the body bytes, hex; `size`: body length |

**Not modelled: encoding.** The author encodes the header once as compact
JSON and signs `"a2p2 record v2\x00"` plus those bytes. The id is the first
16 bytes of their SHA-256, as 32 hex. On the wire: `{"h": base64(header),
"s": base64(sig), "b": base64(body)}`, `b` omitted when no text is sent.
Headers are kept byte for byte, so nothing is re-encoded, and unknown fields
are carried and ignored (how G3 could be closed later). The body is JSON
`{"text", "choices"}`. `to` uses full node ids so a key ground to match an
8-hex prefix cannot claim another agent's handled rights; tools still show
`label@8hex`.

Limits: body 16 KiB, header 4 KiB, 32 addressees, 8 links, labels 64 bytes,
64 member machines per room.

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
record at a held position is a fork and is refused. Replies go to the
parent's author by default (D10). A withdraw or replace must share `ref`'s
author machine; any agent there may issue it (T2).

**Not modelled: who may author.** A machine stores new records only from
current members in its own state. A removed machine's records already held
are kept and served; later ones are refused and not shown as waiting.

**Not modelled: future clocks.** Text expires at `min(created, first stored
here) + 30 days` by the holder's clock: never later than [R6], so Q9 holds,
and an author whose clock runs ahead cannot keep text alive.

## 3. Room log

An entry has `v`, `room`, `author`, `seq`, `prev`, `parents` (ids of the
entries the author had seen), `kind` (`create`, `add`, `remove`, `admin`),
`target`, and for `create` the room label [M12]. An `add`, or an `admin`
naming a machine that is not yet a member, also carries `history` (default
false) and `from`: the admitter's heads, per author, of the records it held
when signing. Signed bytes are `"a2p2 entry v2\x00"` plus the entry's compact
JSON; the id is as for records.

**Not modelled: `from` encoding.** The model admits from the admitter's
per-stream positions [R10]. Since every machine holds gap-free prefixes
closed under links (Q8), those are exactly the records at or before the
admitter's per-author heads: a few ids per author, not one number per stream.

How a machine uses it:

- Entries go through `members.Machine` [M11]: held until their parents are
  present; a second entry by one key at one position is refused with its
  dependants, both kept as evidence, until the forking key is removed.
- `members.Resolve` turns the accepted set into members, admins and a reason
  for every entry without effect (A4): [M3] to [M10], no timers [M12, T10].
- `admin` also grants membership. There is no demotion: removing an admin
  removes the machine; a later add brings it back [M8].
- Entries are passed on in causal order, evidence after them (members
  FINDINGS, last section but two).

Shown to a person (`a2p2 room list`, web page): members, admins, each change
waiting for a second admin with its reason [M4], every entry without effect
with its reason, and fork evidence ("key X signed two changes at position
n; machines keep the first they saw until two other admins remove X") as an
alert (A3).

**Not modelled: catch up before signing.** A daemon signs no log entry for a
room until, since it started, it has pulled that room's log from another
member, unless the person overrides. This narrows members gap 1 and the
restore limit; it cannot close them.

## 4. Pull

One endpoint, `POST /p/v2/pull`, over the existing pinned mutual TLS, per
room. It is the model's Pull [R3, R5] carried both ways in one round trip.

| Request | Response |
|---|---|
| `room`; `summary` [R3]; `log`: the requester's log heads and entries the server seems to lack; `offer`: a page as the model's server would build it, against the server's last summary; `addrs` (hints); `wait_ms` up to 240,000 | `log` entries the requester lacks; `page` [R3]; the server's `summary`; `peers`: other members' addresses (hints); `more` if cut at a limit |

The server first applies the request's log entries (from a non-member, only
if they make it a member), so a newcomer presenting its own `add` is served
at once (T4); then it requires the TLS node id to be
a member, else 403 (T5); then it stores the `offer` under [R4]. It never
sends text at or before a no-history machine's `from` [R10] and offers text
only for wanted positions (Q12). The requester applies log entries, then the
page [R4]. Each side keeps the other's summary as a dated observation, the
only source of copy reports [R5]. With `wait_ms` the server holds the
response until it has something new or the time passes.

**Not modelled: summaries as changes.** A full summary for 64 machines over
30 days is about 27 KB (`TestStreamCostForSixtyFourMachinesOverThirtyDays`).
A summary is sent as the hash of the last one the receiver acknowledged plus
changed streams; on a mismatch (lost reply, restore) the receiver answers
409 and gets the full one.

**Not modelled: offers.** An offer built from a stale summary may carry
duplicates; [R4] refuses them, so staleness costs bytes, not correctness.
Offers are how an N1 machine passes on what it writes.

**Not modelled: when and from whom.**

| Trigger | Action |
|---|---|
| Daemon start or wake from sleep | Pull every room from two members at once |
| Steady state | Keep one waiting pull per room open to each of two *anchors*: members that answered recently, preferring ones up for longest and on the local network |
| Local write, or new records stored | Re-issue the waiting pulls at once, carrying offers |
| A response or request shows the peer holds what we lack | Pull from it again at once (if `more`, repeat) |
| Every 15 minutes | Pull from one other member chosen at random (repairs G4 in practice, spreads copy reports) |
| Failure | Back off 1 s doubling to 5 min with jitter, then try another member |

Peer connections use HTTP/2 from the standard library (`ForceAttemptHTTP2`),
so all waiting pulls to one peer share one TLS connection.

Limits: request 1 MiB; response 4 MiB or 512 records, then `more`; per peer
one waiting pull per room, four other concurrent requests and 20 per second,
answered `429` beyond that; the 256 inbound connection cap stays.

**Cost per new message, 64 machines (estimate).** Two anchors each make
about 128 links; news crosses each about once, so about 130 transfers of
about 2 KB (1 KiB body, header, signature, change summary) deliver 63
copies: about 4 KB per machine, 260 KB per room. Idle: one re-issued waiting
pull per anchor per room every four minutes.

**N1.** Such a machine opens every exchange: waiting pulls bring news in,
offers carry its records out. It needs one anchor others can reach, and its
text spreads only while it is awake.

## 5. Storage

**Not modelled: schema.** Migration 2 adds the new tables and columns;
migration 3 (step 6) drops `messages`, `deliveries`, `marks` and
`rooms.owner`. No data is carried over (nothing was released). One
`DECISIONS.md` entry lists the tables and why each is needed now.

| Table | Holds | Key and indexes |
|---|---|---|
| `rooms` | id, label, create entry id | id |
| `log` | entry bytes, signature, state (accepted, held, refused), arrival batch | (room, id); (room, author, seq) |
| `members` | cache of Resolve: node, admin, name, addresses, last answered | (room, node) |
| `records` | header bytes, signature, body or null, reason (live, expired, withdrawn, not-admitted, missing) [R6], parsed author, epoch, seq, kind, ref, parent, thread, created, first stored | (room, id); unique (room, author, epoch, seq); (room, ref); (room, parent); (room, thread) |
| `streams` | contiguous position, highest position known and when (gaps), retired flag | (room, author, epoch) |
| `observed` | each peer's last summary, its time and hash | (room, peer) |
| `agents` | as today | id |
| `cursors` | per agent and stream: start and cursor [R7, R10] | (agent, room, author, epoch) |
| `reads` | confirmed keys, the text version confirmed, and per-author heads [R7] | (agent, room, key) |
| `invites` | secret hash, room, history, role, expiry, used by | secret hash |
| `requests` | changes waiting for a person (P3) | id |

One transaction each: a send; applying one response or one offer (log,
members cache, records, observed summary); a Seen (reads, cursors); signing
a log entry; a pruning pass. On start `members.Machine` is rebuilt by
replaying `log` in arrival batches, which is deterministic.

Pruning, hourly and before every read: text past its expiry by this clock
becomes `expired`, never undone
(`TestTextExpiredHereStaysExpiredWhenTheClockIsSetBack`); withdrawn text is
deleted when the withdraw is stored [R6]; headers stay as stubs.

**Not modelled: retiring streams.** Stream state otherwise grows without
bound (delivery FINDINGS). A stream whose every record has been expired here
for seven days is retired: one `streams` row stays; its headers, cursors and
reads go; it leaves summaries; its records are refused if offered again; and
servers stop offering records seven days past their expiry by their own
clock. Seven days covers clock differences up to a week.

## 6. Reading

The inbox is the model's walk from each agent's cursors [R7, R8]: the
winning text (highest version, then id); `expired` for a message that expired
before it was shown; nothing for one withdrawn before it was shown; withdraw
and replace notices only to agents shown the original; `late` per [R7]; and a
list of what it waits for where text is missing. Only Seen moves a cursor;
the hook prints counts only. Replies carry `parent_state` (here, missing,
expired, withdrawn). The thread view lists held messages with that `thread`;
the branch view follows `parent` down and misses replies below one not held
(G3). Neither marks anything. Handled reports follow [R8, R9]; copy reports
are dated observations [R5].

MCP tools, still seven:

| Tool | Before | After |
|---|---|---|
| `whoami` | label, node, rooms, mode | unchanged |
| `peers` | rooms, members, agents, reachability | adds admins and pending changes; `message_id?` returns copy and handled reports (D5, A4) |
| `send(text, room?, to?)` | post | adds `ask_handled?` (D5) and `replaces?`: your own message id; with text it replaces, with empty text it withdraws (D9) |
| `inbox(room?, wait_seconds?, limit?)` | unread, then the shim confirms | adds `thread?`: a thread root gives the thread, any other id the branch under it; marks nothing (D10) |
| `reply(message_id, text)` | reply, marks handled | marks handled only where [R9] allows |
| `ack(message_id)` | handled | refused unless [R9] allows |
| `ask_human(question, choices?, room?)` | question kind | message to `human` on this machine |

Three parameters at about 30 tokens each take the definitions from about 800
to about 900 tokens, inside A2's 1,000; `TestToolsGolden` gains an assertion
of the limit.

## 7. Trust changes

**Not modelled: pairing (T3).** `a2p2 room invite <room> [--history]
[--member|--admin]` on an admin machine prints a token such as
`a2p2:7K3Q-M9XD-2HTW-PB4R-C8NV-QF6J@203.0.113.7:7443`: an 80-bit secret, 50
bits of the admin key's fingerprint, and addresses. Single use, 15 minutes,
only its hash stored. `a2p2 join` (token on stdin) dials, checks the TLS key
against the fingerprint, and asks the person to confirm the room label and
fingerprint before sending the secret. The admin machine binds the invite to
the joiner's key and raises a request ("add *name*, fingerprint *f*, with or
without earlier history"); once confirmed it signs and answers the joiner's
waiting request with the entries. No choice means no history (T3).

**Not modelled: requests for a person (P3).** The daemon signs a log entry,
turns networking on, or sends a join secret only after a person confirms on
the web page (alert per A3) or with `a2p2 confirm <id>`, a new command needed
because always-on machines are often headless. Never over MCP.

Changes:

| Change | Entries | Confirmed on |
|---|---|---|
| Add a machine | one `add` by any admin [M3] | that admin and the joiner |
| Remove a non-admin | one `remove` [M3] | that admin |
| Make an admin, room has one admin | one `admin` [M4] | that admin and the joiner |
| Make an admin, room has more | matching `admin` from two admins [M4] | both admins |
| Remove an admin | matching `remove` from two admins, the target's own vote not counting [M4, M5] | both admins |

`a2p2 room admin <room> <machine>` is new, needed to replace a lost admin
(T9). The other admin sees the first signer's entry as a request with reason
"needs a matching entry from another admin" [M4].

**First three admins (T9 default).** Invites for the second and third
machines default to `--admin`. The second is one `admin` entry, confirmed on
the creator's machine and the joiner. The third is an `add` (so it joins at
once) and `admin` entries from both admins, confirmed on both admin machines
and the joiner (`TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations`).
A daemon never co-signs on its own.

**Leaving.** A machine's vote on its own removal does not count [M5], so
`a2p2 room leave` (new: without it a machine cannot stop carrying a room) is
local: the machine stops pulling and deletes the room. It stays on the list
until an admin removes it.

## 8. Failure handling

| Fault | What happens | Model test or gap |
|---|---|---|
| Sleep | Others carry on; on wake it pulls from two members | `TestRelayDeliversAndReportsHandledWithoutAuthorAndReaderEverAwakeTogether`, `TestTwoMachinesNeverAwakeTogetherExchangeNothingAndSenderSeesNoCopy` |
| Crash | Send committed before return (D1); unconfirmed entries shown again identically; a new stream on restart | `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Lost reply | Server observed, puller stored nothing; next pull repeats; hash mismatch forces one full summary | `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Disk lost | New key, must be added again; others keep and repair its records | `TestDiskLostOnOnlyOtherHolderRecordSurvivesOnAuthorAndRepairContinues` |
| Restore from backup, author | New stream linked to what it holds; no fork; older records may arrive late, marked | `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork`, `TestRestoredAuthorThatCatchesUpBeforeWritingKeepsItsOrder`, `TestContinuingTheOldStreamAfterRestoreLosesTheNewMessageSilently` |
| Restore from backup, reader | May be shown messages again; handled not repeated | `TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped` |
| Restore, admin | Signs nothing until it has pulled the log; if it forked anyway, two admins remove it | `TestBackupRestoreForkIsSettledByRemovalOfTheForkingAdmin`; gap 1 |
| Clock set back | Expired stays expired | `TestTextExpiredHereStaysExpiredWhenTheClockIsSetBack` |
| Clock set forward | Text expires early here, by the difference | `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects` |
| Removed machine | Refused by each member once it knows (403); its new records refused | `TestRemovedAdminsConcurrentRemovalOfCompromisedMachineStands`, `TestConcurrentAddAndRemoveOfSameMachineRemovalWins`; refusal of records not modelled |
| Forked log key | First branch kept, evidence passed on, settled by removal | `TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember`, `TestForkInsideSoleAdminPeriodFoolsOnlyMachinesMissingThatEntryUntilSettled`; gap 1 |
| Forked record stream | Second record refused, shown as evidence | `TestEverySmallScheduleKeepsTheDeliveryProperties` (Q8); display not modelled |
| Relay alters or strips | Refused, or text stays missing and is repaired | `TestRelayThatAltersARecordIsRefused`, `TestRelayThatStripsTextAndClaimsWithdrawalCannotLoseIt` |
| Peer withholds | Other streams progress; random pulls find the rest | `TestARefusedStreamDoesNotStarveOthersUnderAPageLimit`; G4 |

## 9. Package plan

| Package | Change |
|---|---|
| `internal/core` | Record and entry types, encoding, signing, verification, limits; the envelope goes |
| `internal/members` | **Added** by moving `internal/model/members` here with its tests; the only change is that an entry's id is `core`'s id of its signed bytes, checked with the signature before the entry reaches `Machine` |
| `internal/model/delivery` | Stays as the oracle; not imported by the daemon |
| `internal/store` | Migration 2; new methods, each one transaction |
| `internal/node` | Send, pull (serve and apply), inbox walk, Seen, handled, withdraw and replace, pruning, pull scheduler, invites, requests; the outbox and owner refresh go; `Transport` shrinks to `Pull` and `Join` |
| `internal/peer` | `/p/v2/pull`, `/p/v2/join`, HTTP/2; `/p/v1/*` go |
| `localapi`, `mcpshim`, `web`, `cmd` | Section 6 tools, `confirm`, `room admin`, `room leave`, requests page |

The membership model is pure and small, so it is promoted: the daemon runs
the code the searches ran. The delivery model's state is maps the daemon
keeps in SQLite, so it stays an oracle: a differential test in
`internal/node` runs its schedules against real nodes on the in-memory
network and compares every inbox and status.

Steps, each ending with `make check` passing:

1. `core`: records and entries, signing, fuzzed decoder.
2. `internal/members` moved; model tests pass from the new path.
3. `store`: migration 2; store tests.
4. `node`: local send, inbox, Seen, handled, withdraw, replace, threads, on the `local` room; old local paths deleted.
5. Room log, invites, requests, `/p/v2/join`; owner code deleted.
6. Pull, scheduler, N1; outbox and `/p/v1/deliver` deleted; migration 3.
7. Tools, CLI, web page.
8. Differential test; fault matrix rows in `TESTING.md`; this file replaces `DESIGN.md`.

## 10. Open questions and known gaps

Skipped KNOWN GAP tests:

| Test | Design response |
|---|---|
| `TestAuthorLearnsOfCopiesOnMachinesItNeverMet` (G1) | Accept and state: copies only from direct contact |
| `TestNewcomerWithoutHistoryIsNotHandedPostsItsAdmitterHadNotSeen` (G2) | Accept and state; proposed, to model next: an admin pulls from every reachable member before signing an add without history |
| `TestBranchUnderAMessageIncludesRepliesBelowAMissingReply` (G3) | Accept for stage 1; thread view still lists them. Proposed: replies carry ancestor ids, an ignorable header field |
| `TestWithholderThatUnderstatesWhatItHoldsIsDetected` (G4) | Accept and state; the 15-minute random pull narrows it |
| `TestCopyReportsDoNotOutliveAHolderRestoredToBeforeTheCopy` (G5) | Accept; reports are shown with their date |
| `TestForkInTwoAdminRoomIsNeverSettled` (members gap 1) | Accept and state; catch-up before signing narrows it |
| `TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval` (members gap 4) | Accept; no rule can refuse it (FINDINGS) |

Members gaps 2, 3 and 5 are passing tests that demonstrate stated limits.

Not settled:

1. P3 against a same-user agent: an agent with a shell can open the web page
   or run `confirm`. The design makes confirmation explicit and keeps it off
   MCP; it cannot prove a person did it.
2. Which grant sets a re-admitted machine's `history` and `from`. Proposed:
   the earliest effective grant of its current membership period.
3. A machine away longer than 37 days gets no stubs for retired streams.
   Proposed: one room-level notice when its last successful pull is older
   than 30 days.
4. Refusing a removed machine's records loses ones it wrote before removal
   that had not yet arrived; a signed cut-off would conflict with [M9].
5. Waiting pulls per room per anchor reach 200 HTTP/2 streams at 100 rooms;
   one pull covering every shared room may be needed.
6. `peers(message_id?)` mixes two questions; a separate tool costs about 60
   tokens.
7. Posting policy (T6) needs an entry kind the members model lacks [M12];
   it is not in stage 1, and adding it means modelling it first.
8. Section 4's costs are estimates until measured in step 8.
