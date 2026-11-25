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

## How make check and make test run

`make check` runs its steps at the same time (`scripts/gate.sh`): fmt,
vet, the Windows build gate, licences, trace, recipes, the race tests
and the launcher's tests. It prints one line per step as it ends, with
its time, then the full log of every step that failed, and exits non-zero
naming each failed step.

`make test` (`scripts/gotest.sh`) runs every test once with
`-race -count=1 -timeout 30m`, as `go test ./...` did. A package whose
tests took 20 seconds or more in total by
[`scripts/test-timings.tsv`](../scripts/test-timings.tsv) is split into
shards, because Go runs the top-level tests of one package one after
another:

- the package's test binary is compiled once (`go test -race -c`), and
  its tests, fuzz targets and examples are listed with `-test.list`;
- the longest recorded tests are placed first, each on the least loaded
  shard; tests with no recorded time are dealt round-robin;
- each shard is one process of that binary, run in the package directory
  with its own `TMPDIR` and `-test.run` set to an anchored alternation of
  its names (`^(TestA|TestB)$`, which also runs their subtests);
- the runner fails if the shards together are not exactly the listed
  tests, or if a shard that exited 0 did not report each of its tests
  once (`--- PASS`, `--- FAIL` or `--- SKIP` in `-test.v` output).

Every other package runs in one `go test`, at the same time as the shards.
`SHARDS` sets the number of shards per package: by default the number of
CPUs, at most 12; the pull-request job uses 2 and `make check-linux` 8.
`make test-timings` runs the tests and rewrites the timings file; run it
when tests are added or become slower, so that the shards stay balanced
and a package that has grown past 20 seconds is sharded. A missing or
stale entry changes only how the tests are grouped, never which run.

Tests of a sharded package must not share anything between processes.
Checked on 2026-11-24 for the six packages sharded then (`internal/node`,
`internal/daemon`, `internal/mcpshim`, `internal/peer`,
`internal/members`, `internal/model/delivery`): every listener binds
port 0, every directory comes from `t.TempDir()` or `os.MkdirTemp`, the
fixed addresses in tests (`127.0.0.1:7423`, `127.0.0.1:7422`) are
strings that are parsed or shown, never bound, and package-level
variables that tests change (limits, timeouts) live in one process and
are never written to disk. Test data under `testdata/` is only written
with `-update`. No test needed a change.

Running the steps together loads the machine more than running them one
after another did, which exposes timing races. Two tests failed that way
in November 2026, and both are fixed:
`TestOverTLSAHeldPullGivesUpItsConcurrencySlot` was told of a hold before
the held request had given up its concurrency slot (`NextHold` now
signals after the slot is given up), and
`TestACrashAfterTheRouterMadeTheMappingIsUndoneFromTheRecordFile` needed
its fake router to answer within 30 ms. Two rules follow for tests:

- a hook that tells a test that something happened fires after all of
  it has happened, not when it begins;
