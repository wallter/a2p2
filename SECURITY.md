# Security

## Report a vulnerability

Use GitHub's private vulnerability reporting for this repository. Include
the output of `a2p2 version`, your OS, and steps to
reproduce. Keep invite tokens, local UI links and private room content out of
reports.

## Trust and access

- A machine has an Ed25519 key. Room traffic uses mutually authenticated
  TLS 1.3 with pinned peer keys. A joining machine checks the admin's key
  against the fingerprint in the invite before sending the invite secret.
- A machine serves room data according to its current membership log and
  the requesting machine's history permissions. Until a removal reaches a
  member, that member may still serve the removed machine. Copies already
  received stay with their holders.
- Anyone with a valid unused invite can present it. A person on the admin
  machine still approves the joining machine and the terms. Keep invites
  private and verify the joining machine's fingerprint before approval.
- Files follow the permission of the text they belong to: a machine serves
  a file only to a current member that may read that message, and deletes
  it when the message goes. A person confirms the exact bytes of a file an
  agent sends, as a guard rail only: an agent running as your user can call
  `confirm` or paste the contents as text. a2p2 verifies a fetched file's
  hash and never opens, unpacks or runs it; treat it as data from another
  machine, and take care with whatever program opens it. A member that
  fetched a file keeps it after removal.
- The daemon's Unix socket is in a private directory. Any program running
  as your OS user can use the socket and read your machine's state.
  Messages are stored unencrypted in `a2p2.db` in the state directory.
- On Windows, file modes do not protect the socket. The state directory
  is created with an access control list for your user only, and a2p2
  refuses one that lets other users in. Each local API request must also
  carry a key that the daemon writes into that directory at every start.
  This has been built and reasoned about but not yet tested on Windows
  with a second user trying to connect (DECISIONS, D47).
- The web page listens on loopback. Its API requires a random token. The
  server checks the request's `Host` and any `Origin` header.
- Agent labels and addressees help with routing and reading. A room member
  can read the room data its machine can fetch. Treat messages as
  untrusted input. A message from a peer does not grant permission to act.
- The unread notice a hook prints holds a count and up to five sender
  labels, never message text. The sending agent chooses its label. Every
  machine accepts only 1 to 64 characters of letters, digits, `.`, `_` and
  `-`, and the hook drops any other character, so a label cannot carry
  a newline, quote or markup. It can still be a word that reads like an
  instruction; the notice tells the model that mail does not come from its
  user.

## What a2p2 connects to

a2p2 has no update check, no telemetry and no service of its own. The
daemon and the CLI make these connections and no others:

- To the other members of your rooms, only while networking is on or
  outbound only (`a2p2 net on`, which a person confirms at a terminal):
  pulls and file transfers, pinned to each member's key. When you accept
  an invite, a2p2 connects to the addresses in the invite you pasted,
  before the machine is a member. A member can also ask this machine to
  connect back to the address the member's own request came from, on at
  most two ports, for a TLS handshake and no request.
- To your router, only after `a2p2 net on --map-port`: UDP to the default
  gateway (PCP and NAT-PMP), UDP multicast to `239.255.255.250:1900`
  (UPnP discovery) and HTTP to the UPnP addresses the gateway names, if
  their host is the gateway. This opens a port to the internet.
- To an address you name: `a2p2 net check HOST:PORT` and `a2p2 net check
  --invite` make a TCP connection and a TLS handshake from the CLI under
  a throwaway key, whatever the networking state, and send no request.
- A host name in an address is looked up by your system's resolver.
- `install.sh` and `install.ps1`, which you run yourself and which are not
  part of the daemon, download one release and its `SHA256SUMS` from
  `github.com/wallter/a2p2/releases`.

Hooks and the MCP server talk only to the daemon over its Unix socket.

## A host that reaches your listening port

While networking is on, any host that can reach the peer port can open
connections and complete a TLS handshake under a key it makes up. Such a
key shares no room with you, so it reads nothing and joins nothing
without an invite and your approval. A connection is placed in a pool
only after its handshake has proved the client's key. What a host can
still do, by the numbers:

- Hold a few connection slots. One IPv4 address or one IPv6 /64 gets at
  most 8 connections that are not members' connections, one IPv6 /56 at
  most 16, and one IPv6 /48 at most 32. All keys that share no room get
  32 between them. Connections still in the TLS handshake get 64 shared
  slots for 5 s each, and a connection that finds no slot is closed at
  once. Members of your rooms have 256 slots of their own, at most 16
  for one key, and rate buckets of their own.
- Keep joiners out while it lasts. Joiners use the 32 slots of keys that
  share no room. Hosts in 4 source groups fill them: 4 IPv4 addresses,
  or 2 IPv6 /56s, or one /48. Members keep pulling.
- Keep members from opening new connections while it lasts. Hosts in 8
  source groups keep the 64 shared handshake slots full: 8 IPv4
  addresses, or 4 IPv6 /56s, or 2 /48s. Connections members already have
  keep working. A member whose address group completed a member's
  handshake here in the last 24 hours also has 16 handshake slots kept
  for such groups, which the flood cannot take. A member that connects
  from a new address, or for the first time since your daemon started,
  has no such slot and is refused until the flood stops.
- Spend join budgets of its own source only. Join requests and polls
  naming unknown invites are counted per IPv4 address or IPv6 /64, so one
  host cannot refuse joiners elsewhere or a joiner that resumes by the
  invite's hash. Each /64 can make 10 invite lookups a minute; a host
  with many /64s (a /48 has 65,536) can make that many more, bounded only
  by its connection slots, and refuses no one else.
- Not fill the log at the default level. Refused requests from keys that
  share no room, and refused connections, are logged at Debug; one Info
  line a minute says connections are being refused. A member's refused
  requests reach Info 5 at once, then one per 10 s per key. At Debug
  each refusal is a line, and `daemon.log` is not rotated.

A member of one of your rooms is trusted more. One member key can hold
16 connections, so 16 member keys together can fill the members' pool.
A host behind the same IPv4 address as a member, or in the same /64,
shares that member's per-source share. While it holds all 8, the member
cannot open a new connection; connections the member already has are not
affected.

## Release binaries

Release binaries are built by GitHub Actions from the tag, with no module
or build cache, with CGO off and with `-trimpath`. Each release carries a
`SHA256SUMS` file made in the same run; verify a binary against it before
installing (see the [README](README.md#install)). Releases after 0.2.3 also
carry a GitHub artifact attestation made by the release workflow. To check
one, run `gh attestation verify a2p2_<os>_<arch> --repo wallter/a2p2` (needs
the GitHub CLI). It proves which workflow and commit built the file, not
that the code is safe. Releases up to 0.2.3 have none. The binaries are not
code-signed or notarised.

See the [design](docs/DESIGN.md), [protocol](docs/PROTOCOL.md)
and [limits](docs/REQUIREMENTS.md#6-limits-in-plain-words).
