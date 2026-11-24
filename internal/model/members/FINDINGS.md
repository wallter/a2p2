# Membership model: findings

Written 2026-11-23 by the sub-agent that built this package (Claude Opus),
saved by the project lead. The model is `resolve.go` (the pure function),
`machine.go` (what one machine accepts) and the tests beside them.

## Rules as implemented

Revised after an independent review found that a second removal of a
re-appointed admin could empty the room
(`docs/research/decisions/model-members-review-claude46.md`).

1. A room starts with one create entry. Its author is the first admin and has
   no other lasting power. A machine is pinned to its room's create and
   refuses any other; `ResolveRoom(root, entries)` gives every other create,
   and what depends on it, no effect with a reason instead of an empty room
   (`TestSecondCreateIsRefusedAndDoesNotEmptyTheRoom`). `Resolve` without a
   root still finds no room in a set with several creates.
2. Every entry is judged against what its author had seen. The author must
   have been an admin there, and the change must make sense there.
3. One admin alone can add a machine or remove a non-admin.
4. Making or removing an admin needs matching entries from two different
   admins, unless the signer was the only admin in what it had seen. Removing
   a machine that is being made an admin at the same time also needs two.
5. A machine's vote on its own removal does not count. A fork it signed
   does (rule 12).
6. Removals that count are never cancelled. Simultaneous mutual removals that
   each have two signatures both stand.
7. A removal is one event. An entry has seen it once it has seen enough votes
   to complete it.
8. A machine is a member if some add of it had seen every removal of it that
   counts. So a removal beats a concurrent add, and an add made knowing of
   the removal brings the machine back.
9. A removal, once complete, is closed. A removed admin's adds and admin
   grants stand only if one of its removers had seen them take effect. Votes
   that had already seen the removal complete change nothing. A further
   vote made without knowledge of the completed removal can only add to what
   stands, never take away. A grant vote that was still waiting for a second
   admin when its signer was removed cannot be revived. A grant voided once
   stays void. The removed admin's own removals still count. Entries carry
   no cut-off field.
10. A grant counts only if its author's admin role came from grants that
    counted. Removals by such an author still count.
11. The local layer checks an entry's chain (its previous entry is its
    author's, one position back) before reserving its position, so a
    malformed entry reserves nothing, is not passed on and is never fork
    evidence (`TestMalformedChainEntryReservesNoPosition`). It keeps the
    first of two valid entries one key signed at one position. It refuses the second and everything that depends on it, and
    keeps and passes on both as evidence. Once the forking key has been
    removed as an admin, a machine accepts both branches and rule 9 decides.
    A settled fork stays settled: what that key signs after being re-admitted
    counts normally. A fork by a key that was not an admin at either entry
    had no effect, and machines accept both branches at once. Each fork
    settles on its own: an open fork by another key holds back only the
    entries that depend on its refused branch
    (`TestOneOpenForkDoesNotHoldBackSettlingAnother`). An admin signs a
    settling removal naming as parents only entries it has accepted, so its
    branch stands (`TestSettlingRemovalNamesOnlyAcceptedEntries`). A
    removal that names one branch of another open fork waits on machines
    holding that fork's other branch until it settles
    (`TestSettlingRemovalThatNamesAnotherOpenForkWaitsForIt`). A machine
    offers accepted entries in causal order, so replaying them into a fresh
    machine holds nothing (`TestOfferReplaysIntoAFreshMachineWithNothingHeld`).
12. Fork evidence is a vote. Two different entries one key signed at one
    position, at least one while it was an admin, count as that key's vote
    for its own removal as an admin, in the period the fork was signed. The
    evidence is part of the set `Resolve` sees, so P1 is unaffected: no
    separate input. One other admin's removal vote then completes the
    removal; evidence alone removes nobody, and the forking key cannot
    complete it alone, since its own removal vote does not count. What
    stands follows rule 9: the branch the remover had seen take effect
    stands, the other does not; a remover that had seen both keeps neither.
    Removals the forking key signed from the fork position on, which no
    remover had seen take effect, have no effect. The forking machine
    rejoins under a new key.
13. `State.Admitted` lists every machine that has ever had an effective
    admission in the set: the creator and every target of an add or admin
    grant that took effect, even if later undone. Every member is in it.
    It is not monotone: more entries can show that an admission never took
    effect, so a machine can leave it (`TestAdmittedCanShrinkWhenMoreEntriesArrive`).
    Smallest cases: A adds B, then A's restored backup re-signs that
    position (the fork voids the add); or A adds and removes C while B,
    having seen neither, adds and removes C (B's removal beats A's
    concurrent add). A store must not drop a machine's records only
    because it left `Admitted`.
