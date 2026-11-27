# Installing a2p2

The [README](../README.md#install) has the one-line installer, the
cautious form (download, read, run), `go install` and building from
source. This page holds the other routes: a release binary checked by
hand, and Windows.

## The installer at a fixed version

The one-line form in the README fetches `install.sh` from the `main`
branch, which changes over time, and installs the latest release. To fix
both the script and the binary to one release, take the script from that
release's tag and name the version. Release tags cannot be moved or
deleted (a rule of the repository):

```sh
curl -fsSL https://raw.githubusercontent.com/wallter/a2p2/v0.3.0/install.sh -o install.sh
less install.sh
A2P2_VERSION=v0.3.0 sh install.sh
```

The checksum the script checks comes from the same release, so this
catches a corrupted download, not a compromised release: whoever can
replace the binary can replace its line in `SHA256SUMS` too. By default
the installer checks SHA-256 only, does not need the GitHub CLI, and says
so after the check. From version 0.3.0, releases also carry an attestation
from the release workflow. With `A2P2_VERIFY_ATTESTATION=1` the installer runs
`gh attestation verify` on the download and installs nothing unless it
passes (needs the GitHub CLI, signed in; it refuses a release up to
0.2.3). The attestation must be from the tag it downloads, so an attested
build of an older tag is refused. Without `A2P2_VERSION` it asks `gh` for
the tag of the latest release; someone who can edit releases can mark an
older release as the latest, so set `A2P2_VERSION` to rule that out. To
check a downloaded binary of vX.Y.Z by hand, run `gh attestation verify
a2p2_<os>_<arch> --repo wallter/a2p2 --signer-workflow
wallter/a2p2/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z`.
It proves which workflow and commit built the file, not that the code is
safe. Releases up to 0.2.3 have none.
The binaries are not code-signed or notarised.

## Which install directories `install.sh` refuses

Every hook and MCP server runs the installed `a2p2` as you, so the
installer refuses a directory where it can see that another user could
replace it. It checks the install directory and each one above it, and
the real path behind each symbolic link, with trailing slashes taken
off. It refuses:

- a directory every user may write, unless it has the sticky bit (as
  `/tmp` has);
- a directory owned by a user other than you or root, and a symbolic
  link owned by another user;
- a directory a group may write (its mode bits, without the sticky bit)
  when that group has a member other than you and root. The members are
  the group's listed members and the users whose primary group it is,
  as `getent` lists them, or `dscacheutil` and `dscl` on macOS;
- on macOS, a directory a group may write when the group holds accounts
  it does not list: `everyone`, `localaccounts` and `netaccounts`, and
  any group that `dsmemberutil` says the account `nobody` is in.

A group of only you and root is accepted with a note: on a Mac used by
one person, Homebrew's `/opt/homebrew/bin` may be written by the group
`admin`, whose members are you and root. A directory in your own group
of your name is accepted without a note. When no tool can list the
group, the installer prints a note and installs: check the group
yourself. The default, `~/.local/bin`, is made with mode 0755 when it is
missing.

It does not read access control lists (ACLs), nested groups, or accounts
of a network directory that the system does not list, so an ACL can let
another user write a directory that passes, and a group can have members
it does not see. On macOS, `ls -le DIR` shows a directory's ACL; on
Linux, `getfacl DIR`.

## From a release binary

Releases start at v0.1.0. Each
[release](https://github.com/wallter/a2p2/releases) has static binaries
named `a2p2_<os>_<arch>` for `darwin_arm64`, `darwin_amd64`, `linux_arm64`
and `linux_amd64`, and a `SHA256SUMS` file. Download your binary and
`SHA256SUMS` from the same release. On an Apple silicon Mac, this checks
the binary and moves it into place only if it prints
`a2p2_darwin_arm64: OK` (on Linux, use `sha256sum -c`):

```sh
awk '$2 == "a2p2_darwin_arm64"' SHA256SUMS | shasum -a 256 -c &&
  chmod +x a2p2_darwin_arm64 && mv a2p2_darwin_arm64 /usr/local/bin/a2p2
```

## On Windows

A PowerShell script, `install.ps1`, does what `install.sh` does (the
latest release, or `$env:A2P2_VERSION`, into `$env:A2P2_INSTALL_DIR`,
default `%LOCALAPPDATA%\a2p2\bin`). It is untried: it was written without
a Windows machine to run it on. Download it and read it before running it:
`irm https://raw.githubusercontent.com/wallter/a2p2/main/install.ps1 -OutFile install.ps1`.

a2p2 runs on Windows 10 (1803 or later) and 11. Its test suite passes on
a Windows Server 2025 runner on amd64 (2026-11-24). Not tried there:
`a2p2 install` with a real harness, the page in a Windows browser,
another Windows user trying to reach your daemon, and arm64 (built, not
run). From version 0.2.0 each release also has
`a2p2_windows_amd64.exe` and `a2p2_windows_arm64.exe`, listed in
`SHA256SUMS`; check one in PowerShell with
`(Get-FileHash a2p2_windows_amd64.exe).Hash` against its line there.
`go install` works as in the README and puts `a2p2.exe` in
`%USERPROFILE%\go\bin`.

- Put `a2p2.exe` in a directory whose path has no spaces and that only
  you can change: the hooks name it in a command a shell runs, and
  `a2p2 install` refuses a path that each shell would need quoted
  differently. Every hook and MCP server then runs that file as you. If
  your user name has a space, let `install.ps1` make the directory
  (`$env:A2P2_INSTALL_DIR = 'C:\a2p2'`, a folder that does not exist
  yet): it gives a directory it creates an access list for you, SYSTEM
  and Administrators only. A folder made by hand directly under `C:\`
  lets every signed-in user replace files in it, and `install.ps1`
  refuses one. To remove the access such a folder inherits, run in
  cmd.exe `icacls DIR /inheritance:r /grant:r "%USERNAME%:(OI)(CI)F"
  "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"` (in PowerShell, write
  `$env:USERNAME` for `%USERNAME%`). The command keeps access granted on
  the folder itself, so run `icacls DIR` afterwards and remove any other
  user it lists. `a2p2 install` does not check this on Windows yet.
- State lives in `%USERPROFILE%\.a2p2` (or `A2P2_HOME`). a2p2 creates it
  readable by your user only and refuses one that other users can open.
- Decisions (`confirm`, `net`, approving a join) need a console window:
  Windows Terminal, PowerShell or `cmd`. Git Bash's mintty is not one.
- `a2p2 install agy` is not supported on Windows. Hermes's configuration
  is looked for in `%LOCALAPPDATA%\hermes` unless `HERMES_HOME` is set.
