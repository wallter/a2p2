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

Unchanged: one daemon per machine, the socket's trust boundary, the
Ed25519 key and pinned mutual TLS 1.3, HTTP+JSON, the stateless MCP shim
with CLI and socket parity (A1), peer text as data (T8), hooks, the
loopback UI, no bodies in logs, and the `local` room.

## 2. Records

A record is a signed header plus an optional body [R1], of four kinds:
`message`, `handled`, `withdraw`, `replace` (delivery FINDINGS, "Record").
`ask_human` becomes a message to the agent `human` on this machine; the
answer is a reply from `human`. Formats: PROTOCOL sections 1 and 2.

- A receiver stores a record only if it passes PROTOCOL 2, is the next
  position of its stream with matching `prev`, and (first record) every
  link, matched by room, author, epoch, sequence and id, is held [R4]. A
  second record at a held position is a fork and is refused. Text for a
  held header is repaired only if it is wanted here and still permitted.
- Every start writes a new stream whose first record links every head of
  the author's history held [R2]; there is no cap on links.
- Writer rules, not checked on receipt: a replace's version is one above
  the winning version its author knew (the model's `World.amend`); a
  handled report comes only from an agent shown the text [R7]. Receivers
  pick the highest version, then the highest id [R7], whatever arrives.
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

**"Was a member" is "has ever had an effective admission"**:
`members.State.Admitted` [M13], not period-aware. `Admitted` can shrink as
entries arrive (`TestAdmittedCanShrinkWhenMoreEntriesArrive`), and [M13]
says a store must not drop records only for that. **Not modelled:** such
records are hidden and not served but kept, and return if the machine
re-enters `Admitted`; the delivery model drops them [RM1].

**Not modelled: too many heads.** A machine whose own heads do not fit a
first record's header (from many restores without catching up) refuses to
write and says why; the person re-adds it under a new key. A stated limit.

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
  entry for a room until it has pulled that room's log from another member
  (creation and sole-member rooms exempt). It narrows the restore problem
  and cannot detect a restore.
- **History permission.** A machine's current permission comes from the
  effective grants of its current membership period (after its latest
  effective removal). When they differ the more restrictive applies [RM2]:
  without history beats with history, and of two frontiers the one that
  hides more per author, the model's per-stream maximum (`restrict` in
  `membership.go`; `TestTwoGrantsThatDifferOnHistoryTheMoreRestrictiveApplies`).
  **Not modelled:** a frontier id that cannot be resolved fails closed (no
  history), and `members.Resolve` exposes these grants for each member,
  added in step 2.

`a2p2 room list` and the page show members, admins, changes waiting for a
second admin [M4], entries without effect with reasons, fork evidence and
the section 6 limits for the admin count; an unsettled fork is an alert.

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
author, a 32-byte digest of that author's summary; the server lists every
author whose digest differs, and the requester's next requests carry its
detail for all of them, paged under a token bound to that first snapshot
(PROTOCOL 4.3). Equal digests are treated as equal summaries, so the
model's server would send nothing for that author and learns what a full
summary would tell it; for the others it receives the full [R3] summary, so
pages and copy reports are the model's. A round that stores nothing on
either side ends the exchange until the next trigger, so mismatches that
cannot be repaired (expired here, live there) do not loop. For 64 machines
a digest-only request is about 9 KB.

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

**Cost (estimates).** About 130 transfers of about 2 KB deliver a 1 KiB
message to 63 machines. Step 7 measures encoding overhead, summaries,
duplicate offers, 100 rooms, anchor load, and N1 behind a relay.

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

MCP tools stay seven (PROTOCOL 6): `send` gains `reply_to`, `ask_handled`
and `replaces`; `inbox` gains `thread`; `peers` shows admins and pending
changes; `ask_human` sends to `human`. Copy and handled reports stay in
`a2p2 trace` and the page. A2 is checked by a proxy, compact definition
bytes divided by four, rounded up (806 today); `TestToolsGolden` asserts at
most 1,000. The true count per model family is measured once in step 7.

**Not modelled: alerts (A3).** `a2p2 alerts --wait` returns on a request
for a person, a fork, or text missing for a day.

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

**First three admins (T9 default).** The second machine gets one `admin`
entry; the third an `add` (it joins at once) plus `admin` entries from both
admins, confirmed on both and on the joiner. The model test
`TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations` covers only
resolution; step 4 adds a bootstrap test. A daemon never co-signs alone.

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

Tests the build must add: the golden vectors (PROTOCOL 9); rejoining after
a lost disk; future-dated records; fork alerts; anchor rotation; the token
proxy; join states; leave and rejoin; the summary exchange against full
summaries, including a round that cannot progress.

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

The adapter injects the id function (model hash in tests, signed-bytes id
in the daemon) into both `Machine.Receive` and `validate`; `Offer` returns
causal order; `core` checks signature and id first. A differential test runs
the delivery model's schedules against real nodes, ids and clocks
normalised.

Steps, each ending with `make check` passing and `TRACE.md` rows updated in
the step that moves or deletes a test they name:

1. `core`: records and entries beside the envelope; decoder; golden vectors; fuzz target.
2. `internal/members` promoted with the adapter, the pinned create entry,
   chain checks before fork bookkeeping, and per-member current grants, with tests.
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
| Too many heads | to be added to section 6 |

Open: P3 against a same-user agent; stream state grows until compaction is
designed (1,920 streams per 64-machine room after 30 days,
`TestStreamCostForSixtyFourMachinesOverThirtyDays`); one pull per peer for
all shared rooms may be needed at 100 rooms; posting policy (T6) is
deferred to stage 3 and needs a new entry kind [M14].
