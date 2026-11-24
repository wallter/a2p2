# a2p2 design

> **Out of date (2026-11-24).** This file describes v0.2, whose delivery
> core has been replaced. The current design is
> [`DESIGN-next.md`](DESIGN-next.md) and the exact formats are in
> [`PROTOCOL-next.md`](PROTOCOL-next.md). Those two replace this file once
> the old code has been deleted (in progress).

Status: v0.2 (2026-11-22), revised after two independent design reviews
(kept in the project's working notes, not in this repository). This is the source of truth for what a2p2 is and
is not. Decisions and their reasons live in [DECISIONS.md](DECISIONS.md); how we
prove it works lives in [TESTING.md](TESTING.md).

## 1. What it is

a2p2 lets independently running AI coding agents (Claude Code, Codex,
Antigravity, anything that speaks MCP or can run a CLI) and the humans who run
them exchange messages in rooms, on one machine or directly between machines,
with no hosted server.

One small binary. One daemon per machine. Rooms are private to the nodes that
were explicitly invited. Messages are durable and delivered at least once.

### Goals

1. Install once, and agents in different harnesses on one machine can talk.
2. Two machines exchange messages directly and encrypted after one explicit
   invite/join step. No account, broker, relay or directory.
3. A message accepted by `send` is never silently lost: it is delivered, or it is
   visibly `queued`, `rejected` or `expired`.
4. A human can see everything their node sees, post, and answer questions, from a
   localhost page.
5. Small enough that one agent can hold the whole codebase in context.

### Non-goals (v1)

- No orchestration: a2p2 never runs another agent's tools and has no
  remote-execution surface. Messages are data.
- No content filtering, redaction or secret scanning.
- No global ordering, consensus, CRDTs, DHT, gossip or third-party forwarding.
- No guaranteed WAN reachability. If two nodes cannot open a direct TCP
  connection (or a user-supplied overlay such as Tailscale/WireGuard), they are
  `unreachable` and we say so.
- No end-to-end secrecy between members of the same room; a room is the privacy
  boundary.
- No A2A protocol, attachments, edits/retractions, or history backfill for late
  joiners. These are post-v1 and must not shape the v1 core.

## 2. Shape

```text
 machine A                                   machine B
 ┌──────────────────────────────────┐        ┌──────────────────────────────────┐
 │ Claude Code ─ a2p2 mcp ─┐        │        │        ┌─ a2p2 mcp ─ Codex        │
 │ Codex ─────── a2p2 mcp ─┤ unix   │        │  unix  ├─ a2p2 mcp ─ Antigravity  │
 │ scripts ───── a2p2 CLI ─┤ socket │        │ socket ├─ a2p2 CLI ─ knowledge    │
 │                         ▼        │  mTLS  │        ▼             graph        │
 │                   a2p2 daemon ◄──┼────────┼──► a2p2 daemon                    │
 │   identity · rooms · SQLite ·    │ TCP,   │                                   │
 │   outbox · web UI (127.0.0.1)    │ direct │                                   │
 └──────────────────────────────────┘        └──────────────────────────────────┘
```

- **Daemon** (`a2p2 daemon`): owns the node identity key, the SQLite database,
  room membership, the outbox retry loop, the peer listener and the web UI.
  Started on demand by any other subcommand; single instance per `A2P2_HOME`.
- **MCP shim** (`a2p2 mcp`): a stdio MCP server the harness launches. It holds no
  state; each tool call is one local API request.
- **CLI** (`a2p2 send|inbox|room|...`): the same local API for humans, scripts and
  non-MCP participants (for example a knowledge-graph service).
- **Hook** (`a2p2 hook`): prints a one-line unread notice for harness hooks.

Everything a2p2 owns lives in `A2P2_HOME` (default `~/.a2p2`, mode 0700):
`identity.key`, `a2p2.db`, `sock`, `config.json`, `daemon.log`.

## 3. Trust model

There are exactly two principals.

| Principal | Authenticated by | Authorised to |
|---|---|---|
| **Node** (one per machine/user) | Ed25519 key; mutual TLS 1.3 on every peer connection | Send to and receive from rooms it is a member of |
| **Local process** (agent, CLI, browser) | Access to the 0700 Unix socket; UI token for the browser | Everything the node can do |

Consequences, stated plainly:

- The node is the security boundary. All agents on a node see all of that node's
  rooms. Agent names are labels that prevent inbox crosstalk; they are not
  credentials. A process running as your OS user can already read the database.
- A peer connection proves which node is talking. Every peer request is then
  checked against room membership. Room names and ids are never credentials.
- Because nodes only ever talk directly (no forwarding), connection
  authentication is sufficient and messages carry no per-message signature.
- Every received message is untrusted input. Tool output labels it with its
  origin and never presents it as an instruction from the user or system.

## 4. Rooms and pairing

- A room has an opaque random id, a label, and an **owner** node (its creator).
- The owner is the only writer of the member list, which carries a version
  number. Every other member **pulls** the list from the owner over a connection
  authenticated as the owner: once a minute, on reconnect, and whenever a peer
  shows evidence of a newer version. No signatures, no voting, no push queue.
  If the owner is offline, messaging continues on the last known list; only
  membership changes wait.
- **You are trusting the owner** with who is in the room. An owner can admit
  anyone. Membership changes are logged and visible in `a2p2 status`.
- Every node has a built-in room `local` that never leaves the machine. This is
  the zero-configuration single-machine experience.
- Networking is off until `a2p2 net on`. Until then the node has no listener.
- **Invite**: `a2p2 room invite <room>` prints a one-time token containing the
  room id, the owner's node id and addresses, a 128-bit secret and an expiry
  (15 minutes). The owner stores only a hash of the secret.
- **Join**: `a2p2 join` (token on stdin, so it stays out of shell history) dials
  the owner, completes a TLS handshake pinned to the owner key in the token, and
  only then presents the secret. The owner consumes the invite atomically and
  binds it to the joiner's TLS key; the same key retrying gets the same answer,
  any other key is refused. Both humans acted explicitly; that is the pairing
  ceremony. Each side is shown the other's key fingerprint to compare.
- The token is a bearer secret: whoever holds it first can join, and a swapped
  token points the joiner at someone else's room. Send it over a channel you
  already trust (the same one you would use to tell a colleague a password).
- **Remove**: the owner publishes a member list without that node. Removal takes
  effect on each member when that member next pulls the list: it rejects its
  queued deliveries to the removed node, stops sending to it, and refuses its
  messages. Removal cannot recall what was already copied. A removed node
  learns of it when the owner refuses its next pull, and drops the room.
- **Addresses** are dial hints, never credentials. A member reports its current
  addresses each time it pulls the list; `advertise` in the config overrides
  detection (for an overlay such as Tailscale). A node always completes the
  pinned TLS handshake before sending application data, and never follows
  redirects. There is no discovery protocol.
- Both sides must be able to accept connections from each other. If only one
  can, messages in the other direction stay `queued` and `a2p2 status` says the
  peer is unreachable.

## 5. Messages and delivery

### Envelope (wire and storage)

```json
{
  "v": 1,
  "id": "0193a5c2-...",            // UUIDv7, chosen by the sender
  "room": "r_9f2c...",
  "from_node": "ab12cd34...",      // overwritten from the authenticated connection
  "from": "claude-api@ab12cd34",   // agent label @ first 8 characters of its node id
  "to": ["codex-web@cd34ef56"],    // empty = everyone in the room
  "kind": "message",               // message | question | answer | receipt
  "reply_to": null,
  "thread": null,                  // id of the first message of the conversation
  "seq": 42,                       // sender node's counter, assigned in the send transaction
  "created_at": "2026-11-22T19:00:00Z",
  "expires_at": null,
  "body": {"text": "Can you review the API contract?"}
}
```

`to` is addressing, not access control: every member node of the room receives
every message, exactly like a chat room with mentions. Limits: 16 KiB body,
32 recipients, 64 nodes per room, 100 envelopes per batch; enforced before
allocation. A request is just a message; a response is a message with
`reply_to`. `question` is for a human and `answer` can only be produced by the
human's UI or CLI.

### Guarantees

- **At least once, idempotent.** `send` commits the message and one delivery row
  per member node (the membership at that instant) in one transaction, then
  returns. The outbox POSTs batches to each node and marks a delivery `stored`
  only when the receiver replies that its own transaction committed. The
  receiver treats a repeat of a message it holds as success; the same id with a
  different author, room or sequence number is rejected, so one member cannot
  overwrite or falsely confirm another's message.
- **Local sends are retry-safe.** The caller may supply the message id; sending
  the same id again returns the original message.
- **Per-sender order.** Exactly one request per destination node is in flight,
  batches go oldest first, and a retry always restarts from the oldest
  unconfirmed message. A node therefore stores each sender's messages in `seq`
  order. `seq` increases but may have gaps (rejections, expiry, messages to
  other rooms). There is no order across senders; views sort by local arrival.
- **Per-envelope results.** The receiver answers each envelope `stored`,
  `rejected` (permanent: invalid, not a member, id conflict) or `retry` (not
  now). A rejection never blocks the messages behind it; a `retry` stops the
  batch so order is preserved.
- **No silent loss.** A delivery is always in exactly one of `queued`, `stored`,
  `rejected` or `expired`. `expired` means the retry deadline (7 days) passed
  without confirmation; the recipient may or may not hold the message. Retries
  back off exponentially with jitter to a 5 minute cap.
- **Offline peers.** The message waits in the sender's outbox. There is no
  store-and-forward through third nodes; if the sender is off, nothing moves.
- **Late joiners get no history**, and a new member's first messages are held
  with `retry` by members that have not yet pulled the new list, not rejected.

### Receipt levels

| State | Scope | Meaning |
|---|---|---|
| `queued` | per destination node, on the sender | Durable on the sender |
| `stored` | per destination node, on the sender | The destination committed it |
| `seen` | per agent, local to the reader's node | Handed to that agent by `inbox` or opened in the UI |
| `handled` | per agent, sent back to the author's node | The recipient called `ack` or replied |

Only `handled` crosses the network, as a `receipt` envelope addressed to the
author's node alone (not the room). A receipt is accepted only for a message
the receiving node actually sent. Receipts never generate receipts, and
`stored` never implies that a model read anything.

Reading is two steps: `inbox` returns unread messages and the caller confirms
with `seen` once they have been handed over, so a lost tool result is shown
again rather than lost.

### How an agent learns it has mail

| Mode | Mechanism | Claimed only when |
|---|---|---|
| `pull` | Agent calls `inbox`; `inbox(wait_seconds)` long-polls | Always works |
| `next_turn` | Harness hook prints a one-line "N unread" notice | Hook verified for that harness |
| `wake` | A documented harness API starts a turn in an idle session | Verified live for that harness version; opt-in |

The notice never contains message bodies and never marks anything seen; the
agent reads the actual text through `inbox`, where it is labelled as peer data.
a2p2 never interrupts a busy turn. The end-of-turn (`Stop`) hook can start
another model turn, so it is installed only with `a2p2 install <harness>
--on-stop`.

**Which mailbox is mine.** An agent's label defaults to
`<harness>-<project directory name>` and can be set with `--name` or
`A2P2_NAME`. The same harness in the same project is therefore one mailbox
across restarts, and a hook finds its mailbox from the harness name and working
directory alone. Two simultaneous sessions of the same harness in the same
directory share that mailbox unless given distinct names.

## 6. Surfaces

### MCP tools (seven)

| Tool | Purpose |
|---|---|
| `whoami` | This agent's label, node id, rooms, delivery mode |
| `peers` | Rooms, member nodes, known agents, reachability |
| `send(text, room?, to?)` | Queue a message; returns id immediately |
| `inbox(room?, wait_seconds?, limit?)` | Unread messages for this agent, bounded; can long-poll |
| `reply(message_id, text)` | Send with room, thread and recipient filled in |
| `ack(message_id)` | Mark handled |
| `ask_human(question, choices?, room?)` | Create a pending question for a human |

### CLI

`install|uninstall <claude|codex|agy>`, `ui`, `doctor`, `status`, `send`,
`inbox`, `history`, `ack`, `trace <message-id>`, `net on|off`,
`room create|invite|remove|list`, `join`, `daemon`, `stop`, `mcp`, `hook`.
Most accept `--json`. `--as <label>` acts as a named script instead of the
human, which is how a non-MCP participant such as a knowledge-graph service
takes part.

### Web UI

Loopback only. Rooms, message stream with per-recipient delivery state, compose
as `human`, pending questions with answer buttons, peers and reachability,
diagnostics. Every API call needs a random token that is new each time the daemon
starts (`a2p2 ui` prints a link carrying it in the URL fragment, which browsers
do not send or log); `Host` and `Origin` are
checked on every request; no CORS; peer-supplied text is only ever rendered as
text. Static HTML plus one vanilla JS file that polls; no build step.

### Harness adapters

An adapter is: how to register the MCP server, which hook to install for
`next_turn`, optionally how to `wake`, and how to undo all of it. Installation
backs up the file it edits, changes only its own entries, and is reversible with
`uninstall`. Verified facts per harness are in
[HARNESSES.md](HARNESSES.md).

## 7. Observability

- **The database is the trace.** Every delivery row records state, attempt count,
  last attempt time and last error. `a2p2 trace <id>` prints a message's full
  lifecycle across recipients.
- **`a2p2 status`**: node id, rooms, per-peer reachability and last contact,
  outbox depth, age of the oldest queued delivery, connected local agents.
- **`a2p2 doctor`**: checks the daemon, socket and key permissions, database
  integrity, each installed harness adapter, the listener port, and each peer,
  and says what to do about each failure.
- **Logs**: structured JSON (`log/slog`) to `daemon.log` with message id, room,
  node fingerprints, sizes, attempts and latencies. Message bodies are never
  logged.

## 8. Implementation

Go, one static binary, no cgo. Direct dependencies are limited to the official MCP
SDK and a pure-Go SQLite driver (plus a property-testing library in tests);
everything else is the
standard library (`crypto/tls`, `crypto/ed25519`, `net/http`, `log/slog`,
`embed`). Both the local API (over the Unix socket) and the peer API (over mTLS)
are plain HTTP+JSON, so one request/response stack serves agents, CLI, UI and
peers, and a stored receipt is simply the HTTP response.

```text
cmd/a2p2          entry point and CLI wiring
internal/core     envelope, ids, validation, limits (no I/O)
internal/store    SQLite schema, migrations, queries
internal/node     the daemon's behaviour: send, inbox, rooms, outbox loop
internal/localapi HTTP over the Unix socket: server and client
internal/peer     identity, mTLS transport, peer endpoints
internal/mcpshim  stdio MCP server
internal/harness  install / uninstall / doctor per harness
internal/web      UI handlers and embedded assets
internal/daemon   assembles the above; finds or starts the daemon
```

Dependency direction is one way: `core` ← `store` ← `node` ← everything else.

## 9. Milestones

| | Scope | Exit test |
|---|---|---|
| **M1** | Daemon, store, local API, MCP shim, CLI, `local` room, hook, Claude Code and Codex adapters, read-only UI | Real Claude Code and Codex sessions exchange a message and a reply on one machine, driven headlessly by a script |
| **M2** | Identity, mTLS peer transport, rooms, invite/join, outbox retry, receipts, `status`/`trace` | Two daemons on loopback and on two machines pass the fault matrix in TESTING.md |
| **M3** | Antigravity adapter, UI compose and `ask_human`, `doctor`, security tests, release build | All ten acceptance criteria in TESTING.md pass |
| **Later** | Wake integrations, notifications, optional relay/overlay guidance, A2A adapter, attachments | Only on demonstrated need |
