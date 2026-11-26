#!/bin/sh
# Install the a2p2 binary from a GitHub release (macOS and Linux).
#
# What it does: downloads a2p2_<os>_<arch> and SHA256SUMS from one release,
# checks the binary's SHA-256 against SHA256SUMS, then puts the binary in
# the install directory and runs `a2p2 version`. Needs curl.
# The checksum shows the download is intact, not who built it: SHA256SUMS
# comes from the same release page. With A2P2_VERIFY_ATTESTATION=1 it also
# runs `gh attestation verify` and installs only a binary built by this
# repository's release workflow from the tag of that release, on a
# GitHub-hosted runner (needs the GitHub CLI, signed in). Then "latest" is
# asked of gh once, and that tag is what is downloaded and checked.
# It refuses an install directory that another user could change: one that
# every user may write without the sticky bit, one a group may write
# (other than this user's own group of the same name), one owned by
# another user than root, a symbolic link owned by another user, or any of
# these above it, along the path as written and the path each link names.
# It makes missing directories with mode 0755 and checks again after.
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
#                     https, or file:// or http://127.0.0.1 / localhost
#                     (no redirects are followed for those).
#   A2P2_VERIFY_ATTESTATION  1 to require `gh attestation verify` to pass
#                     (default 0). Releases up to v0.2.3 have no attestation.
set -eu

usage() {
  cat <<'USAGE'
usage: install.sh [--dry-run] [--help]

Installs a2p2 from a GitHub release after checking its SHA-256.
  --dry-run  print what would be downloaded and where it would go; write nothing
  --help     this text

Environment: A2P2_VERSION (default latest), A2P2_INSTALL_DIR (default
$HOME/.local/bin), A2P2_BASE_URL (tests and mirrors),
A2P2_VERIFY_ATTESTATION=1 (also require `gh attestation verify`).
USAGE
}

die() {
  echo "install.sh: $*" >&2
  exit 1
}

TMP=""
STAGE=""
SECURE=1
HASH=""
cleanup() {
  [ -z "$STAGE" ] || rm -f "$STAGE"
  [ -z "$TMP" ] || rm -rf "$TMP"
}

fetch() { # fetch URL OUTFILE
  command -v curl >/dev/null 2>&1 || die "needs curl"
  # -q (first argument) ignores any .curlrc; -g turns off URL globbing.
  if [ "$SECURE" = 1 ]; then
    curl -q -g -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$2" "$1"
  else
    # file:// or loopback http, for tests and mirrors: never follow a redirect.
    curl -q -g -fsS --max-redirs 0 --proto '=file,http' -o "$2" "$1"
  fi
}

sha256_of() { # sha256_of FILE: sets HASH to 64 lower-case hex characters
  if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum < "$1") || die "sha256sum failed; nothing was installed"
  elif command -v shasum >/dev/null 2>&1; then
    HASH=$(shasum -a 256 < "$1") || die "shasum failed; nothing was installed"
  else
    die "needs sha256sum or shasum to check the download; nothing was installed"
  fi
  HASH=${HASH%% *}
  is_hash "$HASH" || die "the hash tool gave no SHA-256; nothing was installed"
}

REPO=wallter/a2p2
SIGNER=$REPO/.github/workflows/release.yml

# check_one P: prints why another user could change P itself and returns
# 0; returns 1 when P is safe. A symbolic link is judged as the link (find
# without -H or -L does not follow one, as POSIX says): its owner counts,
# its mode does not. A directory is unsafe when every user may write it
# without the sticky bit, when a group may write it (unless that group is
# this user's own group of the same name), or when it is owned by another
# user than this one or root.
check_one() {
  o=$(find "$1" -prune ! -user "$uid" ! -user 0 2>/dev/null) || { echo "cannot check $1 (find failed)"; return 0; }
  [ -z "$o" ] || { echo "$1 is owned by another user"; return 0; }
  w=$(find "$1" -prune ! -type l -perm -0002 ! -perm -1000 2>/dev/null) || { echo "cannot check $1 (find failed)"; return 0; }
  [ -z "$w" ] || { echo "$1 can be written by every user"; return 0; }
  if [ "$upg" = 1 ]; then
    g=$(find "$1" -prune ! -type l -perm -0020 ! -perm -1000 ! -group "$gid" 2>/dev/null) || { echo "cannot check $1 (find failed)"; return 0; }
  else
    g=$(find "$1" -prune ! -type l -perm -0020 ! -perm -1000 2>/dev/null) || { echo "cannot check $1 (find failed)"; return 0; }
  fi
  [ -z "$g" ] || { echo "$1 can be written by a group"; return 0; }
  return 1
}

