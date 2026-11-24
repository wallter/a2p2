# Design for the next version (draft, not built)

**Status: draft for review, revision 3, 2026-11-23; nothing here is built.**
Revised after `docs/research/decisions/design-next-review-sol.md` and
`design-next-rereview-sol.md`. `docs/DESIGN.md` describes the code as it is
(v0.2). This file gives the rules of stage 1 of `docs/REQUIREMENTS.md`
section 9 and why; `docs/PROTOCOL-next.md` gives the exact formats.

Notation: **[M4]** is rule 4 in `internal/model/members/FINDINGS.md`;
**[R7]** is rule 7 and **[RM2]** is membership rule R2 in
`internal/model/delivery/FINDINGS.md`. Requirements are cited by id.
**Not modelled** marks a choice no model test covers; reviewers should
attack those first.

## 1. Summary

Changes from v0.2: signed records (D8); pull from any member instead of the
outbox (D2); a new stream per daemon start [R2]; a signed member log with
several admins [M1–M14]; 30-day text with signed stubs [R6]; withdraw,
replace, handled and replies as records [R7]; per-agent cursors [R7]; short
invites confirmed by a person (T3, P3); outbound-only machines (N1).

Unchanged: one daemon per machine, `A2P2_HOME`, the local socket's trust
boundary, the Ed25519 key and pinned mutual TLS 1.3, HTTP+JSON, the
stateless MCP shim with CLI `--json` and socket parity (A1), peer text as
data, never instructions (T8), harness hooks, the loopback UI, no bodies in
logs, and the one-member `local` room.

## 2. Records

A record is a signed header plus an optional body [R1], of four kinds:
`message`, `handled`, `withdraw`, `replace` (delivery FINDINGS, "Record").
`ask_human` becomes a message to the agent `human` on this machine; the
answer is a reply from `human`. Formats: PROTOCOL sections 1 and 2.

- A receiver stores a record only if it verifies, is the next position of
  its stream with matching `prev`, and (first record) its links are held
  [R4]. A second record at a held position is a fork and is refused.
- Every start writes a new stream whose first record links every head of
  the author's history held [R2]; there is no cap on links.
