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
- An admin can sign entries that wait for a second admin, and each other
  admin's machine asks its person about them. It asks about at most four
  per signing admin at a time, so one admin cannot fill another's queue.
  The requests a person starts have a budget of their own, and a removal
  of an admin passes it, so a person can always propose removing an
  admin. On the other admins that removal passes the seconding budgets
  too, once per proposing admin and target. A hostile admin can still
  make the other people decline its entries, four at a time.
- When the daemon starts, it checks each room's stored log for form and
  signature only, not for receipt rules added since the entries were
  stored. A room whose stored log does not load is set aside: it is not
  served or pulled, nothing can be written to it, setting it aside
  deletes nothing, and `a2p2 status` and `a2p2 doctor` show it. The other
  rooms keep working, so an entry another machine got stored cannot stop
  the daemon starting after an upgrade.
- A machine that reaches a room through one member trusts that member for
  delivery. That member can leave out an author's messages without any
  sign, and a removal of that member never reaches the others while it is
  the only path. Rooms shared between owners should have a listener from
  each owner.
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
- Room labels, machine names, file names and message text can come from
  another machine, and its owner may be someone else. Where a2p2
  shows them (the text a person approves in `confirm`, `alerts` and on
  the page, the join preview, the CLI's messages, alerts, room lists,
  status, `net check` (members' addresses and probe details among
  them), `confirm`, `join`, `fetch`, doctor and error lines, the page,
  and the text of MCP tool results)
  it writes control characters, line breaks where one line is shown,
  terminal escapes, bidirectional overrides and characters that draw as
  nothing as `\u{...}`. Nothing is dropped, and a backslash that would
  start such an escape is itself written `\u{5c}`, so typed text never
  reads as an escaped character. So a room creator cannot rewrite or
  hide the terms of a request with its room's label. Some invisible
  characters are kept where honest writing needs them and they change
  no order: joiners and selectors inside emoji, joiners between letters
  of the scripts that need them, a right-to-left mark between two
  right-to-left letters, a zero width space between two Thai, Lao,
  Tibetan, Myanmar or Khmer characters, one variation selector after a
  CJK ideograph and the flags of England, Scotland and Wales. There they
  can still carry a little information nobody sees. In the text a person
  approves, the room label and the names are isolated (Unicode isolates,
  a `bdi` element on the page, their own line in the terminal when they
  hold right-to-left text), so they cannot reorder the words around
  them. `a2p2 confirm` and the page send the text they showed, and the
  daemon decides nothing if it changed. A seconding request says that
  the other admin chose the name it carries, shows the name the room
  records, and says when other members have that name. On the page, a
  message that asks the person is drawn apart from requests, says who
  asked and of whom, and its choices are plain buttons; a choice its
  button cannot show exactly as it is sent is not sent. `--json` output
  is data: the stored strings, escaped only by JSON's rules, apart from
  sentences the daemon wrote for a person (a request's or an alert's
  text). Names are labels, and lookalike letters are not detected:
  compare fingerprints, not names.
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
  `github.com/wallter/a2p2/releases`. With `A2P2_VERIFY_ATTESTATION=1`
  they also run `gh attestation verify`, which asks GitHub's API for the
  attestation.

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
one from the release vX.Y.Z, run `gh attestation verify a2p2_<os>_<arch>
--repo wallter/a2p2 --signer-workflow
wallter/a2p2/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z
--deny-self-hosted-runners` (needs the GitHub CLI). Without `--source-ref`
an attested build of any other tag passes too. It proves which workflow
and commit built the file, not that the code is safe. Releases up to 0.2.3 have none. The binaries are not
code-signed or notarised.

The installer's checksum proves the download is intact, not that it is
authentic: `SHA256SUMS` comes from the same release page as the binary.
Someone who can replace release files (a token with `contents: write`,
or the maintainer's account) can replace a binary and its line together,
and `install.sh` or `install.ps1` then installs it. Set
`A2P2_VERIFY_ATTESTATION=1` and the installer also runs `gh attestation
verify` and installs nothing without a valid attestation from this
repository's `release.yml`, made from the tag of the release it
downloads (`--source-ref refs/tags/<tag>`) on a GitHub-hosted runner.
That attacker cannot make such an attestation without running the
release workflow, and cannot pass off an attested build of an older tag
as a newer release. Without `A2P2_VERSION` the installer asks `gh` once
for the tag of the latest release and downloads and checks that tag.
Someone who can edit releases can mark an older attested release as the
latest, so for `latest` the installer cannot rule out getting an older
release; set `A2P2_VERSION` to rule that out. Without the setting, the
installer says the checksum does not show who built the file; a release
up to 0.2.3 is refused with it.

The release workflow pins every action to a full commit SHA, so moving a
tag of an action does not change what runs. It has two jobs. The build
job checks out the tag and runs the Go toolchain with a read-only token,
and its checkout does not keep the token. The publish job holds the
write, signing and attestation scopes, checks out nothing and runs no
repository code: it checks the built files against `SHA256SUMS`, attests
them and creates the release, after checking that the files are exactly
the six binaries, `SHA256SUMS` and the notes, and that `SHA256SUMS` has
exactly one line for each binary. `make check` refuses a workflow that
names an action by tag or branch, keeps the checkout token, grants a
write scope at the top level, or gives a write scope (quoted or not) to
a job that checks out code or sets up Go. Its check reads lines, not
YAML, so it also refuses what it does not follow: a tab in the
indentation, a quoted key, a flow mapping, an anchor or alias, a `uses:`
that does not start its line, and a job not at two spaces under `jobs:`.
It reads every `.yml` and `.yaml` file in `.github/workflows`. Still open: a build
job that runs a compromised pinned action or toolchain makes a binary
that the publish job attests, since an attestation says which workflow
ran, not that its inputs were honest. Whether the repository has
immutable releases, tag protection or a protected release environment
is a repository setting, not shown in these files.

Both installers check the install directory and every directory above
it, and refuse these cases:

- `install.sh`: a directory every user may write without the sticky bit;
  one a group may write, unless that group is your own group of the same
  name as your user (on macOS, `staff` is not, so a `g+w` directory is
  refused); one owned by another user than you or root; and a symbolic
  link owned by another user. It checks each part of the path as
  written, and each symbolic link it meets adds the path that link names,
  checked the same way, so the real directories and those above them are
  checked too. It makes missing directories with
  mode 0755 whatever the umask, and checks the whole path again after it
  made them. It does not check access control lists (ACLs) beyond the
  mode bits, and it does not refuse a sticky directory every user may
  write, such as `/tmp`: a directory you made there is still yours.
- `install.ps1`: a junction or symbolic link anywhere in the path; a
  directory owned by an account other than you, SYSTEM, Administrators
  or TrustedInstaller; a directory whose access list it cannot read; and
  an install directory that any other account may write, delete, change
  the access list of or take ownership of, including through entries
  that pass only to new files. A directory above it is refused when
  another account may delete a child, delete it, change its access list
  or take ownership. It makes a missing directory with an access list of
  its own already in place (you, SYSTEM, Administrators; nothing
  inherited), checks that it is a real folder, still empty and yours, and
  checks the whole path again before it copies the file. `install.ps1`
  has not been run on Windows.

See the [design](docs/DESIGN.md), [protocol](docs/PROTOCOL.md)
and [limits](docs/REQUIREMENTS.md#6-limits-in-plain-words).
