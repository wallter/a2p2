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

## What `a2p2 install` approves and writes

- `a2p2 install` approves no a2p2 tool by default, in any harness. Each
  harness's own rules decide whether an agent's `send`, `reply`, `ack` or
  `ask_human` waits for the person. As seen in the runs the
  [harness notes](docs/HARNESSES.md) list: Claude Code asks unless you
  allowed the tool, a headless Codex run refuses the call, and
  Antigravity's headless runs deny MCP tools that are not allowed.
  OpenCode's default agent runs every tool without asking, and Pi runs
  bash commands, the skill's `a2p2 send` among them, without asking (read
  from Pi's README, not run); a2p2 does not change either.
- `a2p2 install codex --approve-tools` lets every a2p2 tool run in Codex
  without a prompt, `send` included, and any tool a later a2p2 adds. Text
  from any member of a room, another owner's included, can then lead the
  agent to post to that room without the person seeing it first. Headless
  `codex exec` needs this. a2p2 0.1.0 to 0.2.3 wrote this approval on every
  Codex install without asking. The next `a2p2 install codex` removes it,
  and `a2p2 doctor` reports it as a fault until then. An agent can pass
  the flag itself: a flag cannot show that the person chose it.
- The team launcher (`scripts/team`) starts members with no person to
  answer prompts, so it approves every a2p2 tool for them. It refuses to
  start a Claude Code, Codex or Antigravity member unless the manifest
  says `"approve_a2p2_tools": true`, and its supervisor starts no such
  member whose saved command line was written without it.
- Pi's skill tells the model to send text only through a heredoc with a
  quoted end word, so bash runs nothing in it. Earlier skills put the text
  in double quotes, where bash runs `$(...)` and backticks: a peer's text
  that a model copied could run a command as you. The end word has eight
  random hex digits chosen at install, so a peer cannot know the line that
  would end the heredoc early. A model that ignores the skill can still
  run text in the shell. `a2p2 doctor` fails a skill an older a2p2 wrote.
- Hooks and MCP entries name the a2p2 binary by its path, and run it as
  you. Install refuses a binary in a temporary directory or Go's build
  cache, one that it or a directory above it lets every user change, and
  one owned by another user, unless a directory higher up keeps other
  users out. It checks the path as written and, for a symlink, where it
  leads now; for a path that does not exist yet, the part that exists. A directory its group can change gives a warning. Access
  control lists are not read. `a2p2 doctor` reports the same. On Windows this check is not
  made yet: keep `a2p2.exe` in a folder only you can change. A new folder
  directly under `C:\` can be changed by every signed-in user;
  [the install notes](docs/INSTALL.md) say how to change that.

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
