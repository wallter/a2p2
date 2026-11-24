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
5. Copies are learned only from summaries, as dated reports. The highest
   position a peer reports for a stream is a hint that shows a gap: a
   higher report from any peer raises it, and the peer that set it lowers
   it by reporting less (it was restored), so a retracted report does not
   leave a gap that cannot close (changed 2026-11-24, with build step 5a).
6. A header is held without text only on the holder's evidence: a signed
   withdraw, no history, or its own clock past 30 days (never undone), in
   that order: text from before a machine's admission is "not admitted"
   however old it is, so a reader is told "before you joined", not
   "expired" (order changed 2026-11-24, with build step 5a).
   Otherwise the text is "missing here": readers wait, any holder repairs.
7. A replace takes one above the highest version held, text or not; the
   highest wins, then the highest id. A withdraw of a non-message does
   nothing. Writers refuse empty text and room-wide posts naming
   addressees. Inbox: expired
   before shown: "expired"; withdrawn: nothing; missing: wait. Notices go
   only to agents who saw the original. Late: the agent confirmed a record
   of its author that this one does not follow. Only Seen moves the cursor,
   and only for an entry a full inbox read listed (its receipt: key and
   version), so peek, recent, skipped and thread never satisfy it. Only an
   agent shown the text may report handled; any read that showed the text
   counts (inbox, thread or branch, recent, skipped), within the addressee
   rule, and moves no cursor. Every view orders messages by created, then
   author, then position. The branch under a message not held, or held as
   a header, lists the replies naming it, the parent marked missing (D10).
8. A newcomer reads from the admitter's positions; without history, earlier
   records come as headers only.

## Reading levels (added 2026-11-24, D-levels; revised by D-reading)

9. Each agent has a level per room, kept on its own machine: `all`
   (default, the rules above unchanged), `addressed` (messages whose `to`
   names it, and room-wide posts whose text begins with `@everyone`:
   ASCII, any case, then whitespace, punctuation or the end) or `none` (no
   message). `ask_handled` does not affect levels. While the text is
   missing the mark is unknown and the walk waits. A message the level
   keeps out is passed like one not meant for the agent, unless a full
   inbox read listed it (a receipt): that one is never kept out or
   skipped. Notices about messages it was shown are not filtered.
   A level change marks skipped every held message, after the agent's
   start and not confirmed or listed, that the old level kept out and the
   new one lets in; skipped messages stay out of the inbox. `Skipped`
   lists what the level keeps out (now or by a skip), with text here or
   expired, marking nothing.
   `TestRandomLevelChangesNeitherLoseNorRepeatAMessage`: every message for
   the agent is shown once or listed skipped, never both.

## Attachments (0.2 draft)

Added 2026-11-24 for requirement L1 and revised twice the same day after review;
not built (`attach.go`). The signed header of a message or a replacement
with files holds only the SHA-256 of a salted manifest (`Record.Manifest`).
The manifest (salt; name, size and hash of each file) and the bytes are
content: they travel apart, on request, under the text's permission.

- F1 Referenced: a machine keeps a manifest or a file for a message only
  while the winning version by header commits to that manifest, or lists
  the file in it, and that version's text is one the machine may keep.
  Otherwise bytes, partials and agents' copies (all scoped to the message)
  go in the step that ended the reference. While a replacement's manifest
  is on its way, what is held for the message stays, unserved, until it
  arrives, and only while an earlier version that lists it may be kept.
- F2 Served: only to a peer the sender sees as a member now that may read
  that version's text in its view, and only bytes held for that message.
  Checked at every request and chunk, in both directions.
- F3 Verified: the declared total must equal the size the manifest gives
  (a manifest: at most 16 KiB) before any byte is taken; a partial is
  hidden; the whole must match. Permission and "shown this version" are
  checked again before a copy is written for an agent.
- F4 Asked: manifests are fetched automatically by a machine that may read
  the text; file bytes only when an agent shown that version asks, or the
  machine carries or keeps them.
- F5 Told: here, partial, coming, not asked, fetchable, unavailable
  (dated answers per message, "gave up" included), list pending, list
  unavailable, list invalid, or gone with the reason.
- F6 Confirmed: the bytes staged when the agent asked are what is
  approved, after their digests are checked again; declined, expired
  after a day or failed, nothing is written and staging goes.
- F7 Budget: held, partial (at its declared size), staged and copied bytes
  count; over budget or over the size limit is refused before a byte
  moves.
- F8 Replace: a replacement lists only files its writer lists again, from
  verified bytes held for the version replaced; approval checks that this
  version still wins, is readable and lists them, and staging goes when
  it does not.
- F9 Carry and want: a machine wants a message's files only in its answer
  to a pull from that message's author, when it cannot dial the author.
  A want is bound to message, version and hash, reserves its size, and
  lapses after WantLife or WantTries answers without the bytes; uploads
  are taken only from the author against such a want. A member asked by
  a permitted peer for a file it may keep but lacks answers "not yet" and
  starts one carry job (Carry, the default; FilesOff: not held; Keep:
  wants every file); a lapsed job is answered "gave up".
