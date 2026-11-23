# Delivery model: findings

## Rules

1. A record is a signed header, which holds the body's hash, plus a body.
2. Each writing start begins a new stream; positions are never reused. A
   stream's first record links to every head of its author's history held.
3. A pull sends the puller's summary: per stream, its extent, positions
   without text, and the texts it wants. The server sends wanted text, then
   later records, predecessors first, stopping a stream at anything it
   withholds.
4. A machine stores only the next position of a stream, with matching
   predecessor and links.
5. Copies are learned only from summaries, as dated reports.
6. A machine holds a header without text only on its own evidence: the
   author's signed withdraw, its own clock having passed 30 days (recorded
   and never undone), or admission without history. Otherwise the text is
   "missing here": readers wait and any holder repairs it.
7. The inbox walks each stream from the agent's cursor:
   - A message shows its winning text: each replace carries a version one
     above the text it replaced; the highest version wins, then the highest
     id.
   - Expired before shown: "expired". Withdrawn before shown: nothing.
     Missing text: wait.
   - A withdraw or replace is shown only to agents who saw the original.
   - A record is late exactly when the agent already confirmed (or saw
     earlier in this listing) a record of its author it does not follow.
   - Only Seen moves the cursor.
8. A message the agent already reported handled is not shown again.
9. Only an addressee that was shown the text may report handled (any agent
   shown it, on a room-wide post that asks).
10. A newcomer reads from the admitter's positions. Without history it gets
    earlier records as headers only.

## Properties

Q1 to Q7 hold, plus Q8 to Q12:
- Q8: gap-free prefixes with their links, and no forks.
- Q9: no text past 30 days by the holder's clock.
- Q10: no withdrawn text shown or kept.
- Q11: every missing body has evidence or is marked repairable.
- Q12: no page slot goes on something refused; limit-one pulls with
  withholding on progress until nothing is left to send.

Also checked: exact late flags, identical re-show after a crash before
confirming, and nobody waiting 33 days after a full round.

- Normal run: 12,769 exhaustive plus 1,000 random, 16 s with -race.
- Deep run: 2,784,994 schedules (depth 3) plus 20,000 random,
  251 s; passes.

## Rejected

- Continuing the old stream after a restore: silent loss.
- Ask-and-wait, provisional writes, a fresh identity: they stall, are
  partial, or need re-admission.
- A head file outside the backup: 64 streams instead of 1,920 for 64 machines
  over 30 days (1.2 KB against 27 KB per summary). But a whole-disk restore
  reuses a position, and the new message is lost silently.
- Trusting a sender's "withdrawn"; a time cut-off for "no history"; copy
  records; the newest replacement by history or clock (it depended on
  arrival order).

Design work: send summaries as changes, and retire stream state after 30
days (today it grows without bound).

## Known gaps (skipped tests)

- G1: A never meets C and cannot report C's copy.
- G2: B admits N before seeing C's room-wide post. N gets the text and its
  inbox shows it.
- G3: the branch view misses replies below a reply that is not held.
- G4: a member that omits a stream's tail and lies in its summary goes
  unseen by a machine that pulls only from it.
- G5: dated copy reports outlive a holder restored to before the copy.

## Limits for users

- Machines never awake together exchange nothing (unless a third relays).
- Away over 30 days, you learn what expired, not what it said.
- If every copy of a text is lost, readers wait (told why) until it expires.
- After a restore:
  - Messages may be shown again, unless a handled report has come back.
  - A read message may show as expired.
  - Writing before catching up can deliver older messages late.
- A crash before confirming re-shows messages. Copy reports can be stale.
- Text expired here stays expired, even if the clock is set back.

## Requirements to revise

D3 and D4 need the restore and crash exceptions; D4 needs the late-flag
sentence. D5: copies are known only from direct contact, and handled
requires having been shown. D7 needs the clock rule and headers kept past 30 days.
Section 6 "two copies" is a status line, not a stopping rule. T3 leaks
(G2). D10 branches need ancestor ids.

## Record

Four kinds (message, handled, withdraw, replace). Fields: author, epoch,
seq, prev, links, version, kind, agent, to, ask-handled, ref, parent,
thread, created, body hash, signature, room.

## Requirement to test

The property tests (`TestEverySmallSchedule…`, `TestRandomSchedules…`) cover D2 to D7, D9 and U3. Named scenarios:

| Req | Tests |
|---|---|
| D1 | `TestLostReplyRetryAndRestarts…` |
| D2, D6 | `TestRelay…`, `TestDiskLost…`, `TestSwitchingSources…`, `TestARefusedStreamDoesNotStarve…`, `TestTextThePullerWillRefuse…` |
| D3 | `TestLostReply…`, `TestAuthorRestored…`, `TestContinuingTheOldStream…`, `TestReaderRestored…` |
| D4 | `TestRestoredAuthorThatCatchesUp…`, `TestRestartAfterFetchingBothBranches…`, `TestLateMeans…` |
| D5 | `TestOnlyAnAgentShownTheText…`, `TestHandledReports…`, `TestTwoMachinesNeverAwake…` |
| D7 | `TestAwayThirtyOneDays…`, `TestExpiryWithDifferingClocks…`, `TestTextExpiredHereStaysExpired…` |
| D8 | `TestRelayThatAltersARecordIsRefused`, `TestRelayThatStripsText…` |
| D9 | `TestWithdrawAndReplace…`, `TestWithdrawArrivingBefore…`, `TestReplacementWinnerIsTheSame…` |
| D10 | `TestThreadsBranches…` |
| U3, T3 | `TestNewMachineInboxStartsEmpty…`, `TestReportHeldOnlyByANoHistoryNewcomer…` |
