# Testing

Run `make check` before submitting changes. It checks Go formatting, runs
`go vet`, builds and vets for Windows (`make windows`), checks the direct
dependencies against an allow-list (`make licences`), checks the
[requirement register](TRACE.md), and runs the Go test suite with the race
detector. `make trace` checks just the register against
the [requirements](REQUIREMENTS.md) and the named tests. Add a fault matrix
row when you add a test for a delivery, ordering, security or compatibility
guarantee.

Keep tests in temporary directories so they leave your a2p2 state and harness
configuration alone. Use controlled clocks and seeded randomness for tests
that need to replay the same steps.

## What the suite covers

| Where | What it checks |
|---|---|
| [`internal/members`](../internal/members), [`internal/model/delivery`](../internal/model/delivery) | Membership and delivery rules over bounded searches and seeded random schedules. |
| [`internal/core`](../internal/core) | Signed records, log entries and invite tokens, including fixed test vectors and fuzz seeds. |
| [`internal/store`](../internal/store) | Transaction rollback, reopening a database and migrations. |
| [`internal/node`](../internal/node) | Pulls, joins and membership changes over an in-memory transport that can inject faults. Differential tests compare the implementation with the delivery model after each step. |
| [`internal/peer`](../internal/peer) | Mutual TLS on loopback, joins, access checks, request limits, waiting pulls and address fallback. |
| [`internal/localapi`](../internal/localapi), [`internal/mcpshim`](../internal/mcpshim), [`internal/web`](../internal/web), [`internal/daemon`](../internal/daemon) | Unix-socket API, MCP calls, web server checks and integration with two daemons. |
| [`internal/harness`](../internal/harness) | Install and uninstall preserve the settings exercised by the tests. Hook tests check the output sent to the harness. |
| [`scripts/recipes.sh`](../scripts/recipes.sh) (`make recipes`, in `make check`) | Every tool, tool argument and CLI command named in [`recipes/`](../recipes) and [`skills/`](../skills) exists in the golden tools file and `a2p2 help`, and each recipe and the skill stay short. |

## Fault matrix

Rows marked "model" include tests of the rules in `internal/members` or
`internal/model/delivery`. Tests without that label exercise the implementation.

