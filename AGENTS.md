# a2p2 — instructions for agents working in this repository

a2p2 is a small, local-first, peer-to-peer messaging tool for AI coding agents
and the humans who run them: one Go binary, one daemon per machine, private
rooms joined by explicit invite, durable at-least-once delivery, no hosted
infrastructure. It is built and maintained by AI agents (Claude Code leads;
Codex and Antigravity review and assist). Read `docs/DESIGN.md` before changing
behaviour and `docs/DECISIONS.md` before changing direction.

## Commands

```bash
make build                      # bin/a2p2 (static, no cgo)
make check                      # gofmt check + go vet + go test -race ./...   (what CI runs)
go test -race -run TestName ./internal/node    # a single test
go test -race -count=20 ./internal/node        # hunt a flaky test
make trace                      # requirement register matches the requirements and the tests
make fuzz                       # fuzz the envelope decoder (FUZZTIME=10m for longer)
make lint vuln                  # staticcheck, govulncheck (need network)
make e2e HARNESS=claude         # real harness end-to-end; spends model tokens
make review WHO=sol             # independent review of uncommitted changes by Codex
scripts/ask.sh sol|astra|luna|agy <prompt-file|->   # ask another model family
```

Try a change by hand without touching your real state:

```bash
export A2P2_HOME=$(mktemp -d) && bin/a2p2 status && bin/a2p2 send --as alice hi
```

## Architecture in one screen

```text
cmd/a2p2          CLI wiring only; no behaviour
internal/core     envelope, ids, limits, validation            (no I/O)
internal/store    SQLite: rooms, members, messages, deliveries, marks
internal/node     behaviour: send, inbox, ack, receive, outbox loop, invites, membership
internal/localapi JSON over HTTP over the Unix socket (server + client)
internal/peer     node key, mutual-TLS HTTP between nodes
internal/mcpshim  stdio MCP server; stateless, one local API call per tool
internal/harness  install / uninstall / hook output / doctor per harness
internal/web      loopback UI
internal/daemon   assembles the above; finds or starts the daemon
internal/members  who is a member and an admin of a room, from signed log entries
                  (pure; next version, not used by the daemon yet); FINDINGS.md
internal/model    executable model of the next version's delivery rules: the oracle
                  the real code is compared against; never imported by the daemon
```

Dependencies point one way: `core` ← `store` ← `node` ← everything else.
`node` knows nothing about HTTP, TLS or MCP; it talks to other nodes through
the three-method `node.Transport` interface, which tests replace with an
in-memory network that injects faults (`internal/node/node_test.go`).

Things that are easy to get wrong:

- **Every `store` method is one transaction.** Returning nil means committed.
  `store.Send` writes the message and one delivery row per destination node
  atomically; that is the at-least-once guarantee.
- **Identity comes from the connection, never the payload.** `node.Receive`
  overwrites `from_node` with the TLS-authenticated node id. Never add a code
  path that trusts a node id, agent id or room membership supplied by a peer.
- **The node is the security principal; agent labels are not credentials.**
- **Order per destination comes from having exactly one in-flight request per
  node** (`Mesh.kick`). Do not parallelise deliveries to the same node.
- **Reading is two steps:** `Inbox` returns unread messages, `Seen` confirms
  them. A hook or summary must never mark anything seen.
- **`a2p2 mcp` owns stdout** for the MCP protocol. Log to stderr only.
- **Message bodies never go in logs.** Ids, rooms, node fingerprints, sizes and
  timings only.

## Rules

1. **Simplicity is the feature.** Prefer deleting to adding. No new direct
   dependency, protocol message, MCP tool, CLI command, config key or database
   table without an entry in `docs/DECISIONS.md` saying why it is needed now.
2. **A guarantee without a test does not exist.** Delivery, ordering, security
   and compatibility claims need a test named for the scenario; add the row to
   the fault matrix in `docs/TESTING.md`.
3. **Tests never touch real state.** Use `t.TempDir()` and pass paths in; never
   read or write `~/.a2p2`, `~/.claude*`, `~/.codex`, `~/.gemini` from a test.
   Unix socket paths must be short (macOS limit ~100 bytes): use
   `os.MkdirTemp("/tmp", "a2p2")` for sockets.
4. **Do not claim what was not run.** Harness behaviour is "verified" only if
   an end-to-end script exercised that harness version; record the version in
   `docs/research/03-harness-integration.md`.
5. **Wire and schema changes are versioned.** Append to `store.migrations`;
   never edit a shipped migration. Envelope changes bump `core.ProtocolVersion`
   unless old nodes can ignore them.
6. **Get an independent read.** Before committing anything non-trivial, run
   `make review` (a different model family) and address or explicitly reject
   each finding. Reviewers get the diff, not your reasoning.
7. **Leave the docs true.** If behaviour changes, update `docs/DESIGN.md` in
   the same change. Decisions are appended to `docs/DECISIONS.md`, never edited.
8. **Sign off commits.** `git commit -s`; the licence is Apache-2.0 and
   contributions come in under the Developer Certificate of Origin.

## Where things are

- `docs/REQUIREMENTS.md` — draft requirements for the next version (awaiting the
  owner's confirmation); read with `docs/research/08-…` before changing the
  delivery core.
- `docs/TRACE.md` — one row per requirement: met or not today, and the tests
  that prove it. `make trace` checks it; update the row in the same change
  that adds a requirement or the test for one.
- `docs/DESIGN.md` — what a2p2 is today (v0.2), guarantees, trust model, surfaces.
- `docs/DESIGN-next.md`, `docs/PROTOCOL-next.md` — the next version, being
  built in the steps of DESIGN-next section 9; rules there cite the models.
- `docs/DECISIONS.md` — append-only decision log with reasons.
- `docs/TESTING.md` — test layers, fault matrix, acceptance criteria.
- `docs/research/` — the original brief, audits of prior art, harness facts,
  design reviews.
- `scripts/` — `ask.sh`, `review.sh`, `e2e/`.
