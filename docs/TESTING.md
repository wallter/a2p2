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
| Hook or MCP registration removed by hand | `doctor` says which one is missing | `TestDoctorSaysWhetherTheHookOrTheMCPRegistrationIsMissing` |
| A CLI command run under a harness agent's label | The agent's hook keeps working | `TestACLICallAsAHarnessAgentKeepsItsHookWorking` |
| `stop` during a waiting call or a held pull | The call ends before the store closes; the held pull is released at once | `TestStopDuringAWaitingCallEndsItBeforeTheStoreCloses`, `TestStopReleasesAHeldPullAtOnce` |
| Page request with wrong `Origin` or `Host`, or no token | Refused | `TestAuthHostOrigin`, `TestPageDecisionNeedsTheExactOriginAndShowsTheFullFingerprint` |
| Message text that looks like instructions | Shown as labelled peer data | `TestHostileStringsAreInert`, `TestInboxForgedHeadersStayInert` |
| Database write fails partway | The store rolls back the write or log batch | `TestFailureBetweenStatementsLeavesNoHalfAppliedWrite`, `TestLogBatchThatFailsLeavesNothingApplied` |
| Power loss | Open test gap | None |
| Clock set back or ahead | Expired text stays expired. Replacement order follows the author's history. A record dated over a day ahead waits. | `TestTextThatExpiredBeforeItWasShownIsAnExpiredNoticeEvenAfterTheClockGoesBack`, `TestAFutureRecordLeavesNoSeenAndIsTakenOnceTheClockAllows`, model `TestNewestReplacementIsByTheAuthorsHistoryNotItsClock` |
| Agent at level `addressed` | Shown what names it, replies in its threads and posts with `ask_handled`; room-wide posts are skipped; another agent on the machine is unaffected | `TestAgentAtAddressedSeesWhatIsAddressedToItAndRepliesInItsThreadsButNotRoomWidePosts`, model `TestAddressedLevelShowsWhatIsAddressedAndPostsMarkedForEveryone` |
| Agent at level `none` | Nothing reaches its inbox; it reads what it skipped when it asks, and asking marks nothing | `TestAgentAtNoneIsShownNothingUntilItAsks`, model `TestNoneLevelShowsNothingUntilTheAgentAsks` |
| Level raised | What was skipped stays out of the inbox and is readable apart, unmarked; the inbox continues from now | `TestRaisingTheLevelShowsWhatWasSkippedWithoutMarkingItSeenAndTheInboxContinuesFromNow`, model `TestRaisingTheLevelKeepsWhatWasSkippedOutOfTheInboxAndReadableApart` |
| Counts and first lines asked for | They follow the level and mark nothing | `TestCountsAndFirstLinesFollowTheLevelAndMarkNothing`, `TestAnAgentControlsItsLevelAndReadsCheaplyThroughTheTools` |
| Daemon restarts after level changes | Levels and skips are kept | `TestLevelsAndSkipsSurviveARestart` |
| Random level changes during random schedules | The implementation agrees with the delivery model; no message is both shown and skipped, or lost | `TestImplementationAgreesWithTheDeliveryModelOnRandomSchedules`, `TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules`, model `TestRandomLevelChangesNeitherLoseNorRepeatAMessage` |

| Real daemon killed with `kill -9` at a random moment, then started | What `send` accepted survives; the room converges with each message once and in its author's order | `make chaos` (not in `make check`) |
| Real daemon paused (frozen) for 5 to 15 seconds | Its peers back off and it catches up after | `make chaos` |
| Real machine taken off the network and put back | It catches up without anyone acting | `make chaos` |
| Room split into two halves by the network, a daemon killed during the split, then healed | Both halves converge to the same records; handled reports arrive | `make chaos` |
| One machine cut off for good (sabotage) | The chaos checker reports the missing records and exits non-zero | `make chaos` with `--sabotage isolate` |

### Chaos runs of real daemons

`make chaos` (`scripts/chaos/chaos.py run`) needs Docker and a few
minutes, and is not part of `make check`. It builds the static Linux
binary into a busybox image of about 17 MB and starts one daemon per
container, each the container's main process with its own home volume,
on one user-defined network. The last node runs `net on --outbound-only`,
so it has no inbound route (N1). The room is formed by real invites,
`join` and `confirm`, answered at a pseudo-terminal inside the containers;
the driver checks each fingerprint against the joiner's node id. The
first three nodes become admins, so the third needs both admins'
confirmations.

