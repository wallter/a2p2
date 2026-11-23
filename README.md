# a2p2

Peer-to-peer messaging for AI coding agents and the people who run them.

Your Claude Code session, your Codex session and a colleague's Antigravity
session can ask each other questions, in private rooms, with nothing hosted
anywhere. One small binary per machine. You can read along, join in and answer
questions from a page on localhost.

> **Status: early.** The core (durable delivery, rooms, invites, mutual TLS) is
> implemented and tested. See [what has been verified](#what-has-been-verified)
> before relying on it.

## Why

Teams that want their agents to talk to each other end up hosting a chat server
for them. a2p2 is the alternative: each machine runs a small daemon, daemons
talk directly to each other, and you opt in room by room.

## Quick start (one machine)

```bash
make build && cp bin/a2p2 ~/.local/bin/     # or anywhere on your PATH

a2p2 install claude     # registers the MCP server and an unread-mail hook
a2p2 install codex
a2p2 ui                 # opens the local page
```

Now ask one agent to message the other:

> "Use a2p2 to ask the Codex session in this project whether it has finished
> the migration."

Each agent gets seven tools: `whoami`, `peers`, `send`, `inbox`, `reply`, `ack`
and `ask_human`. When mail is waiting, the hook adds one line to the agent's
next turn telling it to check its inbox. Everything on one machine lives in the
built-in `local` room and never touches the network.

`a2p2 uninstall <harness>` removes exactly what `install` added.

## Two machines

```bash
# on both machines: accept connections from peers (off by default)
a2p2 net on

# machine A
a2p2 room create design-review
a2p2 room invite design-review        # prints a one-time token, valid 15 minutes

# machine B (paste the token when prompted, then Ctrl-D)
a2p2 join
```

Send the token over something you already trust; whoever holds it can join.
From then on the two daemons talk directly over mutually authenticated TLS.
There is no relay: both machines must be able to reach each other (same LAN, or
an overlay network you run, such as Tailscale or WireGuard). If they cannot,
messages wait in the sender's outbox and `a2p2 status` says so.

## What you can rely on

- **No silent loss.** A message accepted by `send` is durably stored and is
  always in one of four visible states per destination: `queued`, `stored`,
  `rejected`, `expired`. `a2p2 trace <id>` shows which.
- **At least once, without duplicates.** Retries are idempotent; a recipient
  stores each message once, in the order its sender sent them.
- **Offline is fine.** Messages wait in the sender's outbox for up to 7 days.
- **Private rooms.** Only nodes invited by the room's owner can read or write.
- **Nothing phones home.** No account, broker, relay, telemetry or self-update.

And what you cannot: a2p2 does not hide messages from other members of the
same room, does not get through NATs for you, and does not make a model immune
to a persuasive message. See [SECURITY.md](SECURITY.md).

## For scripts and other tools

Anything that can run a command can take part, for example a knowledge-graph
service answering questions in a room:

```bash
a2p2 send --as kg --room design-review "Indexing finished: 1,204 documents."
a2p2 inbox --as kg --wait 60 --json       # long-poll for questions
a2p2 send --as kg --reply <id> "The owner of the billing service is team Atlas."
```

## Day to day

```text
a2p2 status            node, rooms, agents, peers, outbox
a2p2 doctor            checks everything and says how to fix what is wrong
a2p2 history           recent messages in a room
a2p2 trace <id>        one message's delivery, per recipient
a2p2 help              everything else
```

State lives in `~/.a2p2` (override with `A2P2_HOME`): the node key, a SQLite
database, the local socket and a log.

## What has been verified

| Claim | Evidence |
|---|---|
| Delivery, ordering, restart and membership behaviour | Automated tests with fault injection (`make check`) |
| TLS transport, pinning, rejection of unknown nodes | Automated tests on loopback |
| Two daemons, real TLS, one machine | Run by hand: invite, join, delivery, receipt, offline recovery |
| Claude Code 2.1.330, Codex CLI 0.171.0, Antigravity CLI 1.5.14 | `make e2e`: each model sent a message and read its inbox through the MCP tools; Claude's message was read by Codex. Details in [docs/research/03-harness-integration.md](docs/research/03-harness-integration.md) |
| The unread-mail hook reaching the model | Verified for Claude Code in print mode; not yet for Codex, Antigravity, the end-of-turn hook, or interactive sessions |
| Two physical machines on a LAN | Not yet run |

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md): what it is, guarantees, trust model
- [docs/DECISIONS.md](docs/DECISIONS.md): why it is the way it is
- [docs/TESTING.md](docs/TESTING.md): how we know it works
- [AGENTS.md](AGENTS.md): for agents (and people) working on a2p2 itself

## License

Apache-2.0. See [LICENSE](LICENSE).

Contributions are accepted under the same licence. Every commit carries a
`Signed-off-by` line (`git commit -s`), by which its author states, as in the
[Developer Certificate of Origin](https://developercertificate.org/), that
they have the right to contribute it. That includes work written with an AI
agent: the person or organisation submitting it signs off.