- a real-time budget a test must stay within is hundreds of
  milliseconds or more, never tens. A wait that a test expects to run
  out may be short. The portmap tests' client waits 250 ms for a fake
  that answers at once (`testTry`), the dial-back tests use a 300 ms
  deadline, the TLS probe test 2 s, and the nudge pacing test a pace of
  one second.

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
| Random schedules of the real exchange at seeds other than the fixed one | The implementation agrees with the delivery model after every step. `make check` runs 14 seeds that once disagreed at the default size (3 with the race detector); `A2P2_MODEL_DEEP=1` runs 12 of them at the deep size; `make model-sweep` runs any seeds. At the deep size seed 6 meets the known gap two rows down and seed 16 a gap of the comparison (an offer built on a partial observation) | `TestRealExchangeAgreesWithTheDeliveryModelAtSeedsThatOnceDisagreed`, `TestRealExchangeAgreesWithTheDeliveryModelOnTheReducedSeedScenarios`, `TestRealExchangeOfferOnAPartialObservationMayCarryOtherRecordsThanTheModels` (skips as a gap of the comparison) |
| Newcomer without history whose admitter's latest record is withheld by the only member holding it | Earlier text is held without text, undecided, until the head arrives, then `not_admitted` | `TestRealExchangeNewcomerWhoseFrontierHeadIsWithheldDecidesOnceItArrives`, model `TestNewcomerWithoutHistoryHoldsEarlierTextNotAdmittedWhileItsAdmittersHeadIsWithheld`, `TestRemovedNewcomerNotToldOfItsRemovalIsServedNothingMore` |
| Offer cut by a limit of one, or a record offered and refused out of order after the server's restore | The rest of the round's one offer may repeat what the server holds (a wasted copy between members). A record refused out of order is not offered again in that exchange, so the gap closes one exchange later (known gap against the model, which fills it at once) | `TestRealExchangeOfferCutByALimitOfOneMayOfferTheServerWhatItHolds`, `TestRealExchangeOfferRefusedOutOfOrderAfterARestoreWaitsForTheNextExchange` (skips as a known gap), model `TestOfferLimitedToOneIsBuiltEachPullOnTheLastSummary`, `TestAPullWhoseReplyIsLostGivesThePullerNoSummaryToOfferOn` |
| A member asks for a dial-back to its own listener, to another key's listener it listed, to its source address on another port, to a closed port and to a silent one | Only its own key's address is reached; the other key's machine gets a handshake and no request; the answer comes within the deadline | `TestDialBackOverTLSReachesOnlyTheAskersOwnKey`, `TestDialBackReachesOnlyTheAskersOwnKeyAndReportsTheSourceSeen` |
| A member asks for a dial-back to a third party's address, one it advertised elsewhere, three ports of its source address, or one endpoint written two ways | Not dialled: answered `not_tried` at once; only the request's source IP address is dialled, on at most two ports; the repeat is refused 400 | `TestADialBackDialsOnlyTheSourceAddressOnAtMostTwoPorts`, `TestOverTLSACandidateIsVerifiedOnlyWhereTheAskersKeyAnswersAndIPv6Works` |
| An IPv6 address to check and no member reachable over IPv6, then one | Not checked, saying "not checked: no member reachable over IPv6"; then asked over IPv6 and verified | `TestOverTLSACandidateIsVerifiedOnlyWhereTheAskersKeyAnswersAndIPv6Works` |
| A member's answer says it saw control characters, a name, a zone or a port, or lists addresses not asked | The answer is refused before anything is stored or printed | `TestADialBackAnswerSaysOnlyAPlainIPAddressSeen` |
| A machine that shares no room asks for a dial-back | Refused 403, nothing dialled | `TestDialBackIsRefusedToAMachineThatSharesNoRoomAndDialsNothing`, `TestDialBackOverTLSRefusesNonMembersLimitsRateAndOldMachinesSay404` |
| A dial-back names a host, a loopback, special or repeated address, or more than 8 | Names and malformed requests are refused 400; special addresses are skipped, loopback dialled only for an asker on loopback | `TestDialBackRequestsAreStrictAndNameNoHosts`, `TestDialBackNeverDialsLoopbackOrSpecialAddressesForARemoteAsker` |
| Dial-backs asked too often by one member, by many, or at once | 429 with Retry-After per key and for the machine; a key over its own limit spends nothing of the machine's; at most 4 at once | `TestDialBackIsRateLimitedPerPeerAndForTheWholeMachine`, `TestDialBackServesAtMostFourRequestsAtOnce` |
| Timing of a dial-back answer used to learn about a third party | Any answer with an address not reached comes at the deadline | `TestDialBackAnswersAtTheDeadlineUnlessEveryAddressWasReached` |
| Member without the dial-back (older version), or none answers | Reach stays unknown, saying why; an unanswered check never replaces a known answer; a member is asked at most hourly unless a person checks | `TestReachIsYesNoOrUnknownFromTheAnswersAndAsksAPeerAtMostHourly`, `TestDialBackOverTLSRefusesNonMembersLimitsRateAndOldMachinesSay404` |
| `net check` with members reached, timed out and refused; with networking off | Each address with its outcome; only reached members are asked to dial back; off dials nothing; one sentence of advice for each case | `TestNetCheckTriesEachMemberAndAsksThoseReachedToDialBack`, `TestNetCheckWithNetworkingOffTriesNothing`, `TestNetCheckAdviceSaysWhatToDoInEachSituation`, `TestProbeOutcomesOverRealSockets`, `TestReachOverTLSIsYesThenNoWhenTheListenerGoes` |
| Interfaces with loopback, link-local, stable, temporary, deprecated and unique-local addresses | IPv4 and global IPv6 advertised; temporaries only where the interface has no stable address; unique-local last; Linux flags and the lowest-metric default route read from `/proc`, which may hold no default route or IPv6 | `TestAdvertisedAddressesAreIPv4AndGlobalIPv6WithoutLinkLocalOrNeedlessTemporaries`, `TestLinuxRouteAndIPv6FlagsAreReadFromProc` (Linux) |
| A peer's unique-local IPv6 address, from the same site and from another | Dialled only from a machine holding an address in the same `/48` | `TestAUniqueLocalAddressIsDialledOnlyFromTheSameSite` |
| Listening on an unspecified host | One socket takes IPv4 and IPv6 connections; a dial-back over IPv6 loopback is reached | `TestThePeerListenerOnAnUnspecifiedHostTakesIPv4AndIPv6`, `TestOverTLSACandidateIsVerifiedOnlyWhereTheAskersKeyAnswersAndIPv6Works` |
| A lying member reports a public address it saw and claims to reach everything; a mapped address confirmed, then unconfirmed for 3 hours, unmapped and mapped again, or tried and missed once and twice | What members saw is shown and never asked about or advertised; the mapped address is advertised only while a member reached it within 3 hours and it is still mapped; one that comes back starts unconfirmed; one miss keeps it, two withdraw it | `TestWhatAMemberSaysItSawIsShownAndNeverAdvertised`, `TestAMappedAddressIsAdvertisedOnlyWhileRecentlyReachedAndStillMappedAndOneMissDoesNotWithdrawIt` |
| The mapping changes many times in a row | Members are asked at most once per pace; a change that comes too soon is checked when the pace allows | `TestNudgesToCheckThePortMappingArePaced` |
| Members answer but would try none of the addresses | Reach is unknown, saying why, never no | `TestReachIsUnknownWhenTheMembersThatAnsweredTriedNothing` |
| Fake router speaking PCP: map, renew, remove | The same nonce is used to renew and to delete | `TestPCPMapsRenewsWithTheSameNonceAndRemoves` |
| Fake router speaking only NAT-PMP | A PCP answer of version 0 leads to NAT-PMP, which maps | `TestARouterThatSpeaksOnlyNATPMPIsAskedInNATPMP` |
| Fake router that refuses every protocol | Nothing mapped; each protocol's reason is reported | `TestARouterThatRefusesIsReportedWithEachProtocolsReason` |
| Fake router that answers late, and one that answers too late | A late answer within the retries is taken; a too-late one falls through to the next protocol | `TestALateAnswerIsTakenAndOneTooLateFallsThrough` |
| Fake UPnP router with the port taken, or a description pointing elsewhere | The next port is mapped and reported as the external port, then removed; a URL whose host is not the gateway is never fetched | `TestUPnPMapsATakenPortToTheNextAndRemovesIt`, `TestUPnPFollowsNoURLThatIsNotTheGateway` |
| Fake PCP router on loopback that loses the mapping, granting one second | The keeper takes each new port, removes the one replaced, renews before expiry and removes the last at the end | `TestAKeeperFollowsARouterThatLosesTheMappingAndRemovesItAtTheEnd` |
| `--map-port` decided, then `net off`, a decision without it, a stop; no router found | Mapped only when decided, shown in status, not advertised before a member reaches it, removed each time; with no router the reason is shown and the listener still runs; outbound with mapping refused | `TestPortMappingIsShownAndRemovedOnNetOffAnotherDecisionAndStop`, `TestPortMappingWithoutARouterSaysWhyAndStillListens`, `TestPortMappingIsRecordedInTheNetworkingDecisionAndNeedsAListener` |
| The router's external address differs from the address members see | `net check` says the router is behind another NAT and the mapping cannot help | `TestNetCheckSaysWhenTheRouterIsBehindAnotherNAT` |
| Fake router reads the record file to check whether what it is asked was recorded first | The mapping is in the file (mode 0600) before every request, and gone from it, the router and the directory after the end | `TestTheMappingIsRecordedBeforeTheRouterIsAsked` |
| A crash right after a PCP, NAT-PMP or UPnP router made the mapping (UPnP on the next port, no external address learned) | The record file holds what was written before the request, the UPnP external port among it; the next run reads the file and removes the mapping (PCP by the kept nonce) | `TestACrashAfterTheRouterMadeTheMappingIsUndoneFromTheRecordFile` |
| A record with an unknown field, a value out of range, a field of another protocol, a mapping on another router, data after the list, or that does not parse | Nothing is sent to any router; the file is kept unchanged; status says the record cannot be used and what to remove by hand | `TestARecordThatCannotBeUsedWholeSendsNothingAndIsKept`, `TestARecordThatCannotBeUsedIsKeptAndNothingIsSent` |
| A UPnP forward on the recorded port that points at another machine, another internal port, or has another description | Not deleted; the record forgets it | `TestAUPnPForwardThatIsNotThisMachinesIsNotDeleted` |
| A UPnP fault body cut short, on an add (718) and on a removal (714) | Not a refusal: no next port, the request stays recorded; a removal is not counted as done | `TestAMalformedUPnPFaultDrivesNoFallbackAndNoCleanup` |
| Fake NAT-PMP router maps the port, then refuses the external address | Map fails, but the mapping is kept and removed at the end | `TestAMappingMadeBeforeAFailedAddressLookupIsStillRemoved` |
| Fake PCP router on loopback stops answering renewals, then removals | The mapping stays held and shown, and is no longer live once its lease ran out; the failed removal is reported, kept in the record file and done by the next run | `TestAFailedRenewalKeepsTheMappingAndAFailedRemovalIsReportedAndRetried` |
| Fake router answers with lifetime 0, port 0, a version-0 datagram that is not NAT-PMP's refusal, an SSDP 404 or another device, a SOAP 200 cut short | None is taken for a mapping; what may exist is kept to be removed | `TestMalformedRouterAnswersAreNotTakenForAMapping` |
| Fake UPnP router that accepts finite leases, or refuses them (725); removing an entry already gone (714) | A finite lease is asked first, a permanent one only after 725; 714 counts as removed | `TestUPnPAsksForAPermanentLeaseOnlyWhenRefusedAFiniteOne` |
| `--map-port` with a listener on one address the router does not forward to | No mapping; status says why | `TestNoMappingForAListenerThatDoesNotCoverTheAddressTheRouterForwardsTo` |
| The listener stops while the daemon runs | The mapping is removed with it | `TestTheMappingIsRemovedWhenTheListenerStops` |
| A record's header commits to another message's manifest | No list, no want and no bytes for it, from any machine | `TestACopiedCommitmentGetsNoListNoWantAndNoBytes` |
| A removal commits between reading the membership and the rows | The decision reads both again; the removed machine is refused | `TestARemovalCommittedBetweenTheMembershipAndTheRowsIsSeen` |
| This machine's own permission for an author is undecided | Its files are neither kept nor served | `TestAnUndecidedGrantIsARefusal` |
| A requester stops reading, then the message is withdrawn | The response ends without another read | `TestAResponseBlockedOnTheNetworkIsCutWhenItsPermissionEnds` |
| An uploader stalls mid-body, then the want ends | The body is closed, nothing is kept | `TestAnUploadBlockedOnTheNetworkIsCutWhenTheWantEnds` |
| The want ends after the last byte, before the bytes go live | Not promoted; the part goes | `TestAnUploadWhoseWantEndsJustBeforeItGoesLiveIsNotPromoted` |
| The agent edits its copy, then fetches again | A new copy with the verified bytes | `TestAFetchAfterTheAgentEditedItsCopyGetsVerifiedBytes`, `TestPublishingAgainHandsOutANewCopyAndQueuesTheOldOne` |
| An agent puts a byte-identical file at its copy's name, then the message is withdrawn | That file stays | `TestADeletionRemovesOnlyTheCopyItPublishedNeverAByteIdenticalFileAtItsName` |
| A queued deletion fails, then nothing happens in the room | Kept, and the next file tick completes it | `TestAQueuedDeletionThatFailsIsKeptAndTriedAgain`, `TestADeletionThatFailedIsCompletedByTheNextIdleTick` |
| A restart with parts recorded complete, missing or short | Every part is dropped; the transfer completes from zero | `TestAStartDropsEveryPartSoATransferCutByACrashStartsFromZero` |
| A send's request expires while its files are put in place | Not published | `TestARequestThatExpiresWhileItsFilesArePutInPlaceIsNotPublished` |
| A burst of file requests from one machine | Refused past the file bucket; its pulls still admitted | `TestFileRequestsDoNotSpendThePullExchangesAdmission` |
| A transfer's body stalls on the network | The database stays free | `TestTheDatabaseIsFreeWhileATransferWaitsOnTheNetwork` |
| A requester on loopback stops reading a large response | Cut by the minimum rate, long before the total time | `TestAnOutboundResponseToAReaderBelowTheMinimumRateIsCut` |
| A schedule of pulls, fetches, a withdraw, a removal and an expiry, some landing inside a fetch, on the model and on three machines | After every step: the same holders and copies, the bytes each would serve and each copy hash to the file and equal the model's, and the same machines received the file (from the network's transfer events) | `TestImplementationAgreesWithTheModelOnAttachmentSchedules` |
| Staging changed after the digests were checked, before the bytes are put in place | Not published; the request fails | `TestAnApprovalOfStagingChangedAfterItWasShownPublishesNothing` |
| An approval sent with a terms digest that is not the request's | Refused, `terms_changed`; nothing sent | `TestTheApprovedTermsCoverEveryEffectiveTermAndAStaleDigestIsRefused` |
| Another agent on the author's machine fetches a file it was not shown | Refused until it is shown | `TestOnTheAuthorsMachineOnlyTheSendingAgentMayFetchWithoutBeingShown` |
| A fetch while the author is offline and the only other member joined without history | No request reaches that member; the reader is told `coming` | `TestAFetchIsNeverSentToAMemberThatMayNotReadTheMessage` |
| A wanted upload is promoted while the person leaves another room | Both finish; pulls go on (lock order written at `store.Store.fileMu`) | `TestAPromotionAndALeaveAtTheSameTimeBothFinish` |
| A wrong regular file at a blob's path before a download goes live; a held blob changed while stopped | Replaced by the verified bytes, which are copied and served; at the start, removed and not served | `TestAFilePlantedAtTheBlobPathIsNeverTakenForTheBlob` |
| Files under `blobs/` that a2p2 did not name, then a start | Left alone and counted in `status`; blob and part names with no row still go | `TestAStartRemovesOnlyFilesWithNamesA2p2Generates` |
| An implicit reply with files; a send to everyone whose room gains a member before publication | The terms name the parent's author; the second sends nothing and ends `terms_changed` | `TestTheTermsNameTheRecipientsTheMessageIsReallySentTo` |
| Another agent on the author's machine wrote the winning version | That agent fetches unshown; the first sender must be shown it | `TestTheExemptionFromShownIsForTheWriterOfTheVersionFetched` |
| Putting approved bytes in place fails | A `FileError` with an operation and a code, no path | `TestAnApprovalWhoseBytesCannotBeWrittenFailsWithoutAPath` |
| A want's life ends while its promotion waits for the transaction | Not promoted: the clock is read inside it | `TestAPromotionReadsTheClockInsideItsTransaction` |
| A removal whose directory sync fails, then runs with the path gone | Kept until a sync succeeds | `TestARemovalWhoseDirectorySyncFailedStaysQueuedUntilASyncSucceeds` |
| A want past its life, before the sweep | Not advertised in a pull answer | `TestAWantPastItsLifeIsNotAdvertisedBeforeTheSweep` |
| A file moved right after it was opened for staging; a UNC or device path on Windows | Shown where the handle is; refused by name before any I/O | `TestThePathShownIsTheOpenedFilesNotTheNameAskedFor`, `TestRemoteAndDevicePathsAreRefusedByNameBeforeAnyIO` (Windows only) |
| Staging a missing file | The error names the operation and a code, never the path | `TestAFileErrorNamesItsOperationAndCodeNeverThePath` |
| A crash left a mapping on the router and in the record; the next start has networking off, and the removal first fails | It is removed at the next start whatever the decision; a failure is shown in status and retried at stop | `TestAMappingLeftByACrashIsRemovedAtTheNextStartAndAFailureIsShown` |
| A networking decision recorded before IPv6 | It stays IPv4 only, says so, advertises no IPv6, not even a configured `advertise` entry, which `net check` names with why; a new decision covers IPv6 and its prompt says so | `TestANetworkingDecisionFromBeforeIPv6StaysIPv4OnlyUntilDecidedAgain`, `TestAnIPv4OnlyDecisionAdvertisesNoIPv6`, `TestAConfiguredIPv6AddressIsNotAdvertisedUnderAnIPv4OnlyDecision` |
| Two addresses of this machine in the gateway's subnet, the system choosing the second | The router forwards to the address the system selects; one off the gateway's network is refused; the route lookup sends nothing and is tried only on loopback | `TestTheMappedAddressIsTheSourceTheSystemSelectsForTheGateway` |
| Members see a public address not advertised, with no mapping asked for | `net check` prints the `advertise` line for a port forwarded by hand; none behind carrier-grade NAT | `TestNetCheckGivesTheAdvertiseLineForAPortForwardedByHand` |
| A listener whose only member connects out; then the window passes, the member is removed, and a key in no room handshakes | Reach is yes, naming the member, the time and the local address, while it connected in within 10 minutes; unknown after; a removed member's or a non-member's connection never counts | `TestAMemberThatConnectedInRecentlyMakesReachYesUntilTheWindowPassesOrItIsRemoved`, `TestOverTLSAMemberConnectingInMakesTheListenerReachableAndANonMemberDoesNot` |
| `net check` on that listener, and on one whose member has a known address that fails | The member is shown as connecting in and counted ("nothing to do"); outgoing connections are blamed only when a known address could not be reached; status and net check carry the same state | `TestNetCheckCountsAMemberThatConnectsInAndBlamesTheNetworkOnlyWhenAKnownAddressFailed`, `TestStatusAndNetCheckShowTheSameConnectedInState` |
| A machine's own new address list, an empty one | The new list replaces its own earlier word; third parties' stay; an empty list changes nothing | `TestAMachinesOwnNewListReplacesItsOldOneAndAnEmptyListChangesNothing` |
| A probe of HOST:PORT or an invite, to an a2p2 machine and to one with another key | A throwaway certificate is presented, never this machine's; none to a server whose certificate names another key ("certificate key mismatch"); no request follows | `TestAProbeShowsOnlyAThrowawayCertificateAndNoneToAWrongKey` |
| `a2p2 net check --invite TOKEN` and `HOST:PORT` through the command's own parsing, with the invite's key, another key, and a bad token | Reached with the key; a mismatch; the bad token refused before any dial | `TestNetCheckOfAnInviteOrAnAddressGoesThroughTheCommandsOwnParsing` |
| Local API call without the local key, or with a previous run's key (Windows; any system with the test option) | Refused with `not_allowed` before any handler, `stop` included; the key is new at each start and removed at stop; on Unix no key is written | `TestLocalAPICallWithoutTheLocalKeyIsRefusedBeforeAnyHandler`, `TestLocalKeyIsNewAtEachStartAndRemovedAtStop`, `TestDaemonWithoutTheLocalKeyOptionWritesNoKeyOnUnix` |
| Windows home that another user, Everyone, Users or Authenticated Users may open, that has no access list, or that another user owns | Refused; a home a2p2 created, or one with only the user, SYSTEM and Administrators, is accepted, also when SDDL writes the user as a constant such as `LA` | `TestWindowsHomeCreatedByA2p2IsAcceptedAsPrivate`, `TestWindowsHomeOfTheBuiltInAdministratorWrittenAsLAIsItsOwn`, `TestWindowsHomeOthersCanOpenIsRefused`, `TestWindowsHomeOwnedByAnotherUserIsRefused`; on Windows only, `TestWindowsNewHomeIsCreatedWithOnlyThisUserInItsAccessControlList`, `TestWindowsHomeEveryoneCanOpenIsRefused`, `TestWindowsSecondLockOnTheHomeFails`, `TestWindowsSocketPathTooLongIsRefusedWithAReason` (not yet run) |
| Install on Windows with a binary path that has spaces or characters shells read differently, or for Antigravity | Refused before anything is run or written; a plain path is written with forward slashes and recognised again by uninstall and doctor; OpenCode and Hermes keep the path as it is | `TestWindowsHookCommandIsOnePlainWordForEveryShell`, `TestWindowsInstallRefusesABinaryPathEachShellWouldQuoteDifferently`, `TestWindowsInstallSaysAntigravityIsNotEstablishedThere`, `TestWindowsListHarnessesKeepTheBinaryPathAsItIs`, `TestWindowsPiSkillNamesTheBinaryWithForwardSlashes`, `TestUnixInstallIsUnchangedByTheWindowsRules` |
| An agent sends with a file through the local API or the MCP tool | Nothing is written; a `send_files` request waits with every term; approved, the message and its file are written and the agent that asked gets a `request` entry (`sent`), shown once; declined, nothing is written and the entry says `declined` | `TestASendWithAFileWaitsForThePersonAndTheReaderFetchesIt`, `TestADeclinedSendWithAFileTellsTheAgentThatAsked`, `TestASendWithFilesWaitsForThePersonAndFetchReturnsAPathNotBytes` |
| A send names a relative path, a missing file, a directory, nine files, empty text or choices | Refused before anything is staged; no request is raised | `TestASendWithFilesRefusesBadPathsAndTooManyFiles` |
| A staged path holds U+202E or another bidirectional or control character; a replacement keeps a file | `requests`, `confirm` and the page show the path in full with each such character written `\u{...}`, the replaced message's id and first line, and the kept file as `sha256:<hex>` | `TestTheRequestEscapesBidiCharactersInThePathAndNamesTheReplacedMessage`, `TestEscapeDisplayWritesControlsAndBidiAsEscapes`, `TestConfirmShowsEveryTermOfASendWithFiles`, `TestThePageListsASendWithFilesWithEveryTermAndApprovesIt` |
| A reader fetches a file it was shown in its inbox, or only in a thread or history view | A verified copy's path, never its bytes; a view that is not the inbox allows the fetch but not a handled report; after a withdraw of a message it confirmed, `gone` with reason `withdrawn` | `TestAnAgentShownAFileInAThreadViewMayFetchItButNotReportItHandled`, `TestAFileShownInHistoryMayBeFetchedButNotReportedHandled`, `TestAFetchAfterAWithdrawOfAMessageTheAgentConfirmedSaysGone`, `TestFetchPrintsThePathAloneAndSaysItWasNotOpened` |
| Every agent tool driven with files and fetches, good and bad | Only agent calls (`fetch` among them); never `confirm`, `requests` or `room/files`; tool definitions 3,979 compact bytes | `TestEveryToolMakesOnlyAgentCalls`, `TestToolsGolden`, `TestToolDefinitionsFitTheTokenBudget` |
| The page's file and files-setting routes without the token, from another origin or host; a file that is not `here`; a file whose message the page never showed; an earlier copy changed on disk; a name with quotes, slashes or bidirectional marks | Refused as every page route; only a `here` file of a message the page showed (its history, thread or inbox view) is served, from a fresh verified copy, `application/octet-stream`, `attachment`, `nosniff`, exact bytes, sanitised and percent-encoded name; one never shown is refused `not_shown` | `TestFileRoutesRefuseWithoutTokenWithForeignOriginAndBadHost`, `TestTheFileRouteServesAHereFileAsAnAttachmentWithItsExactBytes`, `TestTheFileRouteDoesNotServeAFileThatIsNotHere`, `TestADownloadNameIsSanitisedAndPercentEncoded` |
| A person reads or sets a room's files setting; status | `off`, `carry` or `keep` recorded; any other refused; status shows each room's setting and bytes held, and the machine's | `TestRoomFilesSetsTheSettingAndStatusShowsTheBudgets`, `TestThePageReadsAndSetsTheRoomsFilesSetting` |
| `a2p2 room files ROOM MODE` without a terminal, or answered other than "yes" | Refused; the setting is unchanged; reading it needs no terminal | `TestRoomFilesChangesTheSettingOnlyAtATerminalAfterAYes` |
| A send names a missing path, a directory, a file over 256 MiB or the same bytes twice | One code and one message for all; the send's answer is the request id and `waiting` only; an internal file error is logged and answered without its path | `TestEveryPathThatCannotBeStagedGetsTheSameRefusal`, `TestASendWithAFileWaitsForThePersonAndTheReaderFetchesIt`, `TestSendWithAFilePrintsTheRequestAloneAndSendsAnAbsolutePath`, `TestAnInternalFileErrorIsLoggedAndAnsweredWithoutItsPath` |
| An approval of a send with files without the terms shown, or with others, through the local API, the page or `Rooms.Confirm`; a replacement's approval | Refused `terms_changed`, nothing decided; `Files.ApproveSend` with the terms shown is the only way to approve; the CLI and the page show the kept recipients, the version replaced and every line of the text escaped, and send the terms they showed | `TestAnApprovalWithoutTheTermsShownIsRefused`, `TestASendWithFilesIsApprovedOnlyWithTheTermsShown`, `TestThePageApprovesASendWithFilesOnlyWithTheTermsItShowed`, `TestConfirmRendersAReplacementWithItsRecipientsVersionAndWholeText`, `TestConfirmShowsEveryTermOfASendWithFiles` |
| The page's download raced with an in-place change after the open or after the check; a FIFO or a link put in place of the copy | Changed after the open: 404; after the check: the verified bytes; a FIFO or link: 404 without blocking; no snapshot left | `TestADownloadRacedWithAChangeServesOnlyVerifiedBytes`, `TestADownloadWithAFIFOOrALinkInPlaceOfTheCopyIsRefusedWithoutBlocking`, `TestTheFileRouteServesAHereFileAsAnAttachmentWithItsExactBytes` |
| On a reader's machine, a file shown only by history or the skipped view | Fetched; before any view, refused | `TestOnAReadersMachineAFileShownInHistoryOrSkippedMayBeFetched` |

