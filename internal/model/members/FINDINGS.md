# Membership model: findings

Written 2026-11-23 by the sub-agent that built this package (Claude Opus),
saved by the project lead. The model is `resolve.go` (the pure function),
`machine.go` (what one machine accepts) and the tests beside them.

## Rules as implemented

Revised after an independent review found that a second removal of a
re-appointed admin could empty the room
(`docs/research/decisions/model-members-review-claude46.md`).

1. A room starts with one create entry. Its author is the first admin and has
   no other lasting power.
2. Every entry is judged against what its author had seen. The author must
   have been an admin there, and the change must make sense there.
3. One admin alone can add a machine or remove a non-admin.
4. Making or removing an admin needs matching entries from two different
   admins, unless the signer was the only admin in what it had seen. Removing
   a machine that is being made an admin at the same time also needs two.
5. A machine's vote on its own removal does not count.
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
11. The local layer keeps the first of two entries one key signed at one
    position. It refuses the second and everything that depends on it, and
    keeps and passes on both as evidence. Once the forking key has been
    removed as an admin, a machine accepts both branches and rule 9 decides.
    A settled fork stays settled: what that key signs after being re-admitted
    counts normally.
12. No timers, and no entry kinds beyond create, add, remove and make admin.

## What holds

Deep run (`A2P2_MODEL_DEEP=1`), passed in 28 minutes:

| Search | Size | Holds |
|---|---|---|
| Honest histories, up to 5 entries after create | 109,198 | P1, P2, P4, P5, P6, P7 |
| One-key attacks on those, depth 2 | 44,049,546 | P3 |
| Larger rooms, depth 2 | 35,136 | P3 |
| Seven seeded rooms (73,803 histories) | 32,786,313 attack sets | P1 to P7 |

The seeded rooms include a re-appointed admin removed again, a removal
completed and then voted on by a further admin (four and five admins), a
vote on one's own removal, and a settled fork followed by new entries.

P1 same set, same state. P2 a removal sticks. P3 one key cannot change the
admin set once there are three admins. P4 losing one admin of three is
survivable. P5 nothing is promoted without entries. P6 every entry without
effect has a reason, and no panic. P7 a fully informed change alters only its
target. P3 excludes forks and changes still waiting for a second signature
(gaps 2, 3 and 4). The package's ordinary run takes 11 s under the race
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
- **Settling a fork only on machines that saw the same branch.** The settling
  removal descends from one branch, so machines on the other refuse it.
  Rule 11 closes it.

## Known gaps

1. **A fork by one of two admins never settles**
   (`TestForkInTwoAdminRoomIsNeverSettled`, skipped). A and B are admins; B
   adds D, is restored from backup, and adds E at the same position. A alone
   cannot remove B, so machines that saw D first and those that saw E first
   disagree for ever.
2. **A fork inside a period when the creator was the only admin fools
   machines that missed the honest entry at that position**, until two other
   admins remove the forking key
   (`TestForkInsideSoleAdminPeriodFoolsOnlyMachinesMissingThatEntryUntilSettled`).
   Every machine that already holds the honest entry refuses the fork.
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
   signature the removed admin made before its removal.
5. **A removed key can still remove members it could see**, once per target,
   because its own chain blocks a repeat. It needs a current member willing
   to pass its entries on.

## Limits a user must be told

- Two admins survive a loss but not a theft. Three survive either losing one
  or having one stolen.
- A stolen key can add machines, remove members, and complete changes others
  have started. What it let machines read cannot be recalled.
- A restored admin must catch up before signing. If it signs first, two other
  admins must remove it, and it rejoins under a new key.

## What `docs/REQUIREMENTS.md` should change

- Keep T9 as written (two admins to change the admin set whenever there is
  more than one). A machine's vote on its own removal does not count.
- Say that a removal of an admin, or of a machine being made an admin, waits
  for a second admin and is shown with its reason until then.
- Add gaps 1, 3, 4 and 5 and the restore rule to section 6. Entries carry no
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
| T9 | `TestOneKeyCannotChangeAdminSetOnceThereAreThreeAdmins`, `TestOneKeyCannotChangeAdminSetInLargerRooms`, `TestIdleStolenKeyBackdatingCannotChangeAdminSet`, `TestNoRankAnyTwoAdminsRemoveTheThirdNoneAlone`, `TestCreatorKeyIsRemovableByTwoOtherAdmins`, `TestMutualRemovalWithTwoThreeAndFourAdmins`, `TestOwnVoteOnOwnRemovalDoesNotCount`, `TestSeededRoomsKeepEveryProperty`, the fork tests |
| T5 | `TestRemovedAdminsConcurrentRemovalOfCompromisedMachineStands`, `TestConcurrentAddAndRemoveOfSameMachineRemovalWins`, `TestCascadeFromVoidAdminVoidsItsGrantsKeepsItsRemovals`, `TestRemovedAdminKeepsSigningGrantsVoidRemovalsCountOncePerTarget`, `TestLateVoteOnCompletedRemovalDoesNotUndoReadmission` |
| T10 | `TestAllAdminsLostFreezesMembershipNobodyPromoted`, `TestLossOfAnyOneAdminIsSurvivable`, `TestLostAdminOfThreeIsReplacedByTheOtherTwo`, `TestLostAdminOfTwoLeavesMembersManageableAdminSetFixed` |
| A4 | `TestResolveGivesEveryEntryAReasonOnArbitraryInput` |
| T3 | `TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations` |
| D6 | `TestBackupRestoreForkIsSettledByRemovalOfTheForkingAdmin`, `TestMachinesReceivingSameEntriesInAnyOrderAgree` |
