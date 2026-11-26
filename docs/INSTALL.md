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
curl -fsSL https://raw.githubusercontent.com/wallter/a2p2/v0.2.3/install.sh -o install.sh
less install.sh
A2P2_VERSION=v0.2.3 sh install.sh
```

The checksum the script checks comes from the same release, so this
catches a corrupted or swapped download, not a compromised release. The
installer checks SHA-256 only and does not need the GitHub CLI. Releases
after 0.2.3 also carry an attestation from the release workflow. To check
a downloaded binary, run `gh attestation verify a2p2_<os>_<arch> --repo
wallter/a2p2` (needs the GitHub CLI). It proves which workflow and commit
built the file, not that the code is safe. Releases up to 0.2.3 have none.
The binaries are not code-signed or notarised.

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

- Put `a2p2.exe` in a directory whose path has no spaces, such as
  `C:\a2p2`: the hooks name it in a command a shell runs, and
  `a2p2 install` refuses a path that each shell would need quoted
  differently.
- State lives in `%USERPROFILE%\.a2p2` (or `A2P2_HOME`). a2p2 creates it
  readable by your user only and refuses one that other users can open.
- Decisions (`confirm`, `net`, approving a join) need a console window:
  Windows Terminal, PowerShell or `cmd`. Git Bash's mintty is not one.
- `a2p2 install agy` is not supported on Windows. Hermes's configuration
  is looked for in `%LOCALAPPDATA%\hermes` unless `HERMES_HOME` is set.
