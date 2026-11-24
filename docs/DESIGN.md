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
  holding every log head that member advertised (rooms it created or just
  joined, and rooms where it is the only member, are exempt). It narrows
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

Measured with 64 machines after 30 days of daily restarts, an idle pull
takes 177 bytes up and 72 down. One new message takes 1.7 KB up and
14.9 KB down in three requests.

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
message to 63 machines. Not yet measured: duplicate offers, 100 rooms,
anchor load, and N1 behind a relay.

## 5. Storage

**Not modelled: schema** (PROTOCOL 8). Migrations 2 to 9 create the
tables; migration 10 removes the tables of the earlier v0.2 daemon, and if
they held messages or rooms the daemon tells the person once. In stage 1,
the machine keeps signed headers and read state for the room's life.
`DECISIONS.md` entries justify the tables.

Each send, response application, request log and offer application, Seen,
log entry signing and pruning pass takes one transaction. On startup the
daemon rebuilds `members.Machine` by replaying `log` in arrival batches.
The result is deterministic. It prunes hourly and before every read, page
and offer. Text past expiry becomes `expired` permanently. The daemon
deletes withdrawn text when it stores the withdraw [R6].

## 6. Reading and surfaces

The inbox is the model's walk from each agent's cursors [R7]: winning text,
`expired`, withdraw and replace notices, `late`, and what it waits for. Only
Seen moves a cursor, confirming exactly the keys and versions the inbox
returned; the hook prints counts only and never message text. Claude Code,
Codex and Antigravity get the MCP server and a hook; OpenCode and Hermes
Agent get the MCP server only, and Pi, which has no MCP, a skill that
points its model at the CLI ([harness notes](HARNESSES.md)). Replies carry
`parent_state`. The thread view lists held messages of a thread, or with
`after` only those that follow a given message of it; the branch view
follows `parent` down and misses replies below one not held (G3). The
inbox tool's `recent` (1 to 50) reads a room's last messages through
`history`. None of these three marks anything. A reply goes to its
parent's author unless the sender names addressees (D10).

**Not modelled: the reply tool's addressees.** Without `to`, the `reply`
tool addresses the parent's author and every agent of a current member that
has posted in the thread as held here, without the sender, at most 32 (the
parent's author always kept, then the newest posters). The list is written
as `to`, so the record is an ordinary addressed reply and the reading rule
is unchanged. `a2p2 send --reply-to` without `--to` still reaches only the
parent's author.

**Handled reports.** Only an agent the inbox has shown the text can report
it handled [R7]. `reply` sends the report after the reply; a later `ack`
of the same message, or a second `ack`, succeeds without a second report
and names the first. An `ack` that is not allowed is refused with the
reason: no such message, not addressed and no `ask_handled`, the agent's
own message, or not yet shown by `inbox` (a thread view or `recent` does
not count). The reply and ack tools name the message `id`.

Reasons shown (A4) in `a2p2 trace`, the page and tool results: *missing*
(being fetched, expired, withdrawn, before admission); *refused* (does not
verify, not the next position, fork, author never admitted, dated ahead,
peer not a member); *pending* (needs a second admin, waits for a person,
waits for an entry it had seen); and every `members.Resolve` reason. Every
refusal of the local API and the tools is a sentence, one of a few stable
codes and whether repeating the call later may succeed (PROTOCOL 7).

MCP tools stay seven (PROTOCOL 6): `send` with `reply_to`, `ask_handled`
and `replaces`; `inbox` with `thread`, `after`, `recent` and
`wait_seconds`; `reply` and `ack` taking `id`; `peers` showing each room's
machines and the agents seen writing there; `ask_human` sending to `human`.
Each tool carries annotations: `whoami` and `peers` are read-only; `send`
is destructive, since `replaces` rewrites or withdraws a message; `ack` is
idempotent. The inbox tool waits at most 30 seconds (the CLI and the local
API allow 120), says when a wait ended with nothing, and a cancelled wait
marks nothing seen. Results are compact JSON with `<`, `>` and `&` as
written. Copy and handled reports stay in `a2p2 trace` and the page.

A2 is checked by a proxy, compact definition bytes divided by four and
rounded up: 3,996 bytes, 999 of the 1,000 allowed. `TestToolsGolden` checks
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

**Not modelled: requests for a person (P3), narrowed.** Signing log
entries, turning networking on and sending a join accept wait for
a recorded confirmation naming exactly what is approved, given on the local
page or by `a2p2 confirm` in a terminal (always-on machines are often
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

`a2p2 room admin` replaces a lost admin (T9); the other admin sees the
first entry as a request [M4].

**First three admins (T9 default).** The second machine gets one `admin`
entry; the third an `add` (it joins at once) plus `admin` entries from both
admins, confirmed on both and on the joiner. The model test
`TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations` covers only
resolution; `TestBootstrapFirstThreeMachinesBecomeAdminsWithBothAdminsConfirmingTheThird`
covers the daemon's code. A daemon never co-signs alone.

**Leaving.** A vote on one's own removal does not count [M5], so `a2p2 room
leave` (otherwise a machine cannot stop carrying a room) is local: the
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
designed (1,920 streams per 64-machine room after 30 days,
`TestStreamCostForSixtyFourMachinesOverThirtyDays`); one pull per peer for
all shared rooms may be needed at 100 rooms; posting policy (T6) is
deferred to stage 3 and needs a new entry kind [M14].