- A replace carries a version one above the text it replaces as its author
  knew it (the model's `World.amend`); the highest version wins, then the
  highest id [R7]. A withdraw or replace must share `ref`'s author machine;
  any agent there may issue it (T2).
- Only an agent shown the text may report handled [R7]: an addressee, or
  anyone on a room-wide post with `ask_handled` (the model's `canHandle`).
- Text expires 30 days after `created` by the holder's clock, recorded and
  never undone [R6]. **Not modelled:** a record dated more than 24 hours
  ahead of the receiver's clock is not accepted yet; its stream waits.

**Who may author [RM1].** A machine stores a record if its author is or was
a member in its view, and only from a peer that is a member now. A record
by an author who is not a member now is always shown marked "author
removed", whoever relayed it. A voided author's records are no longer
shown or served, and agents shown one get a "voided" notice
(`TestWithdrawWrittenBeforeRemovalAndArrivingAfterStillWorks`,
`TestVoidedAdmissionHidesAndDropsItsRecordsAndTellsReadersWhoSawThem`).
There is no cut-off on records, so a removed machine's later writes can
still reach informed machines through a member not yet told of the removal
(`TestRecordWrittenAfterRemovalReachesNoHonestMemberButARelayedCopyIsKeptMarked`).
T1 therefore holds after a removal only in this weaker form: such records
are marked, never shown as a member's.

**"Was a member" is "has ever had an effective admission"**, which is
`members.State.Admitted` [M13]; it is not period-aware. After any effective
admission a machine's records are kept, and marked when it is not a member
now, even if a later grant of it is voided. `Admitted` can shrink when more
entries show an admission never took effect
(`TestAdmittedCanShrinkWhenMoreEntriesArrive`), and [M13] says a store must
not drop records only for that. **Not modelled:** such records are hidden
and not served but their bytes are kept, so they return if the machine
re-enters `Admitted`; the delivery model drops them [RM1].

**Not modelled: too many heads.** A machine whose own heads would not fit
in a first record's header (PROTOCOL 1) has written from many restored
backups without catching up. It refuses to write and says why; the person
re-adds it under a new key. This is a stated limit.

## 3. Room log

An entry is one of `create`, `add`, `remove`, `admin` [M14]; `create`
carries the room label (**not modelled**, display only). An `add`, or an
`admin` naming a non-member, carries `history` and `from`, the admitter's
per-author heads (PROTOCOL 3).

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
  entry for a room until it has pulled that room's log from another member
  (creation and sole-member rooms exempt). It narrows the restore problem
  and cannot detect a restore.
- **No-history grants.** When two grants differ, the more restrictive
  applies [RM2]: without history beats with history, and of two frontiers
  the one that hides more for each author, which is the model's per-stream
  maximum (`restrict` in `membership.go`;
  `TestTwoGrantsThatDifferOnHistoryTheMoreRestrictiveApplies`). **Not
  modelled:** a record whose relation to `from` cannot be decided is
  treated as hidden.

`a2p2 room list` and the page show members, admins, changes waiting for a
second admin [M4], entries without effect with reasons, fork evidence, and
the REQUIREMENTS section 6 limits for the room's admin count. An unsettled
fork is an alert (A3).

## 4. Pull

One request type over the pinned mutual TLS, per room: the model's Pull
[R3, R5] carried both ways (PROTOCOL 4).

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

**Not modelled: encoding of the model's summary.** A request carries, per
author, a digest of that author's stream positions, positions without text,
and wanted texts; the server answers with its own detail for authors whose
digest differs, and the requester repeats with its detail for just those
(PROTOCOL 4.3). Equal digests mean equal holdings for that author, so the
model's server would send nothing for it and learns exactly what a full
summary would tell it; for the other authors the server receives the full
[R3] summary, so pages and copy reports are those of the model. For 64
machines a digest-only request is about 6 KB, and detail is about 1.2 KB per
differing author after 30 days.

**Not modelled: when and from whom.**

| Trigger | Action |
|---|---|
| Start or wake | Short pulls of every room from two members |
| Steady state | A waiting pull per room to each of two *anchors*: the most reliable member, and one that rotates at every end, cancellation or failure of a wait through every eligible member and each of its addresses (N2) |
| Local write or new records | Re-issue waiting pulls at once, with offers |
| A peer holds what we lack | Pull from it at once; repeat while `more` |
| Failure | Back off 1 s doubling to 5 min with jitter; next member |

A server holds a waiting pull at most 240 s; client and server deadlines
exceed it by 30 s. A rotating anchor is first probed with a short pull of
10 s; a probe that fails moves on. Waiting pulls to one peer share one
HTTP/2 connection.

**Cost (estimates).** About 130 transfers of about 2 KB deliver 63 copies of
a 1 KiB message in a 64-machine room, plus summaries. Step 7 measures
JSON and base64 overhead, summaries, duplicate offers, 100 rooms, anchor
load, and an outbound-only machine behind a relay (N1).

## 5. Storage

**Not modelled: schema** (PROTOCOL 8). Migration 2 drops the v0.2 tables'
data and creates the new tables; the daemon tells the person once. Signed
headers and read state are kept for the room's life in stage 1. One
`DECISIONS.md` entry justifies the tables.

One transaction each: a send; applying one response or one request's log and
offer; a Seen; signing a log entry; a pruning pass. On start
`members.Machine` is rebuilt by replaying `log` in arrival batches, which is
deterministic. Pruning runs hourly and before every read, page and offer:
text past expiry becomes `expired`, never undone; withdrawn text is deleted
when the withdraw is stored [R6].

## 6. Reading and surfaces

The inbox is the model's walk from each agent's cursors [R7]: winning text,
`expired`, withdraw and replace notices, `late`, and what it waits for. Only
Seen moves a cursor, confirming exactly the keys and versions the inbox
returned; the hook prints counts only. Replies carry `parent_state`. The
thread view lists held messages of a thread; the branch view follows
`parent` down and misses replies below one not held (G3). Neither marks
anything. A reply goes to its parent's author unless the sender names
addressees (D10).

Reasons shown (A4) in `a2p2 trace`, the page and tool results: *missing*
(being fetched, expired, withdrawn, before admission); *refused* (does not
verify, not the next position, fork, author never admitted, dated ahead,
peer not a member); *pending* (needs a second admin, waits for a person,
waits for an entry it had seen); and every `members.Resolve` reason.

MCP tools stay seven (exact JSON in PROTOCOL 6): `send` gains `reply_to`,
`ask_handled` and `replaces`; `inbox` gains `thread`; `peers` shows admins
and pending changes; `reply` and `ack` report handled only where [R7]
allows; `ask_human` sends to `human`. Copy and handled reports stay in
`a2p2 trace` and the page. A2 is checked by a proxy, the bytes of the
compact tool definitions divided by four, rounded up: 3,224 bytes and 806
today; `TestToolsGolden` asserts at most 1,000. The proxy is not a token
count; the true count per model family is measured once in step 7 and
recorded.

**Not modelled: alerts (A3).** `alerts(wait_ms)` on the socket and `a2p2
alerts --wait` return on a request for a person, a fork, or text missing
for a day; A3 needs a way to wait.

## 7. Trust changes

**Not modelled: join (T3).** The token holds a secret, the admin key's
fingerprint and addresses (PROTOCOL 5). The joiner checks the admin's TLS
key against the fingerprint and stops on a mismatch; sends the secret; the
admin machine binds the invite to the joiner's key (single use) and answers
with a preview of room, role and history; a person on the joiner confirms
and the joiner accepts; the admin machine raises a request naming the
joiner's fingerprint, label, role and history; a person confirms; the
daemon revalidates and signs and returns the entries; the joiner may poll.
Once signed, the joiner is a member by the log, so any later request from
that key is answered as a member's, expired invite or not. If the invite
expired or the joiner was removed before signing, the answer is a refusal
with a reason, shown to the person. No choice means no history.

**Not modelled: requests for a person (P3), narrowed.** Signing log
entries, turning networking on and sending a join accept wait for
a recorded confirmation naming exactly what is approved, given on the local
page or by `a2p2 confirm` in a terminal (new: always-on machines are often
headless), never through agent tools. A same-user agent set on bypassing
it can; an OS-separated approver may come later.

| Change | Entries | Confirmed on |
|---|---|---|
| Add a machine | one `add` by any admin [M3] | that admin and the joiner |
| Remove a non-admin | one `remove` [M3] | that admin |
| Make an admin, one admin so far | one `admin` [M4] | that admin (and a new joiner) |
| Make an admin, more admins | matching `admin` from two [M4] | both |
| Remove an admin | matching `remove` from two, own vote not counted [M4, M5]; one, if the target signed a fork [M12] | each signer |
| Remove a machine being made an admin concurrently | matching `remove` from two [M4] | both |

`a2p2 room admin` is new, needed to replace a lost admin (T9); the other
admin sees the first entry as a request [M4].

**First three admins (T9 default).** Invites for the second and third
machines default to admin. The second is one `admin` entry; the third an
`add`, so it joins at once, plus `admin` entries from both admins, confirmed
on both and on the joiner.
`TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations` covers only
how these resolve; step 4 adds a bootstrap test. A daemon never co-signs on
its own.

**Leaving.** A vote on one's own removal does not count [M5], so `a2p2 room
leave` (new: otherwise a machine cannot stop carrying a room) is local: the
room's data is deleted and its generation changes, so no late response
applies. Rejoining needs a new invite; an admin removes it from the list.

## 8. Failure handling

| Fault | What happens | Model test |
|---|---|---|
| Sleep | Others carry on; it pulls on wake | `TestRelayDeliversAndReportsHandledWithoutAuthorAndReaderEverAwakeTogether` |
| Crash, lost reply | Send durable on return (D1); unconfirmed entries re-shown; next pull repeats | `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Disk lost | Others repair its records; it rejoins under a new key | `TestDiskLostOnOnlyOtherHolderRecordSurvivesOnAuthorAndRepairContinues`; rejoining not covered by a model test |
| Restore, author | New stream, no fork; older records may arrive late, marked | `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork` |
| Restore, reader | May be re-shown; handled not repeated | `TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped` |
| Restore, admin | Pulls the log before signing; a fork is an alert until one other admin settles it [M12] | `TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone` |
| Clock back / forward | Expired stays expired / expires early; future-dated records wait | `TestTextExpiredHereStaysExpiredWhenTheClockIsSetBack`, `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects`; waiting not covered |
| Removed machine | Refused as a peer once known; its records kept, marked | `TestMessageOfARemovedAuthorReachesAMachineThatNeverMetItMarked`, `TestHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited` |
| Forked log key | First branch kept, evidence passed on, alert | `TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember`; the alert not covered |
| Forked record stream | Second record refused | Q8 checks closure only; passing on and alerting not covered |
| Relay alters or strips | Refused, or text repaired | `TestRelayThatAltersARecordIsRefused`, `TestRelayThatStripsTextAndClaimsWithdrawalCannotLoseIt` |
| Peer withholds | Other streams progress; rotation reaches other holders | `TestARefusedStreamDoesNotStarveOthersUnderAPageLimit` covers paging; rotation not covered (G4) |

Tests the build must add: rejoining after a lost disk; future-dated
records; fork alerts and record-fork evidence; anchor rotation; the token
proxy in `TestToolsGolden`; join and bootstrap; generation on leave and
rejoin; the two-level summary against full summaries.

## 9. Package plan

| Package | Change |
|---|---|
| `internal/core` | Records, entries, encoding, signing, limits; the envelope goes in step 3 |
| `internal/members` | **Added**: `internal/model/members` moved with its tests, behind an adapter |
| `internal/model/delivery` | Stays as the oracle; not imported by the daemon |
| `internal/store` | Migration 2; each method one transaction |
| `internal/node` | Send, pull, offers, inbox, Seen, amendments, pruning, scheduler, invites, requests; outbox and owner refresh go |
| `internal/peer` | `/p/v2/pull`, `/p/v2/join`, HTTP/2; `/p/v1/*` go |
| `localapi`, `mcpshim`, `web`, `cmd` | PROTOCOL 6 and 7 |

The adapter: the id function is injected (model hash in tests, signed-bytes
id in the daemon, one path through `Machine.Receive` and `validate`);
`Offer` returns causal order; `core` checks signature and id first. A
differential test runs the delivery model's schedules against real nodes,
ids and clocks normalised. Encoding, scheduling and joining get their own
tests.

Steps, each ending with `make check` passing and `TRACE.md` rows updated in
the step that moves or deletes a test they name:

1. `core`: records and entries beside the envelope; decoder; record fuzz target.
2. `internal/members` promoted with the adapter and `Admitted`, with tests.
3. `store` migration 2 with a test from a populated v0.2 database (data
   dropped, daemon starts, person told once); `node`, `localapi`, `mcpshim`,
   `cmd`, `web`, `history` and `trace` converted together; the envelope,
   outbox and `/p/v1/*` deleted. Rooms with other machines are unavailable
   until step 5 (nothing is released between steps).
4. Room log, invites, join, requests, `confirm`, `alerts`; bootstrap test.
5. Pull, offers, scheduler; tests over real TLS.
6. Final tool definitions and golden file; web page.
7. Differential test; measurements, including the token count; `TESTING.md`
   fault rows; this file replaces `DESIGN.md`.

## 10. Known gaps and limits

| Skipped test | Response |
|---|---|
| `TestAuthorLearnsOfCopiesOnMachinesItNeverMet` (G1) | Accept: copies only from direct contact |
| `TestNewcomerWithoutHistoryIsNotHandedPostsItsAdmitterHadNotSeen` (G2) | Accept as the stated T3 exception |
| `TestBranchUnderAMessageIncludesRepliesBelowAMissingReply` (G3) | Accept for stage 1; the thread view lists them |
| `TestWithholderThatUnderstatesWhatItHoldsIsDetected` (G4) | Accept; rotation narrows it |
| `TestCopyReportsDoNotOutliveAHolderRestoredToBeforeTheCopy` (G5) | Accept; reports carry dates |
| `TestWithdrawThatOnlyTheRemovedAuthorHoldsStillReachesMembers` (G6) | Accept: what a removed machine wrote that no member fetched while still allowed to relay it is lost |
| `TestAMoreRestrictiveGrantLearnedLaterDoesNotTakeBackTextAlreadyShown` (G7) | Accept: a narrower grant learned later drops shown text silently; a notice is to be modelled next |
| `TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval`, `TestStolenKeyCanSecondAForkingKeysVoidedRemovalAndThenJoinTheRemoval` (members gap 4) | Accept; no rule can refuse it |

Members gap 1 is closed by [M12]; gaps 2, 3 and 5 are passing tests of
stated limits.

What remains partly resolved, as REQUIREMENTS section 6 limits:

| Finding | Limit |
|---|---|
| A removed machine's posts reach informed machines through a stale member, marked (T1 weakened) | "Removal is not instant" |
| An offer hands text before the sender learns of a removal or narrower grant | "Removal is not instant" |
| A restored admin pulls from a stale peer and reuses a position | "An admin machine restored from backup must catch up before it changes membership" |
| A sole admin's fork never settles | "While a room has had only one admin, that key is the room" |
| Too many heads: re-add under a new key | to be added to section 6 |

Open: P3 against a same-user agent; stream state grows until compaction is
designed (1,920 streams per 64-machine room after 30 days,
`TestStreamCostForSixtyFourMachinesOverThirtyDays`); one pull per peer for
all shared rooms may be needed at 100 rooms; posting policy (T6) is
deferred to stage 3 and needs a new entry kind [M14].