14. No timers, and no entry kinds beyond create, add, remove and make admin.

## What holds

Deep run (`A2P2_MODEL_DEEP=1`), passed in 28.4 minutes (attackers may also sign a second create):

| Search | Size | Holds |
|---|---|---|
| Honest histories, up to 5 entries after create | 109,198 | P1, P2, P4, P5, P6, P7, Admitted |
| One-key attacks on those, depth 2 | 46,002,156 | P3 |
| Larger rooms, depth 2 | 36,334 | P3 |
| One-key attacks with forks: every room 1 deep, rooms up to 2 entries 2 deep | 2,249,595 (1,584,972 with a fork) | P1, P3, settlement |
| The same from four seeded rooms of four and five admins, 1 deep | 2,802 (2,190 with a fork) | P1, P3, settlement |
| Seven seeded rooms (71,461 histories): 1 deep from every continuation, 2 deep from the seed | 12,468,259 attack sets | P1 to P7, Admitted on the histories |

Covered before being stopped, with the same rules: fork attacks two deep
from rooms up to 3 entries after create (3,759,132 sets, 2,980,614 with a
fork, passed), and seeded attacks two deep from the first continuation step
in five of the seven seeds (5,557,035 sets, passed). Before rule 12 the
seeded search at that depth passed on all seven (32,786,313 sets).

The seeded rooms include a re-appointed admin removed again, a removal
completed and then voted on by a further admin (four and five admins), a
vote on one's own removal, and a settled fork followed by new entries.

P1 same set, same state. P2 a removal sticks. P3 one key cannot change the
admin set once there are three admins. P4 losing one admin of three is
survivable. P5 nothing is promoted without entries. P6 every entry without
effect has a reason, and no panic. P7 a fully informed change alters only its
target. P3 with forks: a key that also re-signs positions it has used cannot
change the admin set on a machine holding the room, nor after another
admin settles the fork, and every such set resolves the same in any order.
P3 excludes changes still waiting for a second signature
(gaps 3 and 4), and an unsettled fork, which counts as the forking key's
signed vote. The package's ordinary run takes 11 s under the race
detector.

## What the search rejected

- **A threshold of one signature while a room has two admins.** Smallest
  case: A makes D an admin; D makes C an admin; A's key, signing as if it had
  not seen C's promotion, makes X an admin alone. In the idle-key variant the
  thief removes A, the cut-off voids A's promotion of C, and the thief holds
  the room.
- **Judging a removal's target only in the author's own past.** A stale
  stolen key could remove a newly promoted admin as if it were a plain
  member. Rule 4 closes it.
- **Cut-offs named by the removers.** Two admins naming an empty cut-off
  voided the grants their own authority rested on, and the room emptied.
  Rule 9 uses what the removers had seen instead.
- **A removal counted vote by vote.** One key's late vote on an already
  completed removal undid a re-admission, and in a second form brought back
  an addition the removal had voided. Rules 7 and 9 close both.
- **Letting any remover that had seen a forking key's removal vote keep
  it.** A later vote that had seen the removal complete revived it. Rule
  12 keeps only what a remover had seen take effect.
- **Identifying a removal by how the entry's past classified it.** A
  backup's concurrent promotion turned an earlier member removal into an
  admin removal, and later grants lost track of it
  (`TestForkTurningAnEarlierRemovalIntoAnAdminRemovalKeepsLaterGrants`).
- **Settling a fork only on machines that saw the same branch.** The settling
  removal descends from one branch, so machines on the other refuse it.
  Rule 11 closes it.

## Known gaps

1. **Closed by rule 12: a fork by one of two admins now settles.** A
   alone, holding both branches, removes B, and machines that saw different
   branches agree (`TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone`).
2. **A fork inside a period when the creator was the only admin fools
   machines that missed the honest entry at that position**, until another
   admin removes the forking key (one is enough under rule 12)
   (`TestForkInsideSoleAdminPeriodFoolsOnlyMachinesMissingThatEntryUntilSettled`).
   Every machine that already holds the honest entry refuses the fork.
   Rule 12 does not help a room whose only admin forks: no other admin can
   vote, so the fork never settles. With one admin nobody else can change
   membership anyway.