| Fault | What happens | Test |
|---|---|---|
| Lost response | The retry gives one copy in order | `TestRealExchangeAgreesWithTheDeliveryModelOnEveryShortSchedule`, model `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Author restarts | The author writes to a new stream linked to every head it holds | `TestEveryStartWritesANewStreamLinkedToEveryHeadHeld`, `TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules` |
| Relay restarts | Its database gives the same answers, and it serves again | `TestReopeningTheDatabaseGivesTheSameAnswers`, `TestARestoredServerPagesTheLogFromAMemberThenServesANewPeer` |
| Reader restarts between showing and confirming | The entries appear again. An existing handled report stays single. | `TestCrashBetweenShowAndSeenShowsTheEntriesAgain`, model `TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped` |
| Restore from an older database copy | One resend repairs a few missing entries. By default a restored admin must pull the log before signing. | `TestAServerRestoredWithFewEntriesMissingIsHealedByOneResend`, `TestARestoredDaemonSignsNothingForARoomUntilItHasPulledItsLog`, `TestConfirmationOfAnEntryLostToARestoreIsRefused` |
| Restored author whose newer holders are all asleep | A new stream is started without a fork | model `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork` |
| Old backup knows no current member | A new invite from a current admin lets the restored machine catch up | `TestJoinRestoredMachineThatKnowsNoCurrentMemberCatchesUp` |
| Two machines never awake together | Messages and handled reports pass through a third member that took a copy | `TestRelayCarriesAMessageAndItsHandledReportBetweenAnAuthorAndAReaderNeverAwakeTogether`, `TestTwoMachinesNeverAwakeTogetherExchangeMessagesThroughOffersToARelay` |
| Relay alters or strips text | The reader refuses altered text and can recover missing text from another copy | `TestARelayThatAltersOrStripsTextIsNotBelieved`, `TestRelayCannotAlterAnySignedField`, `TestRelayMayDropTextAndTheHeaderStillVerifies` |
| Text expires at different times on different machines | Each machine uses its own clock. Expired text stays expired. | `TestExpiredTextStaysExpiredWhenTheClockGoesBackOrTextIsOfferedAgain`, model `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects` |
| Away longer than 30 days | The machine is told what is missing, then continues | model `TestAwayThirtyOneDaysIsToldWhatIsMissingThenContinues` |
| Machine added without history | Text before the grant's history boundary stays hidden, including in offers | `TestANewcomerWithoutHistoryIsNeverGivenEarlierTextIncludingThroughAnOffer`, `TestMachineAddedWithoutHistorySeesNoEarlierTextThroughAnySurface` |
| Removed machine makes a request | Refused with the same bytes as other non-member requests | `TestARemovedMachineIsRefused`, `TestTheThreeNotMemberRefusalsAreTheSameBytes` |
| Machine removed while waiting for a response | The server withholds the held response | `TestAHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited` |
| Late messages from a removed machine | They can arrive marked as from a removed machine | `TestRemovalOfAMemberByOneAdminKeepsItsRecordsMarked`, `TestRemovedMachineMarksItsOwnRecordsAndGrantsOverTheHeadLimitAreRefused`, model `TestMessageOfARemovedAuthorReachesAMachineThatNeverMetItMarked` |
| Leave and rejoin | An old invite cannot undo a leave. Responses from before the leave are dropped. | `TestJoinOldInviteCannotUndoALeave`, `TestLeaveThenALateResponseIsNotApplied`, `TestALeaveAndRejoinBetweenBuildAndReleaseReleasesNothingOfTheOldGeneration`, `TestJoinAgainAfterLeavingLocallySignsNoNewGrant` |
| Stolen admin key | Two other admins can remove it. The stolen key alone cannot change the admin set once there are three admins. | model `TestOneKeyCannotChangeAdminSetOnceThereAreThreeAdmins`, model `TestCreatorKeyIsRemovableByTwoOtherAdmins`, model `TestStolenKeyCompletingAPendingRemovalDecidesTheTargetsUnseenEntries` |
| Forking admin key | Machines holding the real entry refuse the fork. Another admin can settle it by removing the key. | `TestForkInTwoAdminRoomIsSettledByTheOtherAdminsRemoval`, model `TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone`, model `TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember` |
| Lost admin | Two of three can replace it. A sole remaining admin can still add or remove members. With no admins left, membership stays fixed. | model `TestLostAdminOfThreeIsReplacedByTheOtherTwo`, model `TestLostAdminOfTwoLeavesMembersManageableAdminSetFixed`, model `TestAllAdminsLostFreezesMembershipNobodyPromoted` |
| Invite used by a second key | Refused | `TestJoinSecretPresentedByASecondKeyIsRefused` |
| Invite expired | Refused before anything is signed | `TestJoinExpiresBeforeSigning`, `TestJoinExpiryAtWriteTimeAndConcurrentAccepts` |
| Invite used after a leave | See leave and rejoin | `TestJoinOldInviteCannotUndoALeave` |
| Join declined by the admin's person | Refused on the joiner | `TestJoinDeclinedByTheAdminsPersonIsRefusedOnTheJoiner` |
| Wrong server key at join | The joiner stops before the secret leaves | `TestJoinWrongServerKeyStopsTheJoinerBeforeTheSecretLeaves`, `TestJoinOverTLSToTheWrongKeySendsNothing` |
| Confirming before the admin has caught up | Needs an explicit override, which is recorded | `TestConfirmBeforeCatchingUpNeedsTheExplicitOverrideAndRecordsIt` |
| Machine cannot accept connections | It converges through its own requests | `TestAMachineThatOnlyOpensRequestsConverges`, `TestOverTLSAMachineWithNoListenerConvergesThroughItsOwnRequests` |
| Dead first address | The second address is reached | `TestOverTLSAPeerWithADeadFirstAddressIsReachedOnItsSecond`, `TestOverTLSAStalledFirstAddressGivesTheNextAttemptToTheSecond` |
| Oversized request | 413 before the body is read | `TestOverTLSAnOversizedBodyGets413BeforeItIsRead`, `TestJoinRouteRefusesOversizeBodies` |
| Malformed request | 400 for invalid or non-strict JSON | `TestOverTLSMalformedOrNonStrictJSONGets400`, `TestPullRequestsAreCheckedAtEveryLevel` |
| Response that does not fit its request | Refused before anything is applied | `TestAResponseThatDoesNotFitItsRequestIsRefusedBeforeAnythingIsApplied` |
| Too many requests | 429 with `Retry-After` | `TestOverTLSTheRateLimitGives429WithRetryAfter` |
| Peer that answers every wait at once | It is backed off | `TestAPeerThatAnswersEveryWaitAtOnceIsBackedOff` |
| Stranger connects | The handshake fails and its request is refused | `TestOverTLSAStrangersHandshakeFailsAndItsRequestIsRefused` |
| Networking off | The daemon makes no peer connections until a person enables networking | `TestNetworkingOffDialsNothingAndOutboundDials`, `TestNetworkingIsOffUntilAPersonsDecisionIsRecorded` |
| Stop arrives while a networking decision is being applied | The daemon still stops | `TestStopRightAfterANetworkingDecisionStillStopsTheDaemon` |
| Networking on, but the listener cannot bind | The daemon runs outbound only; `status` shows `applied: outbound` with the error as `reason`, and a `network` alert is raised | `TestANetworkingDecisionIsAppliedByTheDaemonItself` |
| A question to the person is replaced after it was read | It is a question again, with its new text | `TestAReplacementOfAQuestionIsAQuestion`, `TestAlertsListQuestionsWaitingForThePerson` |
| Every other member keeps refusing this machine | After ten minutes the room is shown as perhaps removed and a `removed` alert is raised; one member still answering, a shorter time, or `local` is not | `TestARoomEveryOtherMemberKeepsRefusingMayHaveRemovedThisMachine` |
| Reply in a thread with several participants | The reply tool reaches the parent's author and everyone who posted in the thread, across machines, at most 32; a plain `send` with `reply_to` and no `to` still reaches only the parent's author | `TestReplyReachesEveryoneWhoPostedInTheThreadAcrossMachines`, `TestSurfaceThreadReplyAddresseesAreCappedWithTheParentsAuthorKept`, `TestSendWithReplyToAndNoToStillAddressesOnlyTheParentsAuthor` |
| `ack` that is not allowed | Refused with the reason; the rule itself is unchanged | `TestAckRefusalsSayWhy`, `TestSurfaceAckRefusalsSayWhyAndTheRuleIsUnchanged` |
| `ack` of a message already reported, or after `reply` reported it | Succeeds without a second report and names the first; the CLI says "already reported handled" | `TestAckAfterReplySucceedsAndSaysItWasAlreadyReported`, `TestAckAlreadyReportedSaysSoAndNamesTheFirstReport` |
| Any refusal of the local API or a tool | A sentence, a stable code and whether retrying can help | `TestEveryRefusalCarriesAStableCodeAndWhetherRetryingCanHelp`, `TestToolErrorsAreOneObjectWithACodeAndRetryable` |
| Inbox wait longer than allowed, or cancelled | The tool caps the wait at 30 seconds and says when it ended empty; a cancelled wait marks nothing seen | `TestInboxWaitIsCappedSaysWhenItEndedEmptyAndACancelledWaitMarksNothing` |
| Thread read with `after` | Only what follows that message | `TestThreadReadAfterAMessageReturnsOnlyWhatFollows` |
| Text with `<`, `>` or `&` | Written as it is in tool results, API responses and `--json` output | `TestToolResultsDoNotEscapeHTML`, `TestResponsesWriteAngleBracketsAndAmpersandsAsWritten`, `TestJSONOutputWritesAngleBracketsAndAmpersandsAsWritten` |
| A harness opens the MCP server with an old, current or unknown revision | The built binary negotiates a revision it supports | `TestTheBuiltShimNegotiatesEachSupportedMCPRevision` |
| An agent writes as if it were the person | Its record never answers the person's question or confirms a request | `TestAnAgentsRecordNeverCountsAsThePersonsAnswerOrConfirmation` |
| Message text built to look like a notice, JSON or a command | The hook's output never contains it | `TestHookOutputNeverContainsMessageTextWhateverItSays` |
| Install, install again, then uninstall | Every file and directory of the home is as it was, including Codex's tool approval | `TestUninstallLeavesTheWholeHomeAsItWasBeforeInstall`, `TestCodexInstallApprovesOnlyItsOwnServerAndUninstallRemovesIt` |
| OpenCode, Hermes or Pi installed, installed again, then uninstalled | Every file and directory of the home is as it was, also after OpenCode adds `$schema` to the file a2p2 created | `TestFileAdapterUninstallLeavesTheWholeHomeAsItWasBeforeInstall`, `TestFileAdapterDryRunAndMissingCLI` |
| The harness's config file holds other servers, comments or a layout a2p2 cannot edit | Only the `a2p2` entry changes; a file a2p2 cannot edit safely, or a skill it did not write, is refused untouched | `TestOpenCodeInstallSetsOnlyItsOwnServer`, `TestHermesInstallEditsOnlyItsEntryInConfigYAML`, `TestPiInstallWritesOnlyItsSkill`, `TestDoctorChecksTheFileAdapters` |
| Any of the six installed over the person's own entry named `a2p2`, the file edited by the person, then uninstalled | The person's entry is back, their later edits stay, the record is gone, and a backup of a changed file is kept | `TestUninstallPutsBackTheEntryNamedA2P2ThatInstallReplaced` |
| Uninstall with no install record | Nothing is removed and no harness CLI runs; the report says so | `TestUninstallWithoutARecordRemovesNothing` |
| Hermes `config.yaml` with CRLF, no final newline, a comment after `mcp_servers:`, `mcp_servers: {}`, flow style, tabs, a list or scalar document | Either a correct edit that parses back with every original line, and uninstall restores the exact bytes, or a refusal with nothing written | `TestHermesEditNeverTurnsAValidFileIntoAnInvalidOne` |
| `opencode.json` with comments or trailing commas, not an object; a `SKILL.md` a2p2 did not write | Refused, nothing written and no record saved; `opencode.jsonc` is never edited | `TestOpenCodeAndPiRefuseWhatTheyCannotEditExactly` |
| A config path that is a symlink, dangling or to an existing file | Dangling: refused before anything is written or run; existing: the target is edited and the link stays, for all six | `TestConfigSymlinksAreFollowedAndDanglingOnesRefused` |
| Hook or MCP registration removed by hand | `doctor` says which one is missing | `TestDoctorSaysWhetherTheHookOrTheMCPRegistrationIsMissing` |
| A CLI command run under a harness agent's label | The agent's hook keeps working | `TestACLICallAsAHarnessAgentKeepsItsHookWorking` |
| `stop` during a waiting call or a held pull | The call ends before the store closes; the held pull is released at once | `TestStopDuringAWaitingCallEndsItBeforeTheStoreCloses`, `TestStopReleasesAHeldPullAtOnce` |
| Page request with wrong `Origin` or `Host`, or no token | Refused | `TestAuthHostOrigin`, `TestPageDecisionNeedsTheExactOriginAndShowsTheFullFingerprint` |
| Message text that looks like instructions | Shown as labelled peer data | `TestHostileStringsAreInert`, `TestInboxForgedHeadersStayInert` |
| Database write fails partway | The store rolls back the write or log batch | `TestFailureBetweenStatementsLeavesNoHalfAppliedWrite`, `TestLogBatchThatFailsLeavesNothingApplied` |
| Power loss | Open test gap | None |
| Clock set back or ahead | Expired text stays expired. Replacement order follows the author's history. A record dated over a day ahead waits. | `TestTextThatExpiredBeforeItWasShownIsAnExpiredNoticeEvenAfterTheClockGoesBack`, `TestAFutureRecordLeavesNoSeenAndIsTakenOnceTheClockAllows`, model `TestNewestReplacementIsByTheAuthorsHistoryNotItsClock` |
| Agent at level `addressed` | Shown what names it, replies in its threads and posts whose text begins with `@everyone`; other room-wide posts are skipped; another agent on the machine is unaffected | `TestAgentAtAddressedSeesWhatIsAddressedToItAndRepliesInItsThreadsButNotRoomWidePosts`, model `TestAddressedLevelShowsWhatIsAddressedAndPostsMarkedForEveryone` |
| Agent at level `none` | Nothing reaches its inbox; it reads what it skipped when it asks, and asking marks nothing | `TestAgentAtNoneIsShownNothingUntilItAsks`, model `TestNoneLevelShowsNothingUntilTheAgentAsks` |
| Level raised | What was skipped stays out of the inbox and is readable apart, unmarked; the inbox continues from now | `TestRaisingTheLevelShowsWhatWasSkippedWithoutMarkingItSeenAndTheInboxContinuesFromNow`, model `TestRaisingTheLevelKeepsWhatWasSkippedOutOfTheInboxAndReadableApart` |
| Counts and first lines asked for | They follow the level and mark nothing | `TestCountsAndFirstLinesFollowTheLevelAndMarkNothing`, `TestAnAgentControlsItsLevelAndReadsCheaplyThroughTheTools` |
| Daemon restarts after level changes | Levels and skips are kept | `TestLevelsAndSkipsSurviveARestart` |
| Random level changes during random schedules | The implementation agrees with the delivery model; no message is both shown and skipped, or lost | `TestImplementationAgreesWithTheDeliveryModelOnRandomSchedules`, `TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules`, model `TestRandomLevelChangesNeitherLoseNorRepeatAMessage` |
| Agent reads a message through a thread, `recent` or `skipped`, then acks it | Allowed within the addressee rule; no cursor moves, nothing marked seen | `TestWorkerThatJoinsAfterAKickoffReadsItWithRecentAndReportsItHandled`, model `TestAgentAdmittedAfterAKickoffReportsItHandledAfterReadingTheThread`, `TestAgentAtLevelNoneReportsHandledWhatSkippedShowedIt`, `TestReadingAThreadDoesNotLetANonAddresseeReportHandled` |
| `seen` with an id only peek, recent, skipped or a thread showed | Refused as `not_shown`; only a full inbox listing's receipt counts | `TestSeenNeedsAnInboxReceiptThatSurvivesALevelChange`, model `TestSeenRefusesWhatOnlyAThreadOrSkippedViewShowed` |
| Level lowered after the inbox listed a message | The message can still be confirmed, is not skipped, and its notices arrive | `TestSeenNeedsAnInboxReceiptThatSurvivesALevelChange`, model `TestMessageListedAtAllAndConfirmedAfterSwitchingToNoneStaysConfirmed`, `TestLevelChangeDoesNotHideAMessageTheInboxListed` |
| One agent with opposite levels in two rooms, one changed | Each room follows its own level; the page shows both from its state | `TestOneAgentsLevelsInTwoRoomsAreIndependent`, `TestPageRendersOneAgentsOppositeLevelsInTwoRoomsFromItsState` |
| More skipped than one page | Paged with `before` to the end, nothing repeated or dropped; `recent` marks what was skipped | `TestSkippedHistoryPagesThroughEverythingALevelKeptOut`, `TestAgentAtNoneReadsWhatItSkippedPageByPageThroughTheTool` |
| Text of a kept-out message expires, or never arrives | Listed in `skipped` as expired | `TestSkippedListsAHeaderWhoseTextNeverArrivedAsExpired`, model `TestSkippedListsAMessageWhoseTextExpiredAsExpired` |
| Many messages in one millisecond | Every view keeps each author's order, the same on every read | `TestMessagesOfOneMillisecondKeepTheOrderWrittenInEveryView` |
| Branch under a message withheld, or held only as a header | Its replies are listed, the parent marked missing | `TestRealExchangeAgreesWithTheDeliveryModelOnTheBranchUnderAnUnheldParent`, model `TestBranchUnderAnUnheldMessageListsItsRepliesWithTheParentMissing`, `TestBranchUnderAHeaderWithoutTextListsItsRepliesWithTheParentMissing` |
| `peek` for one room while another is busy | Counts and first lines come from that room only | `TestPeekForOneRoomTakesItsFirstLinesBeforeTheCap` |

| Real daemon killed with `kill -9` at a random moment, then started | What `send` accepted survives; the room converges with each message once and in its author's order | `make chaos` (not in `make check`) |
| Real daemon paused (frozen) for 5 to 15 seconds | Its peers back off and it catches up after | `make chaos` |
| Real machine taken off the network and put back | It catches up without anyone acting | `make chaos` |
| Room split into two halves by the network, a daemon killed during the split, then healed | Both halves converge to the same records; handled reports arrive | `make chaos` |
| One machine cut off for good (sabotage) | The chaos checker reports the missing records and exits non-zero | `make chaos` with `--sabotage isolate` |
| A scheduled fault that did not take effect, or a run with nothing delivered | The chaos run is inconclusive (exit 3), not a pass | `chaos.py check DIR --unverify partition`, `--empty` |

### Chaos runs of real daemons

`make chaos` (`scripts/chaos/chaos.py run`) needs Docker and a few
minutes, and is not part of `make check`. It builds the static Linux
binary into a busybox image of about 17 MB, tagged for the run, and
starts one daemon per container, each the container's main process with
its own home volume, on one user-defined network. The last node runs
`net on --outbound-only`, so it has no inbound route (N1). The room is
formed by real invites, `join` and `confirm`, answered at a
pseudo-terminal inside the containers; the driver checks each
fingerprint against the joiner's node id. The first three nodes become
admins, so the third needs both admins' confirmations.

Every Docker object a run makes has a name with a random run id and the
label `a2p2chaos.run=<run id>`. A name already in use fails the run
instead of being reused. Cleanup removes only the objects Docker
returned to the run and those with its label, also after SIGINT or
SIGTERM, and a cleanup failure makes the exit status non-zero. `--keep`
leaves them and prints them.

While a fault schedule made from the seed runs, every node's agent sends
numbered room-wide posts, posts that ask for handled reports, replies in
threads, one replace and one withdraw, and reads its inbox and reports
handled what it may. Before replacing or withdrawing, the author waits
up to 20 seconds for another member's agent to be shown the original, so
that the notice path is exercised. The faults are `docker kill -s KILL`
and start, `docker pause` and unpause, `docker network disconnect` and
connect, and a partition into two halves (blackhole routes inside the
containers) with a kill inside it and a heal; extra kills overlap the
others. A kill waits for a driver inbox read in progress on that node
to finish, so the window between showing and confirming (D3's
exception) is not exercised. The schedule is written to `schedule.json`
before anything runs.

A fault counts only once its effect is seen: the process gone with exit
137, the container running again, paused or not, detached from the
network or back on it at its address, or the blackhole routes read back
from each running node (each pair across the split cut on at least one
side). The summary lists faults applied and not applied.

After the heal every node sends once more. The checker then requires:

- on every member, the same records with every field equal; each message
  once; every message a sender accepted; for each, the text, author,
  creation time, parent, thread and addressees the driver sent, and no
  record whose text no workload agent tried to send;
- each author's messages in the author's order in history and in the
  order the inbox showed them; nothing marked late; nothing still waited
  for; no message shown twice; every message for the agent shown;
- every handled report in `trace` on every member;
- the withdrawn message absent from history, absent from every file of
  the store once its one-minute scrub has run (DECISIONS D43), and an
  agent shown it told it was withdrawn and shown no withdrawn text
  after; the replacement in history, last in what each agent was shown,
  and an agent shown the original told it was replaced;
- every daemon running, no exit other than the kills, no panic in any
  log.

A run is inconclusive (exit 3) when any kind of fault was never seen
applied, no applied partition had an accepted send on both sides during
it, a member had no accepted send after the last fault, the run made no
accepted reply, handled report, replace or withdraw, no reader was shown
an original before its replace or withdraw, or an inbox read failed
partway (that member's inbox cannot be checked for completeness). A
violation exits 1, a run that could not be carried out or cleaned up
exits 2. Variables: `NODES`, `SEED`, `DURATION`. Logs, the schedule, a
snapshot and `summary.txt` go to `.chaos/`.

The checker fails when it should, shown on a saved snapshot with
`chaos.py check DIR`: `--drop n3:n1` (n1's records deleted from n3's
copy) gives violations; `--corrupt` (one message's text changed the same
way on every member) gives violations; `--unverify partition` and
`--empty` (nothing got through) are inconclusive. `run --sabotage
isolate` leaves one node cut off after the heal and fails the run.

Recorded runs on 2026-11-24, after the store gained `secure_delete` and
its one-minute scrub (DECISIONS D43); every fault of every run was seen
applied, and no withdrawn text was found in any member's files:

| Seed | Nodes | Faults for | Kills | Accepted sends | Handled reports | Notices checked (replace, withdraw) | Settled after heal | Result |
|---|---|---|---|---|---|---|---|---|
| 1 | 3 | 107 s | 8 | 175 | 85 | 2, 2 | 6 s | pass |
| 2 | 3 | 92 s | 5 | 106 | 51 | 3, 1 | 6 s | pass (a first run was inconclusive: the driver's own flag missed the notices; the checker now reads them from the inbox) |
| 3 | 3 | 96 s | 6 | 175 | 78 | 2, 2 | 0 s | pass |
| 4 | 8 | 120 s | 10 | 570 | 732 | 7, 7 | 4 s | pass |
| 5, `--sabotage isolate` | 3 | 55 s | 3 | 45 | 19 | 0, 1 | not settled in 60 s | 34 violations, exit 1 |

Not covered: a real WAN, latency or loss (`tc netem` is not used),
clock skew, a full disk, power loss (a kill -9 leaves the kernel's page
cache intact, so it does not test `synchronous(FULL)`), a crash between
showing and confirming, restores from backup, removals during faults,
and machines on different versions.

## Acceptance

1. `TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace` covers messaging with two
   daemons. `TestJoinOverTLSBetweenTwoMachines` covers the join itself.
2. A message reaches three machines over loopback TLS and its handled report
   returns in `TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns`.
3. The seven MCP tool definitions match their fixture and fit the token budget
   in `TestToolsGolden` and `TestToolDefinitionsFitTheTokenBudget`.

## Team launcher

`make team-test` (part of `make check`) runs `scripts/team/test_team.py`
in about fifteen seconds: the launcher with a fake harness that calls the
real `a2p2 mcp` over stdio, against a throwaway home. The real harness
commands are shadowed by stubs, so no model is ever called.

| Scenario | What happens | Test |
|---|---|---|
| A member's tool server connects | `started` only once the label is registered under the harness's name, from this run's working directory, with a last-seen time at or after the launch time taken before the process started; a member that exits 0 without that is `not_ready`, not `done` | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| A label registered earlier under the same harness name, refreshed by a CLI call during the run | Not counted as connected; the member is `not_ready` | `test_a_reused_label_refreshed_by_the_cli_is_not_ready` |
| A member exits 0, exits non-zero, passes its wall-clock limit, goes quiet or asks the person | `done` with the saved answer, `failed` with the exit status, `limit` and the process group stopped, `silent`, `needs input`; each posted once | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| The room or the a2p2 CLI is slow while a member passes its limit | The member is stopped on the supervisor's monotonic clock within a tick, independent of room polling and of CLI calls returning | `test_limit_is_enforced_while_room_calls_block` |
| A member posts text that looks like launcher commands or notices | Nothing is stopped or changed | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| `team report` after a run | Messages, multi-inbox replies, questions to the person, refusals by code and empty polls from the logs, exit state and `DONE:`/`BLOCKED:` per member, as JSON and Markdown | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| `team status`, `team logs`, `team stop LABEL` on a running member | The supervisor answers the stop request and stops the member; `stopped` is posted; a second `up` on the same state is refused | `test_status_logs_and_stop_one_member_through_the_supervisor` |
| The supervisor is killed, then `team stop` runs with the recorded pid now naming another process | The member's guard ends its process group; `stop` finds no live supervisor, signals nothing and marks the roster ended | `test_stop_never_signals_a_pid_read_from_disk` |
| The supervisor gets SIGTERM | It stops its members first and records them as stopped | `test_a_terminated_supervisor_stops_its_members_first` |
| Two `team up` on one state directory at once | One starts the run, the other is refused | `test_two_ups_at_once_one_wins` |
| An Antigravity member's checkout already has `.agents/mcp_config.json`, as a file or a symlink | Refused before any daemon starts; the file and the symlink's target are unchanged | `test_antigravity_config_in_a_checkout_is_never_overwritten` |
| A manifest with a bypass mode (`bypassPermissions`, unscoped `Bash`, `danger-full-access`), an unknown key, a zero, negative, infinite, NaN or string limit, too many members, or a repeated or reserved label | Refused with the allowed values, before any daemon starts or state is reserved | `test_manifest_refuses_bypass_modes_and_bad_limits` |

## CI

Most testing is local, and the hosted CI is kept small on purpose
([releasing](RELEASING.md#what-is-tested-where)). The
[CI workflow](../.github/workflows/ci.yml) is one Linux job. It runs on a
pull request that changes Go code, `go.mod`, `go.sum`, the Makefile,
`scripts/`, the workflows or test data, and when started by hand. It runs
`make check` and `make lint`. Nothing runs on a push to `main`, and no job
runs on macOS.

The [release workflow](../.github/workflows/release.yml) runs when a tag
starting with `v` is pushed. On Linux, without a module or build cache, it
runs `make check`, `make lint` and `make vuln`, checks that CHANGELOG.md
has a section for the tag, runs `make release VERSION=<tag>`, checks that
the Linux binary reports the tag, and publishes the binaries and
`SHA256SUMS` as a GitHub release with that section as its notes (a
prerelease when the tag contains a hyphen).

Before a tag, a maintainer runs `make prerelease` on their own machine:
`make check`, `make lint`, `make vuln`, `make check-linux` and a 60-second
`make chaos` run. `make check-linux` runs `go vet` and the race tests on
Linux in a Docker container, so a macOS machine also tests Linux.
`make chaos` runs real daemons in Docker containers through kills, pauses
and network splits (see [Chaos runs of real daemons](#chaos-runs-of-real-daemons)).

## Optional checks

- `A2P2_MODEL_DEEP=1 go test ./internal/members ./internal/model/delivery ./internal/node`
  uses larger search bounds and includes measurements. The last recorded
  deep membership run took 33 minutes.
- `go test ./internal/core -run '^$' -fuzz FuzzParseRecord -fuzztime 20s`
  fuzzes the record parser. Use `FuzzParseEntry` for log entries or
  `FuzzDecodeToken` for invite tokens.
- `make e2e HARNESS=claude` uses a logged-in Claude Code CLI and spends model
  tokens. The end-to-end runner also accepts `codex`, `agy` or `all`. Its
  last recorded results predate the delivery rewrite; the
  [harness notes](HARNESSES.md) record a single-run check of the tools on
  the current code with each harness.

## Test gaps

- Automated tests of the local page's JavaScript in a browser. The page
  has been checked by hand in headless Chrome at 1280 and 390 pixels wide,
  in light and dark themes, and with the keyboard.
- The end-to-end runner after the delivery rewrite, interactive sessions,
  hooks inside a real session (including end-of-turn hooks and Codex and
  Antigravity models seeing hook text), and a real installed configuration.
- Two physical machines on a LAN, packet captures, and a real network with a
  machine whose clock is wrong by days.
- Power loss, a kill at arbitrary points, and a database failure during a full
  pull. Tests cover restarts at chosen points and individual store failures.
- End-to-end throughput and memory. There are membership benchmarks and
  measurements of request and response bytes, including
  `TestPullSizesInASixtyFourMachineRoomAfterThirtyDaysOfDailyRestarts`.
