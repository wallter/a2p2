# Testing strategy

The product is a delivery guarantee, so the tests are mostly about what happens
when things go wrong. A guarantee without a test does not exist. A feature is
not done until the test that would catch its regression exists and runs in
`make check`. `docs/TRACE.md` lists each requirement and the tests that prove
it; `make trace` fails if that register and `docs/REQUIREMENTS.md` disagree or
a named test does not exist.

All Go tests run with `-race`. Tests never touch the real `~/.a2p2` or real
harness configuration: everything takes `A2P2_HOME` and config paths as
parameters. Time and randomness are injected, so expiry and retry tests are
deterministic.

## Layers

| Layer | What it proves | Where | Run by |
|---|---|---|---|
| Rule models | Membership rules (who is a member and an admin, from signed log entries) and delivery rules (order, copies, handled reports, expiry, history) hold on every small schedule and on seeded random ones | `internal/members`, `internal/model/delivery` | `make check` |
| Signed formats | One spelling per record and entry; a relay cannot alter a signed field; hostile input is refused without panics | Golden vectors in `internal/core/testdata/vectors`, token vectors, fuzz targets `FuzzParseRecord`, `FuzzParseEntry`, `FuzzDecodeToken` | `make check` runs the seeds |
| Store | Every store method is one transaction; nothing half-applied after a failure or reopen; migrations from older databases | `internal/store` | `make check` |
| Node, differential | The real code gives the same answers as the delivery model, after every step, on every short schedule and on random ones, with lost responses and restarts | `internal/node/differential_test.go`, `pull_differential_test.go` | `make check` |
| Node, pull and join | Pulls, the scheduler, address book, requests, confirmation and the join exchange, over an in-memory transport that injects faults | `internal/node` | `make check` |
| Real TLS on loopback | Mutual TLS, strangers refused, body and rate limits, waiting pulls, dead addresses, joins between two machines | `internal/peer` | `make check` |
| Surfaces | Local API, MCP tools (golden file `internal/mcpshim/testdata/tools.json`), the page's server (token, `Host`, `Origin`), and two daemons on loopback | `internal/localapi`, `internal/mcpshim`, `internal/web`, `internal/daemon` | `make check` |
| Harness adapters | Install, uninstall and hook output never damage existing config | `internal/harness`, temp home directories | `make check` |
| Requirement register | Every requirement has a row and every test a row names exists | `scripts/trace.sh` | `make trace` (part of `make check`) |
| Deep searches | Larger bounds for the model searches and some measurements | tests that read `A2P2_MODEL_DEEP=1` | `A2P2_MODEL_DEEP=1 go test ./internal/members ./internal/model/delivery ./internal/node` (minutes; 33 minutes for `internal/members` in its last recorded run) |
| Fuzzing | Decoders accept only what they should | `go test ./internal/core -run '^$' -fuzz FuzzParseRecord -fuzztime 20s` (also `FuzzParseEntry`, `FuzzDecodeToken`) | by hand; see CI below |
| Harness end-to-end | Real Claude Code, Codex and agy call the tools and exchange messages | `scripts/e2e/run.sh` | `make e2e HARNESS=claude`; manual, spends model tokens; **not re-run since the rewrite** |

A model test shows that a rule is sound in the small executable model. It does
not show that the daemon follows the rule; the differential tests and the node
tests do that.

## Fault matrix

One row per fault. "Model" tests exercise `internal/members` or
`internal/model/delivery` only. Every other test runs the real code.

