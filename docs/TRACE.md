# Requirement register

One row per requirement in `docs/REQUIREMENTS.md`. It answers two questions:
is this requirement met by the code today, and which test proves it. `make
trace` fails if a requirement has no row, a row has no requirement, a named
test does not exist, or a row says `met` without naming a test.

Status values:

- `met`: the tests named here exercise the requirement as written.
- `partly`: some of it is tested; the note says what is missing.
- `untested`: believed true from reading the code; no test proves it.
- `not yet`: not built.

A test under `internal/model` shows that a rule is sound in the small
executable model; it does not show that the daemon follows the rule, so it
never makes a row `met`. Such tests are listed after "Model:".

"New path:" lists tests of the next version's code in `internal/core`,
`internal/members`, `internal/store` and `internal/node` (`machine.go`,
`reading.go`, `rooms.go`, `pull.go`). That code is real but the daemon's tools, CLI and page do not
use it until build step 3b, so it does not change a row's status either.

"Due" is the step in section 9 of the requirements where the row must reach
`met`. Statuses below describe the v0.2 code on 2026-11-23, before the
delivery rework.

| ID | Status | Due | Tests and notes |
|---|---|---|---|
| D1 | partly | 2 | `TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace`. Restart is tested; power loss is not. New path: `TestFailureBetweenStatementsLeavesNoHalfAppliedWrite`, `TestReopeningTheDatabaseGivesTheSameAnswers` |
| D2 | not yet | 2 | Today only the author's machine delivers its messages. Model: `TestRelayDeliversAndReportsHandledWithoutAuthorAndReaderEverAwakeTogether`, `TestReportHeldOnlyByANoHistoryNewcomerSurvivesTheLossOfItsAuthor`. New path: `TestRelayCarriesAMessageAndItsHandledReportBetweenAnAuthorAndAReaderNeverAwakeTogether`, `TestTwoMachinesNeverAwakeTogetherExchangeMessagesThroughOffersToARelay` |
| D3 | partly | 2 | `TestConcurrentWritersOnTwoMachinesReachAThirdOnceAndInOrder`. One path only; several paths do not exist yet. Model: `TestLostReplyRetryAndRestartsGiveOneCopyInOrder`, `TestEverySmallScheduleKeepsTheDeliveryProperties`. New path: `TestCrashBetweenShowAndSeenShowsTheEntriesAgain`, `TestRecordIsStoredOnlyAtTheNextPositionOfItsStream`, `TestRealExchangeAgreesWithTheDeliveryModelOnEveryShortSchedule` (lost responses, restarts, several sources) |
| D4 | met | 2 | `TestOverTLSAMachineWithNoListenerConvergesThroughItsOwnRequests`, `TestConcurrentWritersOnTwoMachinesReachAThirdOnceAndInOrder`. Must be re-proven after the rework. Model: `TestSwitchingSourcesPastAHoleLosesSkipsAndReordersNothing`, `TestRestoredAuthorThatCatchesUpBeforeWritingKeepsItsOrder`. New path: `TestEveryStartWritesANewStreamLinkedToEveryHeadHeld`, `TestImplementationAgreesWithTheDeliveryModelOnRandomSchedules`, `TestSwitchingSourcesPastAHoleFillsTheStream`, `TestPullPageCarriesLinkedStreamsInOrder`, `TestABlockedStreamDoesNotHoldBackTheOthersInItsChunk`, `TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns` |
| D5 | partly | 2 | `TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace`, `TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns`, `TestInvalidHandledReportDoesNotHideTheMessage`. Reports go straight to the author, so they do not arrive when the two are never online together. Model: `TestHandledReportsOnlyFromAddresseesAndRoomWidePostsOnlyWhenAsked`, `TestTwoMachinesNeverAwakeTogetherExchangeNothingAndSenderSeesNoCopy`. Known gap: `TestAuthorLearnsOfCopiesOnMachinesItNeverMet`. New path: `TestHandledOnlyByAnAddresseeAfterItWasShownTheText`, `TestSeenRefusesEntriesInboxWouldNotShowAndHandledStaysTiedToShownText`, `TestAnOlderObservationNeverReplacesALaterOne`; known gap `TestKnownGapForkedStreamsLookEqualAndGiveAWrongCopyReport` |
| D6 | partly | 2 | `TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace`. No random-kill test and no network-split test. Model: `TestRandomSchedulesKeepTheDeliveryProperties`, `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork`, `TestMachinesReceivingSameEntriesInAnyOrderAgree`. New path: `TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules`, `TestARestoredServerPagesTheLogFromAMemberThenServesANewPeer`, `TestAServerRestoredWithFewEntriesMissingIsHealedByOneResend`, `TestJoinRestartOfEitherSideInEveryStateResumes`, `TestJoinRestoredMachineThatKnowsNoCurrentMemberCatchesUp`, `TestARoomLeftAndJoinedAgainBeforeTheSchedulerLooksIsPulledAgain`, `TestAWriteBetweenTheLookAndTheWaitIsOfferedAtOnce` |
| D7 | partly | 2 | `TestTextThatExpiredBeforeItWasShownIsAnExpiredNoticeEvenAfterTheClockGoesBack`. Today's periods are 7 days for delivery and 30 for storage, not the rule as written. Model: `TestAwayThirtyOneDaysIsToldWhatIsMissingThenContinues`, `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects`, `TestRelayThatStripsTextAndClaimsWithdrawalCannotLoseIt`. New path: `TestExpiredTextStaysExpiredWhenTheClockGoesBackOrTextIsOfferedAgain`, `TestHeaderWithoutTextIsMissingAndRepairedOnlyWhenPermitted` |
| D8 | partly | 2 | `TestRelayCannotAlterAnySignedField`, `TestRecordAndEntryCannotBeTakenForEachOther`, `TestGoldenVectors`, `FuzzParseRecord`: the signed formats exist in `internal/core`; the daemon does not use them yet (today the author is known only from the direct connection, `TestFromNodeComesFromTLS`). Model: `TestRelayThatAltersARecordIsRefused`. New path: `TestOnlyAVerifiedRecordCanBeStored`, `TestARelayThatAltersOrStripsTextIsNotBelieved`, `TestAResponseThatDoesNotFitItsRequestIsRefusedBeforeAnythingIsApplied` |
| D9 | not yet | 2 | Model: `TestWithdrawAndReplaceBeforeAndAfterTheReaderWasShown`, `TestWithdrawArrivingBeforeItsMessageMeansTheMessageIsNeverShown`, `TestWithdrawDropsTheTextOfReplacementsToo`. New path: `TestWithdrawDropsTheTextOfTheMessageAndOfItsReplacements`, `TestReplaceAndWithdrawOnlyOfThisMachinesOwnMessages`, `TestNoticesGoOnlyToAgentsWhoSawTheOriginal`, `TestAMessageWithdrawnBetweenBuildAndReleaseIsNotSent` |
| D10 | partly | 2 | `TestSendInboxReplyAckThroughTheDaemon`. Replies record parent and thread; reading one thread is not built. Model: `TestThreadsBranchesAndRepliesWhoseParentIsMissing`. Known gap: `TestBranchUnderAMessageIncludesRepliesBelowAMissingReply`. New path: `TestReplyGoesToTheParentsAuthorUnlessAddresseesAreNamed`, `TestThreadAndBranchFollowOnlyValidRepliesAndWaitLikeInbox` |
| T1 | met | 2 | `TestIngressRefusesWrongRoomSecondCreateAndBadSignature`, `TestOverTLSAStrangersHandshakeFailsAndItsRequestIsRefused`, `TestTheThreeNotMemberRefusalsAreTheSameBytes` |
| T2 | met | 2 | `TestCertIsNodeID`, `TestFromNodeComesFromTLS`, `TestPinningRefusesWrongServerKey`. New path: `TestOverTLSAStrangersHandshakeFailsAndItsRequestIsRefused` |
| T3 | partly | 2 | `TestInviteStatesAndExpiry`. No confirmation by a person, no choice about earlier history, and the token is over 300 characters. Model: `TestNewMachineInboxStartsEmptyAndHistoryFlagFiltersWhatMembersHandOver`, `TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations`. Known gap: `TestNewcomerWithoutHistoryIsNotHandedPostsItsAdmitterHadNotSeen`. New path: `TestANewcomerWithoutHistoryIsNeverGivenEarlierTextIncludingThroughAnOffer`, `TestACursorIsSeededOnlyWhenEveryHeadTheAdmissionNamesIsHeld`, `TestJoinPreviewShowsTheRoomAndNothingIsSignedUntilTheAdminsPersonConfirms`, `TestJoinWrongServerKeyStopsTheJoinerBeforeTheSecretLeaves`, `TestJoinSecretPresentedByASecondKeyIsRefused`, `TestJoinExpiresBeforeSigning`, `TestJoinOldInviteCannotUndoALeave`, `TestJoinOverTLSBetweenTwoMachines`, `TestTokenVectors` (the token is 52 characters plus addresses) |
| T4 | partly | 2 | `TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns`. The member list is fetched only from the room's owner |
| T5 | met | 2 | `TestARemovedMachineIsRefused`, `TestJoinerRemovedLaterIsToldRemoved`, `TestJoinOldInviteCannotUndoALeave`. Model: `TestRemovedAdminsConcurrentRemovalOfCompromisedMachineStands`, `TestConcurrentAddAndRemoveOfSameMachineRemovalWins`, `TestMessageOfARemovedAuthorReachesAMachineThatNeverMetItMarked`, `TestHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited`, `TestStaleViewHandsTextToARemovedMachineUntilTheServerLearns`. New path: `TestARemovedMachineIsRefused`, `TestAHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited`, `TestTheThreeNotMemberRefusalsAreTheSameBytes`, `TestALeaveAndRejoinBetweenBuildAndReleaseReleasesNothingOfTheOldGeneration`, `TestJoinerRemovedLaterIsToldRemoved` |
| T6 | not yet | 3 | |
| T7 | met | 2 | `TestWireIsEncrypted`, `TestServerRejectsBadClientCerts` |
| T8 | met | 2 | `TestHostileStringsAreInert`, `TestInboxForgedHeadersStayInert`, `TestNoticeSanitisesLabels` |
| T9 | not yet | 2 | Today a room has one owner. Model: `TestOneKeyCannotChangeAdminSetOnceThereAreThreeAdmins`, `TestSeededRoomsKeepEveryProperty`, `TestLostAdminOfThreeIsReplacedByTheOtherTwo`, `TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone`. New path: `TestOnlyVerifiedEntriesReachTheMachine`, `TestJoinBootstrapThreeMachinesFromNothingThenAFourthAsAMember`, `TestJoinRequestFailsWhenTheAdminCountChanged`, `TestAnExchangeWhoseAdvertisedHeadsAreNotAcceptedDoesNotUnlockSigning`, `TestConfirmBeforeCatchingUpNeedsTheExplicitOverrideAndRecordsIt` |
| T10 | untested | 2 | Nothing promotes a machine today; needs a test once there are several admins. Model: `TestAllAdminsLostFreezesMembershipNobodyPromoted` |
| U1 | partly | 3 | `TestReplyGoesToTheParentsAuthorUnlessAddresseesAreNamed`, `TestSurfaceRefusesForeignAgentsAndUnknownAddressees`. Addressing works; there are no levels to choose |
| U2 | not yet | 3 | |
| U3 | partly | 3 | `TestNewcomersAgentsStartAtTheAdmittersPositionsWithOrWithoutHistory`. No way to read what was skipped. New path: `TestAgentRegisteredLaterSeesOnlyWhatComesAfter` |
| U4 | partly | 3 | `TestHookOutputs` covers the unread count. No first-lines reading |
| N1 | not yet | 2 | Today both machines must accept connections. Model: `TestMachineThatOnlyOpensRequestsStillConverges`. New path: `TestAMachineThatOnlyOpensRequestsConverges` (in-memory transport; not yet over a real network), `TestOverTLSAMachineWithNoListenerConvergesThroughItsOwnRequests` (real TLS on loopback), `TestNetworkStatesAfterARestart`, `TestANetworkingDecisionIsAppliedByTheDaemonItself`, `TestAnAddressThatCannotBeBoundLeavesAWorkingDaemonThatSaysSo`, `TestTheLatestDecisionIsTheOneCommittedLast` |
| N2 | untested | 2 | Several addresses are tried in turn; no test. New path: `TestOverTLSAPeerWithADeadFirstAddressIsReachedOnItsSecond`, `TestOverTLSAStalledFirstAddressGivesTheNextAttemptToTheSecond` |
| N3 | untested | 2 | True by construction; no test |
| A1 | met | 2 | `TestToolsGolden`, `TestSurfaceSendInboxSeenReplyAckThreadHistoryTrace`, and `make e2e` for the three harnesses |
| A2 | partly | 2 | `TestToolsGolden` fixes the definitions (about 800 tokens); nothing asserts the limit |
| A3 | partly | 2 | `TestAskHumanArrivesForTheHumanAndTheAnswerIsAReply`, `TestQuestionAnswerFlow`, `TestSendAsHuman`, `TestSurfaceInboxWaitReturnsWhenAMessageArrives`, `TestQuestionsForThePersonAreListedUntilAnsweredWhateverTheBacklog`, `TestAReplacementOfAQuestionIsAQuestion`, `TestAlertsListQuestionsWaitingForThePerson`, `TestNoMissingAlertForTextWhoseReadabilityIsUndecided`, `TestNoMissingAlertBeforeTheReadersStartOrPastRetention`. No confirming of requests; the browser flow is not exercised |
| A4 | partly | 2 | `TestTraceSavedMeansTheHeaderIsHeldAndAnUnknownIDIsAnError`. Reasons for non-delivery are shown; copies on other machines are not. Model: `TestResolveGivesEveryEntryAReasonOnArbitraryInput` |
| I1 | partly | 1 | `TestInstallFresh`, `TestDoctor`. Not yet shown that an agent can install from the repository's instructions alone |
| I2 | met | 1 | `TestExistingSurvivesIdempotentOnStopUninstall`, `TestUninstallFreshLeavesEmptyObject`, `TestOwnershipIsExact` |
| I3 | partly | 1 | `AGENTS.md` exists; no portable skill, no recipes |
| I4 | untested | 1 | One static binary, two direct dependencies; licences are not checked automatically |
| I5 | not yet | 2 | |
| I6 | untested | 1 | True from reading the code; no test |
| L1 | not yet | 5 | |

Principles P1–P6 and the limits in section 6 are not rows. Each limit in
section 6 becomes a scenario in step 0 that shows the limit behaving as
described.
