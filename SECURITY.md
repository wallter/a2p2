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

## Release binaries

Release binaries are built by GitHub Actions from the tag, with no module
or build cache, with CGO off and with `-trimpath`. Each release carries a
`SHA256SUMS` file made in the same run; verify a binary against it before
installing (see the [README](README.md#install)). The binaries are not
signed yet.

See the [design](docs/DESIGN.md), [protocol](docs/PROTOCOL.md)
and [limits](docs/REQUIREMENTS.md#6-limits-in-plain-words).