| Fault | What happens | Test |
|---|---|---|
| Lost response | The retry gives one copy, in order | `TestRealExchangeAgreesWithTheDeliveryModelOnEveryShortSchedule`; model `TestLostReplyRetryAndRestartsGiveOneCopyInOrder` |
| Author restarts | A new stream is written, linked to every head held; nothing forks | `TestEveryStartWritesANewStreamLinkedToEveryHeadHeld`, `TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules` |
| Relay restarts | Its database gives the same answers, and it serves again | `TestReopeningTheDatabaseGivesTheSameAnswers`, `TestARestoredServerPagesTheLogFromAMemberThenServesANewPeer` |
| Reader restarts between showing and confirming | The entries are shown again; handled is not duplicated | `TestCrashBetweenShowAndSeenShowsTheEntriesAgain`; model `TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped` |
| Restore from an older database copy | A restored server is healed by one resend; a restored admin signs nothing until it has pulled the log | `TestAServerRestoredWithFewEntriesMissingIsHealedByOneResend`, `TestARestoredDaemonSignsNothingForARoomUntilItHasPulledItsLog`, `TestConfirmationOfAnEntryLostToARestoreIsRefused` |
| Restored author whose newer holders are all asleep | A new stream is started without a fork | model `TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork` |
| Machine restored from a very old backup | Catches up from the room's record when it knows a current member | `TestJoinRestoredMachineThatKnowsNoCurrentMemberCatchesUp` |
| Two machines never awake together | Messages and handled reports pass through a third member that took a copy | `TestRelayCarriesAMessageAndItsHandledReportBetweenAnAuthorAndAReaderNeverAwakeTogether`, `TestTwoMachinesNeverAwakeTogetherExchangeMessagesThroughOffersToARelay` |
| Relay alters or strips text | An altered record is refused; stripped text is not believed and is repaired | `TestARelayThatAltersOrStripsTextIsNotBelieved`, `TestRelayCannotAlterAnySignedField`, `TestRelayMayDropTextAndTheHeaderStillVerifies` |
| Text expires; clocks differ | Expiry follows each holder's own clock and never resurrects text | `TestExpiredTextStaysExpiredWhenTheClockGoesBackOrTextIsOfferedAgain`; model `TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects` |
| Away longer than 30 days | The machine is told what is missing, then continues | model `TestAwayThirtyOneDaysIsToldWhatIsMissingThenContinues` |
| Machine added without history | It is never given earlier text, including through an offer | `TestANewcomerWithoutHistoryIsNeverGivenEarlierTextIncludingThroughAnOffer`, `TestMachineAddedWithoutHistorySeesNoEarlierTextThroughAnySurface` |
| Machine removed: refused | Every not-member refusal gives the same bytes | `TestARemovedMachineIsRefused`, `TestTheThreeNotMemberRefusalsAreTheSameBytes` |
| Machine removed: response held while it waited | The held response is not released | `TestAHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited` |
| Machine removed: its late messages | They can still reach members, marked as from a removed machine | `TestRemovedMachineMarksItsOwnRecordsAndGrantsOverTheHeadLimitAreRefused`; model `TestMessageOfARemovedAuthorReachesAMachineThatNeverMetItMarked` |
| Leave and rejoin | An old invite cannot undo a leave; a late response is not applied; nothing of the old generation is released | `TestJoinOldInviteCannotUndoALeave`, `TestLeaveThenALateResponseIsNotApplied`, `TestALeaveAndRejoinBetweenBuildAndReleaseReleasesNothingOfTheOldGeneration`, `TestJoinAgainAfterLeavingLocallySignsNoNewGrant` |
| Stolen admin key | Other admins remove it; one key cannot change the admin set once there are three admins | model `TestOneKeyCannotChangeAdminSetOnceThereAreThreeAdmins`, `TestCreatorKeyIsRemovableByTwoOtherAdmins`, `TestStolenKeyCompletingAPendingRemovalDecidesTheTargetsUnseenEntries` |
| Forking admin key | A fork is refused by machines holding the real entry and settled by another admin's removal | `TestForkInTwoAdminRoomIsSettledByTheOtherAdminsRemoval`; model `TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone`, `TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember` |
| Lost admin | Two of three replace it; with two, members stay manageable; with none, membership freezes | model `TestLostAdminOfThreeIsReplacedByTheOtherTwo`, `TestLostAdminOfTwoLeavesMembersManageableAdminSetFixed`, `TestAllAdminsLostFreezesMembershipNobodyPromoted` |
| Invite used by a second key | Refused | `TestJoinSecretPresentedByASecondKeyIsRefused` |
| Invite expired | Refused before anything is signed | `TestJoinExpiresBeforeSigning`, `TestJoinExpiryAtWriteTimeAndConcurrentAccepts` |
| Invite used after a leave | See leave and rejoin | `TestJoinOldInviteCannotUndoALeave` |
| Join declined by the admin's person | Refused on the joiner | `TestJoinDeclinedByTheAdminsPersonIsRefusedOnTheJoiner` |
| Wrong server key at join | The joiner stops before the secret leaves | `TestJoinWrongServerKeyStopsTheJoinerBeforeTheSecretLeaves`, `TestJoinOverTLSToTheWrongKeySendsNothing` |
| Confirming before the admin has caught up | Needs an explicit override, which is recorded | `TestConfirmBeforeCatchingUpNeedsTheExplicitOverrideAndRecordsIt` |
| Machine cannot accept connections | It converges through its own requests | `TestAMachineThatOnlyOpensRequestsConverges`, `TestOverTLSAMachineWithNoListenerConvergesThroughItsOwnRequests` |
| Dead first address | The second address is reached | `TestOverTLSAPeerWithADeadFirstAddressIsReachedOnItsSecond`, `TestOverTLSAStalledFirstAddressGivesTheNextAttemptToTheSecond` |
| Oversized request | 413 before the body is read | `TestOverTLSAnOversizedBodyGets413BeforeItIsRead`, `TestJoinRouteRefusesOversizeBodies` |
| Malformed request | 400; strict JSON | `TestOverTLSMalformedOrNonStrictJSONGets400`, `TestPullRequestsAreCheckedAtEveryLevel` |
| Response that does not fit its request | Refused before anything is applied | `TestAResponseThatDoesNotFitItsRequestIsRefusedBeforeAnythingIsApplied` |
| Too many requests | 429 with `Retry-After` | `TestOverTLSTheRateLimitGives429WithRetryAfter` |
| Peer that answers every wait at once | It is backed off | `TestAPeerThatAnswersEveryWaitAtOnceIsBackedOff` |
| Stranger connects | The handshake fails and its request is refused | `TestOverTLSAStrangersHandshakeFailsAndItsRequestIsRefused` |
| Networking off | Nothing is dialled; off until a person decides | `TestNetworkingOffDialsNothingAndOutboundDials`, `TestNetworkingIsOffUntilAPersonsDecisionIsRecorded` |
| Page request with wrong `Origin` or `Host`, or no token | Refused | `TestAuthHostOrigin`, `TestPageDecisionNeedsTheExactOriginAndShowsTheFullFingerprint` |
| Message text that looks like instructions | Shown as labelled peer data | `TestHostileStringsAreInert`, `TestInboxForgedHeadersStayInert` |
| A database write fails partway | The store leaves no half-applied write | `TestFailureBetweenStatementsLeavesNoHalfAppliedWrite`, `TestLogBatchThatFailsLeavesNothingApplied` (a failure inside a whole pull: no test) |
| Power loss | No test (restart and reopen are tested, a cut mid-write is not) | no test |
| Clock set back or ahead | Text that expired stays expired; replacement order follows the author's history, not its clock; a record dated over a day ahead waits | `TestTextThatExpiredBeforeItWasShownIsAnExpiredNoticeEvenAfterTheClockGoesBack`, `TestAFutureRecordLeavesNoSeenAndIsTakenOnceTheClockAllows`; model `TestNewestReplacementIsByTheAuthorsHistoryNotItsClock` |