- F10 Durable storing and cleanup: bytes before rows; a send is written
  only after its bytes, with its rows; deletions queued in the deciding
  step never remove a path a live row names; a restart finishes the queue
  and removes files with no row, temporary copies included.
- F11 Self-void: a machine that learns its own admission was voided drops
  its staged sends.
- F12 Manifest states: one that matches its hash and breaks a rule is
  invalid for good and nothing it lists is fetched; a missing one is asked
  for at most ListTries times, then unavailable until AskList.

Found while modelling. Round 1: with permission by hash alone, a member
admitted without history could list a known hash in its own message and
have an honest member serve it; F2's "held for that message" closes it
(`TestANewReferenceToAKnownHashDoesNotWidenWhoIsServed`). With the
manifest the hash is no longer in the header, so this is now defence in
depth. Round 2: a replacement's header can arrive before its manifest, and
dropping at that moment deleted a file the replacement still lists
(`TestAReplacementListsOnlyTheFilesItsWriterListsAgain` failed); hence the
pending clause of F1. Dropping only when a header arrived, not when a
manifest did, kept a file a replacement no longer lists (seed 86 of the
random test); a manifest's arrival now runs F1. Round 3: the pending
clause had to keep the earlier version's manifest too, or which file it
listed was lost in the same step (order of a map walk); a send's record
storing first could let the deletion queue run between the bytes and
their rows (seed 0); `Decide` now holds deletions until its rows are in.
Wants were first selected by "the puller said it holds", which disclosed
a hash to a no-history former holder and let two carriers wait on each
other; wanting only from the author removes both.

Tests: 37 scenarios in `attach_test.go` and
`TestRandomSchedulesWithFilesKeepTheAttachmentRules` (400 schedules of 100
steps, with random outbound-only machines and modes, withdrawals,
replacements, expiry, admissions without history, removals, lost disks,
wrong bytes and crashes injected at three points). After every step every
held file matches its hash and is kept for a message that lets the machine
keep it and, by the true grants, read it; parts and copies likewise; the
disk is what the rows say; and every transfer so far went to a machine
the sender saw as a member, that the true grants let read the version,
and that could keep it, and every upload came from the author. Removing
the read check from serving, the "asked for" check from uploads, both
author checks on the receiver, the live-row guard of the queue, the bound
on the pending clause, or the invalid state, fails named tests. The
author check on emission, the receiver's two author checks and the
approval re-check (also done when staging is purged) are each redundant
with another check, so removing one alone fails nothing.

Rejected: a new record kind, a body field, or a wire version (0.1.0 refuses
the first two and blocks the author's stream; the third splits mixed
rooms); names, sizes or hashes in the header (they reach machines that may
not read the text, and outlive a withdrawal); the file count in the header
(more than existence, and nothing needs it before the manifest); carrying
files forward on a replacement (it would share them with whoever may read
the replacement without anyone choosing to); version detection for `want`
(0.1.0 ignores it, and only a machine that asked receives an upload);
wants to any machine that said it holds a file (disclosure, and carriers
waiting on each other);
`HEAD` (dated answers to fetches say who holds what); refusing a malformed
reference on receipt (0.1.0 and 0.2 would store different records); bytes
in the database or in pull pages; fetching every file at once (U4, disk),
except where a person sets Keep.

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
- G3: the branch view misses replies below an unheld reply. (Replies
  directly under an unheld message are listed: they name it.)
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
ask-handled, ref, parent, thread, created, body hash, signature, room;
in the 0.2 draft, a manifest hash (L1).

## Requirement to test

Property tests cover D2 to D7, D9, U3, T5 and N1. Every test names its
requirements in a comment. Examples, and the membership tests:

| Req | Tests |
|---|---|
| D1 to D10 | `TestLostReplyRetry…`, `TestRelay…`, `TestAuthorRestored…`, `TestLateMeans…`, `TestAwayThirtyOneDays…`, `TestRelayThatAlters…`, `TestWithdrawAndReplace…` |
| U3, T3 (R2) | `TestNewMachineInboxStartsEmpty…`, `TestNoHistoryNewcomer…`, `TestTwoGrantsThatDiffer…`, `TestHeldResponseIsChecked…` |
| T5 (R1, R2) | `TestMessageOfARemovedAuthor…`, `TestWithdrawWrittenBeforeRemoval…`, `TestRecordWrittenAfterRemoval…`, `TestVoidedAdmission…`, `TestStaleViewHands…`, `TestHeldResponseIsNotReleased…` |
| N1 (R3) | `TestStaleOfferCosts…`, `TestOffersDoNotStarve…`, `TestMachineThatOnlyOpensRequests…` |
| L1 (F1–F12, 0.2 draft) | `TestAFileIsSentOnlyAfterAPersonApproves…`, `TestInARoomWithOneListenerAFile…`, `TestANewcomerWithoutHistoryGetsNeither…`, `TestWithdrawDeletesAFile…`, `TestRandomSchedulesWithFiles…` and the rest of `attach_test.go` |
