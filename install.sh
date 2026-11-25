#!/bin/sh
# Install the a2p2 binary from a GitHub release (macOS and Linux).
#
# What it does: downloads a2p2_<os>_<arch> and SHA256SUMS from one release,
# checks the binary's SHA-256 against SHA256SUMS, then puts the binary in
# the install directory and runs `a2p2 version`.
# What it never does: use sudo, edit a shell profile or PATH, configure a
# harness, start a daemon, or install a file whose checksum did not match.
#
# Read it before running it:
#   curl -fsSL https://raw.githubusercontent.com/wallter/a2p2/main/install.sh -o install.sh
#   less install.sh && sh install.sh
#
# Settings (environment):
#   A2P2_VERSION      a release such as v0.2.1 (default: the latest)
#   A2P2_INSTALL_DIR  where to put a2p2 (default: $HOME/.local/bin)
#   A2P2_BASE_URL     for tests and mirrors: the releases URL (default
#                     https://github.com/wallter/a2p2/releases). Must be
#                     https, or file:// or http://127.0.0.1 / localhost.
set -eu

usage() {
  cat <<'USAGE'
usage: install.sh [--dry-run] [--help]

Installs a2p2 from a GitHub release after checking its SHA-256.
  --dry-run  print what would be downloaded and where it would go; write nothing
  --help     this text

Environment: A2P2_VERSION (default latest), A2P2_INSTALL_DIR (default
$HOME/.local/bin), A2P2_BASE_URL (tests and mirrors).
USAGE
}

die() {
  echo "install.sh: $*" >&2
  exit 1
}

TMP=""
NEWFILE=""
SECURE=1
cleanup() {
  [ -z "$NEWFILE" ] || rm -f "$NEWFILE"
  [ -z "$TMP" ] || rm -rf "$TMP"
}

fetch() { # fetch URL OUTFILE
  if command -v curl >/dev/null 2>&1; then
    if [ "$SECURE" = 1 ]; then
      curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$2" "$1"
    else
      curl -fsSL -o "$2" "$1"
    fi
  elif command -v wget >/dev/null 2>&1; then
    if [ "$SECURE" = 1 ]; then
      wget -q --https-only --secure-protocol=TLSv1_2 -O "$2" "$1"
    else
      case "$1" in
        file://*) die "wget cannot read file:// URLs; install curl" ;;
      esac
      wget -q -O "$2" "$1"
    fi
  else
    die "needs curl or wget"
  fi
}

sha256_of() { # sha256_of FILE: prints the hash
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "needs sha256sum or shasum to check the download; nothing was installed"
  fi
}

main() {
  dry=0
  for arg in "$@"; do
    case "$arg" in
      --dry-run) dry=1 ;;
      -h | --help) usage; exit 0 ;;
      *) usage >&2; exit 2 ;;
    esac
  done

  manual="download a binary and SHA256SUMS from https://github.com/wallter/a2p2/releases and check it by hand (see the README)"
  os=$(uname -s)
  arch=$(uname -m)
  case "$os" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) die "unsupported system '$os'. Use the manual route: $manual" ;;
  esac
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture '$arch'. Use the manual route: $manual" ;;
  esac
  asset="a2p2_${os}_${arch}"

  base=${A2P2_BASE_URL:-https://github.com/wallter/a2p2/releases}
  base=${base%/}
  case "$base" in
    https://*) ;;
    file:///* | http://127.0.0.1 | http://127.0.0.1[:/]* | http://localhost | http://localhost[:/]*) SECURE=0 ;;
    *) die "A2P2_BASE_URL must be https (or file:// or http://localhost for tests): $base" ;;
  esac

  version=${A2P2_VERSION:-latest}
  if [ "$version" = latest ]; then
    url="$base/latest/download"
  else
    case "$version" in
      v[0-9]*) ;;
      *) die "A2P2_VERSION must look like v0.2.1, got '$version'" ;;
    esac
    case "$version" in
      *[!A-Za-z0-9._-]*) die "A2P2_VERSION has a character that does not belong in a version: '$version'" ;;
    esac
    url="$base/download/$version"
  fi

  if [ -n "${A2P2_INSTALL_DIR:-}" ]; then
    dir=$A2P2_INSTALL_DIR
  else
    [ -n "${HOME:-}" ] || die "set HOME or A2P2_INSTALL_DIR"
    dir=$HOME/.local/bin
  fi
  target="$dir/a2p2"

  if [ "$dry" = 1 ]; then
    echo "dry run: nothing will be written"
    echo "would download $url/$asset"
    echo "would download $url/SHA256SUMS"
    echo "would check the SHA-256 of $asset against SHA256SUMS"
    echo "would install it as $target"
    return 0
  fi

  if [ ! -d "$dir" ]; then
    mkdir -p "$dir" 2>/dev/null || die "cannot create $dir (no sudo is used). Set A2P2_INSTALL_DIR to a directory you can write"
  fi
  [ -w "$dir" ] || die "cannot write to $dir (no sudo is used). Set A2P2_INSTALL_DIR to a directory you can write"

  TMP=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-install.XXXXXX") || die "cannot make a temporary directory"
  trap cleanup EXIT
  trap 'exit 1' INT TERM HUP

  echo "downloading $asset ($version)"
  fetch "$url/$asset" "$TMP/$asset" || die "could not download $url/$asset"
  fetch "$url/SHA256SUMS" "$TMP/SHA256SUMS" || die "could not download $url/SHA256SUMS"

  want=$(awk -v n="$asset" '$2 == n || $2 == "*" n { print $1 }' "$TMP/SHA256SUMS")
  [ -n "$want" ] || die "SHA256SUMS has no line for $asset; nothing was installed"
  [ "$(printf '%s\n' "$want" | wc -l | tr -d ' ')" = 1 ] || die "SHA256SUMS has more than one line for $asset; nothing was installed"
  got=$(sha256_of "$TMP/$asset")
  want=$(printf '%s' "$want" | tr 'A-F' 'a-f')
  if [ "$got" != "$want" ]; then
    die "checksum mismatch for $asset (expected $want, got $got); nothing was installed"
  fi
  echo "checksum ok: $got"

  NEWFILE="$dir/.a2p2.new.$$"
  cp "$TMP/$asset" "$NEWFILE" || die "cannot write to $dir"
  chmod 755 "$NEWFILE"
  mv -f "$NEWFILE" "$target"
  NEWFILE=""

  echo "installed $target"
  "$target" version || die "installed $target but it did not run"

  case ":$PATH:" in
    *":$dir:"*) echo "$dir is on your PATH." ;;
    *)
      echo "$dir is not on your PATH. Add this line to your shell profile:"
      echo "  export PATH=\"$dir:\$PATH\""
      ;;
  esac
  echo "Next: connect a harness with  a2p2 install claude  (or codex, agy, opencode, hermes, pi),"
  echo "then run  a2p2 doctor"
}

main "$@"