3. **A stolen key can complete a change another admin started**, though
   never one aimed at itself
   (`TestStolenKeyCompletingAPendingRemovalDecidesTheTargetsUnseenEntries`,
   `TestStolenKeyCanCompleteAPromotionBeatenByAConcurrentRemoval`). When it completes
   a removal it decides whether the removed admin's grants that the first
   signer had not seen stand. It can also complete a promotion that a
   concurrent removal had beaten, which leaves the removal waiting for a
   second admin.
4. **A stolen key can second a removed admin's voided promotion vote and
   then join the removal**
   (`TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval`,
   skipped). Stolen key D seconds C's vote, claiming not to have seen C's
   removal, then votes for the removal. The same entries arise honestly (D
   seconded in good faith, A and D removed C, a late vote arrives), and there
   the promotion must stand, so no rule can refuse it. Like gap 3 it needs a
   signature the removed admin made before its removal. The same holds for
   a forking key's removal vote voided by rule 12: a stolen key seconds it
   and joins the removal having seen it take effect
   (`TestStolenKeyCanSecondAForkingKeysVoidedRemovalAndThenJoinTheRemoval`,
   skipped).
5. **A removed key can still remove members it could see**, once per target,
   because its own chain blocks a repeat. It needs a current member willing
   to pass its entries on.

## Limits a user must be told

- Two admins survive a loss but not a theft. Three survive either losing one
  or having one stolen.
- A stolen key can add machines, remove members, and complete changes others
  have started. What it let machines read cannot be recalled.
- A restored admin must catch up before signing. If it signs first, one
  other admin removes it, and it rejoins under a new key. Entries the
  remover had not seen on the restored machine's other branch are lost.

## What `docs/REQUIREMENTS.md` should change

- Keep T9 as written (two admins to change the admin set whenever there is
  more than one). A machine's vote on its own removal does not count.
- Say that a removal of an admin, or of a machine being made an admin, waits
  for a second admin and is shown with its reason until then.
- Add gaps 2, 3, 4 and 5, rule 12 and the restore rule to section 6. Entries carry no
  cut-off.
- The design must say that a machine sends accepted entries in causal order,
  fork evidence after them, and follows rule 11.

## "The first three machines are admins"

Workable. For the second admin a person confirms on the creator's machine and
on the joining machine. For the third, a person confirms on both admin
machines and on the joining machine; a daemon that co-signed automatically
would let one stolen key act as two. Until the third admin exists the room
has the two-admin weaknesses above.

## Requirement to test

| Requirement | Tests |
|---|---|
| T9 | `TestOneKeyCannotChangeAdminSetOnceThereAreThreeAdmins`, `TestOneKeyCannotChangeAdminSetInLargerRooms`, `TestIdleStolenKeyBackdatingCannotChangeAdminSet`, `TestNoRankAnyTwoAdminsRemoveTheThirdNoneAlone`, `TestCreatorKeyIsRemovableByTwoOtherAdmins`, `TestMutualRemovalWithTwoThreeAndFourAdmins`, `TestOwnVoteOnOwnRemovalDoesNotCount`, `TestSeededRoomsKeepEveryProperty`, `TestOneKeyCannotChangeAdminSetByForkingItsOwnChain`, `TestForkEvidenceNeedsTwoEntriesByOneKeyAtOnePosition`, the fork tests |
| T5 | `TestRemovedAdminsConcurrentRemovalOfCompromisedMachineStands`, `TestConcurrentAddAndRemoveOfSameMachineRemovalWins`, `TestCascadeFromVoidAdminVoidsItsGrantsKeepsItsRemovals`, `TestRemovedAdminKeepsSigningGrantsVoidRemovalsCountOncePerTarget`, `TestLateVoteOnCompletedRemovalDoesNotUndoReadmission` |
| T10 | `TestAllAdminsLostFreezesMembershipNobodyPromoted`, `TestLossOfAnyOneAdminIsSurvivable`, `TestLostAdminOfThreeIsReplacedByTheOtherTwo`, `TestLostAdminOfTwoLeavesMembersManageableAdminSetFixed` |
| A4 | `TestResolveGivesEveryEntryAReasonOnArbitraryInput` |
| T3 | `TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations` |
| D6 | `TestBackupRestoreForkIsSettledByRemovalOfTheForkingAdmin`, `TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone`, `TestForkSettledByOneRemoverKeepsTheBranchItHadSeen`, `TestForkByMemberWhoIsNotAnAdminChangesNothing`, `TestMachinesReceivingSameEntriesInAnyOrderAgree` |