Tests skipped on Windows, each with its reason in the skip message:
`TestLoadIdentity` after its first checks (file modes), `TestHookCmdShellSafe`
(runs `sh`), `TestWriteSymlinkAndMode` (file modes),
`TestConfigSymlinksAreFollowedAndDanglingOnesRefused` (only when the
account cannot make symbolic links) and
`TestDaemonWithoutTheLocalKeyOptionWritesNoKeyOnUnix` (Windows always
uses the key). The Windows job in `.github/workflows/ci.yml` (`what`: `windows`) also leaves out, with `-skip`, the deep schedules `Test*AgreesWithTheDeliveryModelOnEveryShortSchedule` and `Test*AgreesWithTheDeliveryModelOnRandomSchedules` (19 minutes on the runner; they test the delivery rules, not the system), and
lists every skip it meets.

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
up to 45 seconds for another member's agent to be shown the original, so
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

`make check` runs there with `SHARDS=2` for the runner's two cores.

The [release workflow](../.github/workflows/release.yml) runs when a tag
starting with `v` is pushed. On Linux, without a module or build cache, it
runs `go vet ./...`, checks that CHANGELOG.md has a section for the tag,
runs `make release VERSION=<tag>`, checks that the Linux binary reports
the tag, and publishes the binaries and `SHA256SUMS` as a GitHub release
with that section as its notes (a prerelease when the tag contains a
hyphen). It does not run the tests: they ran in `make prerelease` on the
same commit, which `make tag` checks (DECISIONS, D-cycle).

