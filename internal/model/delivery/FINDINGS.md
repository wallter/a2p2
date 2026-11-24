# Delivery model: findings

## Rules

1. A record is a signed header, which holds the body's hash, plus a body.
2. Each writing start begins a new stream; positions are never reused. A
   stream's first record links to every head of its author's history held.
3. A pull sends the puller's summary (per stream: extent, positions without
   text, texts wanted) and the authors it takes. The server sends wanted
   text, then later records, predecessors first, stopping a stream at
   anything it withholds.
4. A machine stores only the next position of a stream, with matching
   predecessor and links.
5. Copies are learned only from summaries, as dated reports.
6. A header is held without text only on the holder's evidence: a signed
   withdraw, its own clock past 30 days (never undone), or no history.
   Otherwise the text is "missing here": readers wait, any holder repairs.
7. A replace takes one above the highest version held, text or not; the
   highest wins, then the highest id. A withdraw of a non-message does
   nothing. Writers refuse empty text and room-wide posts naming
   addressees. Inbox: expired
   before shown: "expired"; withdrawn: nothing; missing: wait. Notices go
   only to agents who saw the original. Late: the agent confirmed a record
   of its author that this one does not follow. Only Seen moves the cursor;
   only an agent shown the text may report handled.
8. A newcomer reads from the admitter's positions; without history, earlier
   records come as headers only.

## Membership

Machines apply changes (grant, remove, void) when told, in order; a grant
learned late never undoes a removal.

- R1: a machine stores a record if its author is or was a member in its
  view, and only from a peer that is a member now in its view. A removed
  author's records are marked "author removed". A voided author's records
  are dropped; agents shown one get a "voided" notice.
- R2: a server serves only a peer it sees as a member now, checked when
  building a page and again when releasing a held response, and gives text
  only within its view of the peer's permission. Two grants: the more
  restrictive applies.
- R3: a request may carry an offer built on the server's last summary
  (none: no offer); the receiver applies R1 and rule 4.

Found while modelling: a grant learned after its removal re-admitted the
machine; pages spent slots on authors the puller refuses; an offer with no
summary sent everything.

## Properties

Q1 to Q12 hold. New: Q13, no text to or from a machine whose peer knows it was removed;
Q14, no pre-admission text to a no-history machine from a peer that knows
its permission, in responses or offers; Q15, convergence when C only ever
pulls (half the random runs). R1: nothing held by a non-member author.

- Normal run: 10,342 exhaustive plus 1,000 random, 14 s with -race.
- Deep run: 2,987,106 exhaustive plus 20,000 random, 181 s; passes.

## Rejected

- Continuing the old stream after a restore (silent loss); ask-and-wait,
  provisional writes, a fresh identity.
- A head file outside the backup: 64 streams, not 1,920, for 64 machines
  over 30 days, but a whole-disk restore then loses a message silently.
- Trusting a sender's "withdrawn"; time cut-offs for "no history" or for a
  removed author; copy records; replacement order by arrival.

Design work: summaries as changes; retire stream state after 30 days.

## Known gaps (skipped tests)

- G1: A never meets C and cannot report C's copy.
- G2: a no-history newcomer gets room-wide posts its admitter had not seen.
- G3: the branch view misses replies below an unheld reply.
- G4: a member omitting a stream's tail, and lying, goes unseen.
- G5: dated copy reports outlive a holder restored to before the copy.
- G6: anything a removed machine wrote that no member fetched in time (a
  withdraw, say) is lost.
- G7: a narrower grant learned later drops text already shown, silently.

## Limits for users

- Machines never awake together exchange nothing without a relay.
- Away over 30 days, you learn only what expired.
- Text with every copy lost leaves readers waiting, told why, until expiry.
- After a restore: re-shown or expired messages, and late ones if it wrote
  before catching up. A crash before confirming re-shows.
- A member not yet told of a removal still serves the removed machine and
  relays its new records (marked) until it learns.

## Requirements to revise

D3 and D4 need the restore and crash exceptions; D4 the late-flag sentence.
D5: copies are known only from direct contact. D7 needs the clock rule.
Section 6 "two copies" is a status line. T3 leaks (G2). D10 branches need
ancestor ids. T5 holds per member, as written. N1 holds with offers.

Record fields: author, epoch, seq, prev, links, version, kind, agent, to,
ask-handled, ref, parent, thread, created, body hash, signature, room.

## Requirement to test

Property tests cover D2 to D7, D9, U3, T5 and N1. Every test names its
requirements in a comment. Examples, and the membership tests:

| Req | Tests |
|---|---|
| D1 to D10 | `TestLostReplyRetry…`, `TestRelay…`, `TestAuthorRestored…`, `TestLateMeans…`, `TestAwayThirtyOneDays…`, `TestRelayThatAlters…`, `TestWithdrawAndReplace…` |
| U3, T3 (R2) | `TestNewMachineInboxStartsEmpty…`, `TestNoHistoryNewcomer…`, `TestTwoGrantsThatDiffer…`, `TestHeldResponseIsChecked…` |
| T5 (R1, R2) | `TestMessageOfARemovedAuthor…`, `TestWithdrawWrittenBeforeRemoval…`, `TestRecordWrittenAfterRemoval…`, `TestVoidedAdmission…`, `TestStaleViewHands…`, `TestHeldResponseIsNotReleased…` |
| N1 (R3) | `TestStaleOfferCosts…`, `TestOffersDoNotStarve…`, `TestMachineThatOnlyOpensRequests…` |
