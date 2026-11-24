# Working on a2p2

a2p2 is a local-first messaging tool for agents and people. Each machine
runs a daemon and keeps copies of messages for its rooms. To install and
use it, follow the [README](README.md).

Read [the design](docs/DESIGN-next.md) and [protocol](docs/PROTOCOL-next.md)
before changing behaviour. Read [the decisions](docs/DECISIONS.md) before
changing direction. For delivery changes, also read the
[requirements](docs/REQUIREMENTS.md) and the models' findings in
[`internal/model/delivery`](internal/model/delivery/FINDINGS.md) and
[`internal/members`](internal/members/FINDINGS.md).

## Code map

| Package | Role |
|---|---|
| `cmd/a2p2` | CLI wiring |
| `internal/core` | Signed records and log entries, invite tokens, labels and limits |
| `internal/members` | Resolve membership and admin changes from signed entries |
| `internal/store` | SQLite records, room logs, requests, invites, peers and read state |
| `internal/node` | `Machine`, `Rooms`, `Joiner`, `JoinServer`, `Exchange`, `Scheduler` and the shared `Surface` |
| `internal/peer` | Machine keys and pinned mutual TLS for `/p/v2/join` and `/p/v2/pull` |
| `internal/localapi` | JSON API over the Unix socket |
| `internal/mcpshim`, `internal/harness` | MCP tools, harness setup and unread hooks |
| `internal/web`, `internal/daemon` | Loopback UI and daemon wiring |
| `internal/model/delivery` | Executable delivery model used in tests, outside the daemon |

## Rules for changes

- Keep transport details outside `internal/node`. Tests substitute an
  in-memory network through `PullTransport` and `JoinTransport`. Use the
  TLS peer key to identify the requesting machine. Verify the separate
  signatures on records and log entries. Agent labels carry no authority.
- Accept records only at the next position of the author's stream.
  Preserve transaction boundaries. Each send, application of a pull
  response or a request's log and offer, Seen, log signing and pruning pass
  commits as a unit.
- If `confirm` returns `not_caught_up`, signing needs explicit
  `not_caught_up_ok`. Record that decision.
- `Inbox` returns entries. `Seen` confirms the keys and versions shown.
  Hooks, summaries and thread views must leave read state alone.
- Keep MCP stdout for the protocol. Log to stderr and keep message bodies
  out of logs.
- Before adding a direct dependency, protocol message, MCP tool, CLI
  command, config key or database table, record why it is needed in
  [DECISIONS.md](docs/DECISIONS.md). Append decisions and migrations.
  Change the wire version for incompatible changes.
- Back delivery, ordering, security and compatibility claims with a named
  test. Update the fault matrix in [TESTING.md](docs/TESTING.md) and the
  requirement row in [TRACE.md](docs/TRACE.md) when its evidence changes.
  Update [DESIGN-next.md](docs/DESIGN-next.md) if behaviour changes.
- Keep tests off real state and harness settings. Use `t.TempDir()` for
  test state and short directories under `/tmp` for Unix sockets.
- Claim harness verification only after a real end-to-end run and record
  the tested version in the [harness notes](docs/research/03-harness-integration.md).
- Before committing a non-trivial change, get a diff-only review from a
  different model family. Address each finding or record why it was
  declined. Sign off commits as described in [CONTRIBUTING.md](CONTRIBUTING.md).
