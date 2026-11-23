# Testing strategy

The product is a delivery guarantee, so the tests are mostly about what happens
when things go wrong. A feature is not done until the test that would catch its
regression exists and runs in `make check`.

## Layers

| Layer | What it proves | How | Status |
|---|---|---|---|
| **Unit** | Envelope validation, limits, ids, label cleaning | Table tests in `internal/core` | In `make check` |
| **Behaviour under faults** | Delivery, ordering, restart, membership, invites, receipts | `internal/node/node_test.go`: real stores in temp dirs, an in-memory network that drops responses, takes nodes down, garbles answers and delays joins | In `make check` |
| **Transport** | Mutual TLS, key pinning, rejection of bad certificates and strangers, body and connection limits, rate limiting, nothing readable on the wire | `internal/peer/peer_test.go`: real listeners on loopback | In `make check` |
| **MCP contract** | The shim speaks MCP and tool definitions do not drift | SDK client over an in-memory transport against a real node; golden file `internal/mcpshim/testdata/tools.json` | In `make check` |
| **Harness adapters** | Install, uninstall and hook output never damage existing config and produce exactly the expected shapes | Temp home directories and a recording command runner | In `make check` |
| **Web UI server** | Token, `Host` and `Origin` checks; no executable DOM sinks; answers and sends | `httptest` against a real node; a source scan for forbidden DOM APIs | In `make check` |
| **Fuzz** | Decoding untrusted envelopes never panics, and anything accepted survives a round trip | `go test -fuzz` in `internal/core` | `make fuzz`; 30 s in CI |
| **Harness end-to-end** | Real Claude Code, Codex and agy call the tools and exchange messages | `scripts/e2e/run.sh` runs each harness headless against a throwaway node and project | `make e2e`; manual, spends model tokens |
| **Property** | For any interleaving of send, deliver, lost response, duplicate, crash and restart, every accepted message ends in a terminal state exactly once per destination | State-machine test with `pgregory.net/rapid` over `internal/node` | **Not written yet** |
| **Browser** | The page behaves: room switching, answering, polling, dark mode | Scripted browser run | **Not written yet**; checked by screenshot only |
| **Two machines** | LAN pairing, direct encrypted traffic, nothing sent anywhere else | Manual checklist with packet capture, recorded in `docs/research/` | **Not run yet** |

All Go tests run with `-race`. Tests never touch the real `~/.a2p2` or real
harness config: everything takes `A2P2_HOME` and config paths as parameters.
Time and randomness are injected so retry and expiry tests are deterministic.

## Fault matrix

Each row has a test in `internal/node`, `internal/peer` or `internal/web`
unless marked otherwise.

| Failure | Required behaviour |
|---|---|
| Receiver down | Delivery stays `queued`; never `stored` |
| Response lost after receiver commits | Retry; receiver holds one row; sender reaches `stored` |
| Sender killed between commit and transmit | After restart the message is sent |
| Sender killed after transmit, before recording `stored` | After restart it resends; receiver deduplicates (covered by the lost-response and restart tests together) |
| Receiver's database write fails | Receiver answers `retry`; sender keeps `queued` (by code inspection; no test yet) |
| Node removed from room | Once a member has pulled the new list: queued deliveries to the removed node are `rejected`, its sends are `rejected`, it receives nothing further, and it drops the room |
| Unknown node connects | TLS succeeds, every room endpoint refuses |
| Invite reused or expired | Join refused; no membership change |
| Sender's clock jumps | `seq` still increases; ids stay unique; expiry follows the clock |
| Same harness, same project, restarted | Same mailbox: unread mail survives the restart |
| An agent registers after messages were sent | Earlier history is not unread for it |
| A new member's message reaches a node that has not pulled the new list | Held with `retry`, stored after the pull; never rejected |
| A member reuses another message's id | Rejected; the original is unchanged |
| Local `send` retried with the same id | One message |
| Message text that looks like instructions | Returned as labelled peer data only |
| UI request with wrong `Origin`, `Host`, or no token | Refused; no state change |
| Oversized body or batch | Refused before the body is read into memory |
| 100 messages each from two concurrent senders | 200 stored, per-sender `seq` increasing, no duplicates |

## Acceptance criteria (v1)

1. Claude Code and Codex exchange a message and a reply on one machine with no
   copy-paste; both are visible in the UI.
2. agy does the same using the same tools.
3. Two machines pair with an invite and exchange messages directly over TLS; a
   packet capture shows traffic only between the two hosts.
4. A message sent to an offline node arrives exactly once after it returns.
5. Killing either daemon at any point during send or acknowledgement loses no
   accepted message and duplicates none.
6. Concurrent senders: row 14 of the fault matrix.
7. A human posts in a room, receives an `ask_human` question, answers it, and
   the asking agent sees the answer. Nothing executes because of the question.
8. An unpaired LAN node can neither read nor write any room.
9. Mail for an idle agent is visible in the UI; each harness's delivery mode is
   documented as tested or untested, with the version.
10. Cross-origin and unauthenticated UI requests are rejected.

### Status (2026-11-22)

| # | Status |
|---|---|
| 1 | Met: `make e2e` with Claude Code 2.1.330 and Codex CLI 0.171.0 |
| 2 | Met: `make e2e` with agy 1.5.14 |
| 3 | Partly: two daemons over TLS on one machine, by hand and in tests. Two machines and a packet capture not done |
| 4 | Met: `TestOfflineRecipientGetsEverythingOnceInOrder`, and by hand with two daemons |
| 5 | Met for the cases tested: sender restart, receiver restart, lost responses. Not a random kill at arbitrary points (that is the unwritten property test) |
| 6 | Met: `TestConcurrentSenders` |
| 7 | Partly: server side tested (`internal/web`); the browser flow was seen in a screenshot, not exercised |
| 8 | Met: `TestStrangerAndWrongRoomAreRejected`, `StrangerIsRefused` |
| 9 | Partly: `pull` verified for all three harnesses; `next_turn` hook output is produced but its pickup by each harness is unverified |
| 10 | Met: `internal/web` tests |

## Budgets

Targets, **not yet measured** (there are no benchmarks in the tree): local
`send` p95 under 50 ms; 1,000 one-kilobyte messages across loopback TLS in
under a minute; idle daemon memory under 100 MiB. When measured, record
hardware and versions here. Reliability outranks throughput.

## CI

GitHub Actions on every push and pull request: `gofmt` check, `go vet`,
`staticcheck`, `go test -race ./...`, a 30-second fuzz smoke run, `govulncheck`,
and a cross-compile for linux and darwin on amd64 and arm64. Harness end-to-end
tests are not in CI (they need logged-in CLIs).

## Review

Every non-trivial change gets an independent read from a different model family
before it is committed (`scripts/review.sh`). Reviewers get the diff and the
relevant doc, not the author's reasoning.