Before a tag, a maintainer runs `make prerelease` on their own machine:
`make check`, `make lint`, `make vuln`, `make check-linux` and one `make
chaos` run at its default length, all at the same time, each to its own
log in `.prerelease/`. It prints one line per step with its time and
exits non-zero naming every failed step; when every step passed on a
clean tree it writes `.prerelease/stamp`. `make check-linux` runs `go vet`
and the same sharded race tests on Linux in a Docker container, so a
macOS machine also tests Linux. `make chaos` runs real daemons in Docker
containers through kills, pauses and network splits (see
[Chaos runs of real daemons](#chaos-runs-of-real-daemons)).

## Optional checks

- `A2P2_MODEL_DEEP=1 go test ./internal/members ./internal/model/delivery ./internal/node`
  uses larger search bounds and includes measurements. The last recorded
  deep membership run took 33 minutes.
- `make model-sweep SEEDS="1 2 3"` runs the deep random differential tests
  of `internal/node` once per seed (`A2P2_MODEL_SEED`), about two minutes a
  seed, and keeps the log of each failing seed in `.model-sweep/`. A
  failure logs its operations; `A2P2_MODEL_REDUCE=<file>
  A2P2_MODEL_MATCH=<text> go test -run TestReduceAFailingSchedule
  ./internal/node` shrinks them to a schedule that still fails that way.
  On 2026-11-24 the deep real-exchange test was swept over seeds 1 to 26
  and the fixed seed. Before the harness was corrected, seeds 1, 2, 4 and
  6 failed (a head counted reachable that a withholding member held; an
  offer cut by a limit of one); after each correction the sweep was run
  again and found the next mismatches of the harness (seeds 5, 7, 8, 10,
  11, 13, 15, 17, 24), each reduced to a scenario in
  `internal/node/pull_seeds_test.go`. It now passes at every one of those
  seeds except 6 (a known gap of the code against the model) and 16 (a gap
  of the comparison); both are named there. The deep implementation test
  (`TestImplementationAgreesWithTheDeliveryModelOnRandomSchedules`) passed
  at seeds 1 to 12.
- `go test ./internal/core -run '^$' -fuzz FuzzParseRecord -fuzztime 20s`
  fuzzes the record parser. Use `FuzzParseEntry` for log entries or
  `FuzzDecodeToken` for invite tokens.
- `make e2e HARNESS=claude` uses a logged-in Claude Code CLI and spends model
  tokens. The end-to-end runner also accepts `codex`, `agy` or `all`. Its
  last recorded results predate the delivery rewrite; the
  [harness notes](HARNESSES.md) record a single-run check of the tools on
  the current code with each harness.

## Test gaps

- What the attachment differential test cannot catch: it has one message
  and one small file of one chunk on three machines over memory, so it
  says nothing of replacements, several files, carry and keep modes,
  uploads by an outbound-only author, budgets, restarts or a crash
  between steps; an event lands only at a fetch's first decision, never
  between two chunks of a body (the lease tests above cover that); it
  compares who received the file, not how often or from whom; and where
  the model and the code share a mistaken rule, both agree.
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