## Acceptance

1. Two daemons on loopback pair, send, reply, acknowledge and trace:
   `TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace`, and the join over real
   TLS, `TestJoinOverTLSBetweenTwoMachines`.
2. A message reaches three machines over real TLS and its handled report
   returns: `TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns`.
3. A stranger can neither read nor write: `TestOverTLSAStrangersHandshakeFailsAndItsRequestIsRefused`.
4. The seven tool definitions do not drift and fit the budget: `TestToolsGolden`,
   `TestToolDefinitionsFitTheTokenBudget`.
5. Requirement by requirement: `docs/TRACE.md`.

## CI

GitHub Actions runs `make check`, `make lint`, `make fuzz FUZZTIME=30s`,
`make vuln` and `make release` (cross-compiles for linux and darwin). `make
fuzz` runs only `FuzzDecodeEnvelope`, the target for the old envelope; the new
targets named above are not fuzzed in CI, only run on their seeds by `make
check`. Harness end-to-end runs are not in CI because they need logged-in CLIs.
Every non-trivial change gets an independent read from another model family
(`make review`) before it is committed.

## Not tested

- The page's script in a browser. The page's server is tested; the script has
  never been run in a browser.
- Real harness runs since the rewrite (`make e2e`).
- Two physical machines on a LAN, and a packet capture of their traffic.
- Power loss or a kill at an arbitrary point (restarts are tested at chosen points).
- Throughput and memory. There are cost tests for message sizes in a large
  room (`TestPullSizesInASixtyFourMachineRoomAfterThirtyDaysOfDailyRestarts`)
  but no benchmarks, and no budget has been measured.
- Hook text reaching Codex and Antigravity models, the end-of-turn hook, and
  interactive harness sessions.
- A machine whose clock is wrong by days on a real network.