While a fault schedule made from the seed runs, every node's agent sends
numbered room-wide posts, posts that ask for handled reports, replies in
threads, one replace and one withdraw, and reads its inbox and reports
handled what it may. The faults are `docker kill -s KILL` and start,
`docker pause` and unpause, `docker network disconnect` and connect, and
a partition into two halves (blackhole routes inside the containers)
with a kill inside it and a heal; extra kills overlap the others. The
schedule is written to `schedule.json` before anything runs. After the
heal every node sends once more, and the checker requires, on every
member: the same records (key, version, sender, text, thread and
addressees), each message once, every message a sender accepted, each
author's messages in the author's order in history and in the order its
inbox showed them, no message shown twice and every message for the
agent shown, nothing marked late, nothing still waited for, every
handled report in `trace`, the withdrawn message and its text gone, the
replacement text shown, every daemon running, no exit other than the
kills, and no panic in any log. Variables: `NODES`, `SEED`, `DURATION`.
Logs, the schedule, a snapshot and `summary.txt` go to `.chaos/`.

The checker fails when delivery is broken. `chaos.py check DIR --drop
n3:n1` deletes n1's records from n3's copy in a saved snapshot and
reports four violations; `run --sabotage isolate` leaves one node cut
off after the heal and fails the run.

Recorded runs (2026-11-24, 3 nodes, default schedule unless stated):
seeds 1, 2 and 3 (92 to 107 seconds of faults, 5 to 8 kills each, 169
to 184 accepted messages, 82 to 85 handled reports) had no violations and
settled within 6 seconds of the heal. Eight nodes, seed 4, 120 seconds
(10 kills, 641 accepted messages, 866 handled reports): no violations,
settled within 5 seconds. Seed 5 with `--sabotage isolate`: 11
violations, exit 1, as it should.

What the runs showed beyond the checks: the text of a withdrawn message
is gone from every surface, but its bytes can stay on disk. The summary
greps each daemon's database files for the withdrawn text; in four of
six runs the write-ahead log still held it at the end, and once the main
database file did (free pages; the store does not set
`secure_delete`). D9 says machines drop the withdrawn text; on disk that
holds only once SQLite reuses the pages.

Not covered: a real WAN, latency or loss (`tc netem` is not used),
clock skew, a full disk, power loss (a kill -9 leaves the kernel's page
cache intact, so it does not test `synchronous(FULL)`), restores from
backup, removals during faults, and machines on different versions.

## Acceptance

1. `TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace` covers messaging with two
   daemons. `TestJoinOverTLSBetweenTwoMachines` covers the join itself.
2. A message reaches three machines over loopback TLS and its handled report
   returns in `TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns`.
3. The seven MCP tool definitions match their fixture and fit the token budget
   in `TestToolsGolden` and `TestToolDefinitionsFitTheTokenBudget`.

## Team launcher

`make team-test` (part of `make check`) runs `scripts/team/test_team.py`
in about five seconds: the launcher with a fake harness that calls the
real `a2p2 mcp` over stdio, against a throwaway home. No model is called.

| Scenario | What happens | Test |
|---|---|---|
| A member's tool server connects | `started` is posted only after the member's MCP server registered its label after launch; a member that never starts one gets no `started`, and its `done` says so | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| A member exits 0, exits non-zero, passes its wall-clock limit, goes quiet or asks the person | `done` with the saved answer, `failed` with the exit status, `limit` and the process group stopped, `silent`, `needs input`; each posted once | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| A member posts text that looks like launcher commands or notices | Nothing is stopped or changed | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| `team report` after a run | Messages, multi-inbox replies, questions to the person, refusals by code and empty polls from the logs, exit state and `DONE:`/`BLOCKED:` per member, as JSON and Markdown | `test_watcher_reports_each_ending_and_the_run_is_measured` |
| `team status`, `team logs`, `team stop LABEL` on a running member | The roster shows it running; its log is printed; it is stopped and `stopped` is posted; a second `up` on the same state is refused | `test_status_logs_and_stop_one_member` |
| A manifest over its member cap, or with a repeated or reserved label | Refused before any daemon starts | `test_manifest_limits` |

## CI

The [CI workflow](../.github/workflows/ci.yml) runs on every push to `main`
and on pull requests. On Linux and macOS it runs `make check` (which
includes the Windows build gate and the licence check), then
`make release`, which builds the Linux and macOS binaries. On Linux alone
it runs `make lint`, `make vuln` and `make fuzz FUZZTIME=30s`; the fuzz job
exercises `FuzzParseRecord` and `FuzzParseEntry`, and the Go test suite
also runs the seeds for `FuzzDecodeToken`.

The [release workflow](../.github/workflows/release.yml) runs when a tag
starting with `v` is pushed. On Linux, without a module or build cache, it
runs `make check` and `make release VERSION=<tag>`, checks that the Linux
binary reports the tag, and publishes the binaries and `SHA256SUMS` as a
GitHub release (a prerelease when the tag contains a hyphen).

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
