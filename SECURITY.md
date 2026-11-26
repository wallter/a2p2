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
  `github.com/wallter/a2p2/releases`. With `A2P2_VERIFY_ATTESTATION=1`
  they also run `gh attestation verify`, which asks GitHub's API for the
  attestation.

Hooks and the MCP server talk only to the daemon over its Unix socket.

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