# unsafe_dir DIR: prints why another user could replace a file in DIR, or
# put a directory of their own in its place, and returns 0; returns 1 when
# DIR is safe. It checks each part of the path as written and every part
# above it (a symbolic link there is judged as the link). Each link it
# meets adds the path the link names, walked the same way, so the real
# directories and their parents are checked too (readlink; without it,
# the fully resolved directory, cd -P). A missing part is skipped:
# install.sh makes it later, as this user, and checks again.
unsafe_dir() {
  case "$1" in
    /*) p=$1 ;;
    *) p=$(pwd)/$1 ;;
  esac
  uid=$(id -u 2>/dev/null) || { echo "id -u failed, so the owner of $p cannot be checked"; return 0; }
  gid=$(id -g 2>/dev/null) || { echo "id -g failed, so the group of $p cannot be checked"; return 0; }
  un=$(id -un 2>/dev/null) || un=""
  gn=$(id -gn 2>/dev/null) || gn=""
  upg=0
  [ -z "$un" ] || [ "$un" != "$gn" ] || upg=1
  nl='
'
  todo=$p
  hops=0
  while [ -n "$todo" ]; do
    p=${todo%%"$nl"*}
    if [ "$p" = "$todo" ]; then todo=""; else todo=${todo#*"$nl"}; fi
    while :; do
      if [ -e "$p" ] || [ -L "$p" ]; then
        check_one "$p" && return 0
        if [ -L "$p" ]; then
          hops=$((hops + 1))
          [ "$hops" -le 40 ] || { echo "$1 has too many symbolic links to check"; return 0; }
          if command -v readlink >/dev/null 2>&1; then
            t=$(readlink "$p" 2>/dev/null) || { echo "cannot read the link $p"; return 0; }
            case "$t" in
              /*) ;;
              *) t=$(dirname "$p")/$t ;;
            esac
          else
            t=$(cd -P "$p" 2>/dev/null && pwd -P) || { echo "cannot resolve $p"; return 0; }
          fi
          todo=$t${todo:+$nl$todo}
        fi
      fi
      [ "$p" != / ] && [ "$p" != . ] || break
      p=$(dirname "$p")
    done
  done
  return 1
}

# make_dirs DIR: makes each missing level of DIR as this user with mode
# 0755 (whatever the umask). It fails if a level appears in between; the
# caller checks the whole path again afterwards.
make_dirs() {
  [ -d "$1" ] && return 0
  make_dirs "$(dirname "$1")" || return 1
  mkdir -m 0755 "$1" 2>/dev/null
}

is_hash() { # exactly 64 lower-case hex characters
  [ "${#1}" = 64 ] || return 1
  case "$1" in *[!0-9a-f]*) return 1 ;; esac
  return 0
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
    *[[:space:]]* | *@* | *\?* | *\#*) die "A2P2_BASE_URL must not have whitespace, '@', '?' or '#': $base" ;;
  esac
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

  attest=${A2P2_VERIFY_ATTESTATION:-0}
  case "$attest" in
    0 | 1) ;;
    *) die "A2P2_VERIFY_ATTESTATION must be 1 or 0, got '$attest'" ;;
  esac

  if [ "$dry" = 1 ]; then
    echo "dry run: nothing will be written"
    echo "would download $url/$asset"
    echo "would download $url/SHA256SUMS"
    echo "would check the SHA-256 of $asset against SHA256SUMS"
    if [ "$attest" = 1 ]; then
      [ "$version" != latest ] || echo "would ask gh for the tag of the latest release and download from that tag"
      echo "would run gh attestation verify on $asset (signer $SIGNER, source ref the release tag)"
    fi
    echo "would install it as $target"
    return 0
  fi

  # The cleanup trap comes before anything it cleans up.
  trap cleanup EXIT
  trap 'exit 1' INT TERM HUP

  if [ -d "$dir" ]; then
    [ -w "$dir" ] || die "cannot write to $dir (no sudo is used). Set A2P2_INSTALL_DIR to a directory you can write"
  fi
  if [ -d "$target" ]; then
    die "$target is a directory; remove it or set A2P2_INSTALL_DIR"
  fi
  if why=$(unsafe_dir "$dir"); then
    die "$why, so another user could replace a2p2 there; nothing was installed. Set A2P2_INSTALL_DIR to a directory only you can change"
  fi
  if [ "$attest" = 1 ]; then
    case "$version" in
      v0.1.* | v0.2.[0-3] | v0.2.[0-3]-*) die "release $version has no attestation (releases up to v0.2.3 have none); unset A2P2_VERIFY_ATTESTATION or pick a later release" ;;
    esac
    command -v gh >/dev/null 2>&1 || die "A2P2_VERIFY_ATTESTATION=1 needs the GitHub CLI (gh); nothing was installed"
    # The attestation must name the tag the files come from, so "latest" is
    # resolved once here and both files are fetched from that tag.
    if [ "$version" = latest ]; then
      version=$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>/dev/null) || die "could not ask gh for the latest release; set A2P2_VERSION"
      case "$version" in
        v[0-9]*) ;;
        *) die "gh named the latest release '$version', which is not a version tag; set A2P2_VERSION" ;;
      esac
      case "$version" in
        *[!A-Za-z0-9._-]*) die "gh named the latest release '$version', which is not a version tag; set A2P2_VERSION" ;;
        v0.1.* | v0.2.[0-3] | v0.2.[0-3]-*) die "the latest release $version has no attestation (releases up to v0.2.3 have none); unset A2P2_VERIFY_ATTESTATION" ;;
      esac
      url="$base/download/$version"
    fi
  fi

  TMP=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-install.XXXXXX") || die "cannot make a temporary directory"

  echo "downloading $asset ($version)"
  fetch "$url/$asset" "$TMP/$asset" || die "could not download $url/$asset"
  fetch "$url/SHA256SUMS" "$TMP/SHA256SUMS" || die "could not download $url/SHA256SUMS"

  want=$(awk -v n="$asset" '$2 == n || $2 == "*" n { print $1 }' "$TMP/SHA256SUMS")
  [ -n "$want" ] || die "SHA256SUMS has no line for $asset; nothing was installed"
  [ "$(printf '%s\n' "$want" | wc -l | tr -d ' ')" = 1 ] || die "SHA256SUMS has more than one line for $asset; nothing was installed"
  want=$(printf '%s' "$want" | tr 'A-F' 'a-f')
  is_hash "$want" || die "SHA256SUMS has no valid SHA-256 for $asset; nothing was installed"
  sha256_of "$TMP/$asset"
  if [ "$HASH" != "$want" ]; then
    die "checksum mismatch for $asset (expected $want, got $HASH); nothing was installed"
  fi
  echo "checksum ok: $HASH"
  if [ "$attest" = 1 ]; then
    if ! gh attestation verify "$TMP/$asset" --repo "$REPO" --signer-workflow "$SIGNER" \
      --source-ref "refs/tags/$version" --deny-self-hosted-runners >"$TMP/attest.out" 2>&1; then
      cat "$TMP/attest.out" >&2
      die "$asset has no valid attestation from $SIGNER for $version; nothing was installed"
    fi
    echo "attestation ok: built by $SIGNER from $version"
  else
    echo "note: the checksum shows the download is intact, not who built it."
    echo "  To check that too, run this again with A2P2_VERIFY_ATTESTATION=1 (needs gh)."
  fi

  # Only now is the directory created, if it is missing.
  # Each level is made with mode 0755, and the whole path is checked again
  # after: a level another user made or changed in the meantime is refused.
  if [ ! -d "$dir" ]; then
    make_dirs "$dir" || die "cannot create $dir (no sudo is used). Set A2P2_INSTALL_DIR to a directory you can write"
  fi
  if why=$(unsafe_dir "$dir"); then
    die "$why, so another user could replace a2p2 there; nothing was installed. Set A2P2_INSTALL_DIR to a directory only you can change"
  fi
  [ -w "$dir" ] || die "cannot write to $dir (no sudo is used). Set A2P2_INSTALL_DIR to a directory you can write"

  # Invariant: the bytes moved into place are bytes whose hash was checked
  # after they were written there. The staging file is unique and made
  # exclusively by mktemp, copied into, and hashed again before the rename.
  STAGE=$(mktemp "$dir/.a2p2.new.XXXXXX") || die "cannot write to $dir"
  cp "$TMP/$asset" "$STAGE" || die "cannot write to $dir"
  sha256_of "$STAGE"
  if [ "$HASH" != "$want" ]; then
    die "the copy in $dir does not match the checksum; nothing was installed"
  fi
  chmod 755 "$STAGE"
  if [ -L "$target" ]; then
    echo "note: $target was a symbolic link; the link itself is replaced"
  fi
  mv -f "$STAGE" "$target"
  STAGE=""

  echo "installed $target"
  "$target" version || die "installed $target but it did not run"

  case ":$PATH:" in
    *":$dir:"*) echo "$dir is on your PATH." ;;
    *)
      case "$dir" in
        *[!A-Za-z0-9/._-]*)
          echo "$dir is not on your PATH. Add this directory to your PATH:"
          echo "$dir"
          ;;
        *)
          echo "$dir is not on your PATH. Add this line to your shell profile:"
          echo "  export PATH=\"$dir:\$PATH\""
          ;;
      esac
      ;;
  esac
  echo "Next: connect a harness with  a2p2 install claude  (or codex, agy, opencode, hermes, pi),"
  echo "then run  a2p2 doctor"
}

# One brace group, so the shell parses the whole call before it runs anything.
{ main "$@"; exit $?; }
