#!/bin/sh
# Test scripts/demo.sh: two runs at the same time, one plain and one with
# --file, each with its own free loopback port and its own throwaway homes.
# Checks the exit status, the lines that show each step worked, that the
# output holds neither this host's name nor this user's home path, and that
# afterwards the folders are gone and the daemons are not alive. Builds its
# own binary, so it can run beside make build and the other tests.
# Usage: scripts/demo-test.sh
set -u
cd "$(dirname "$0")/.."
GO=${GO:-go}
command -v python3 >/dev/null 2>&1 || { echo "demo-test: python3 not found, skipped"; exit 0; }

work=$(mktemp -d "${TMPDIR:-/tmp}/a2p2-demo-test.XXXXXX") || exit 2
trap 'rm -rf "$work"' EXIT
CGO_ENABLED=0 $GO build -trimpath -o "$work/a2p2" ./cmd/a2p2 || { echo "demo-test: build failed"; exit 1; }

# Two free loopback ports, found by the OS.
ports=$(python3 -I -c '
import socket
a, b = socket.socket(), socket.socket()
a.bind(("127.0.0.1", 0)); b.bind(("127.0.0.1", 0))
print(a.getsockname()[1], b.getsockname()[1])')
set -- $ports
A2P2_BIN=$work/a2p2 A2P2_DEMO_PORT_A=$1 scripts/demo.sh > "$work/plain.out" 2>&1 & p1=$!
A2P2_BIN=$work/a2p2 A2P2_DEMO_PORT_A=$2 scripts/demo.sh --file > "$work/file.out" 2>&1 & p2=$!
wait "$p1"; rc1=$?
wait "$p2"; rc2=$?

fail=0
bad() { echo "FAIL: $*"; fail=1; }
[ "$rc1" = 0 ] || bad "demo.sh exited $rc1"
[ "$rc2" = 0 ] || bad "demo.sh --file exited $rc2"

expect() { # expect FILE REGEX WHAT
  grep -Eq "$2" "$work/$1.out" || bad "$1: missing: $3"
}
for f in plain file; do
  expect $f '^  created "design-review"' "room created"
  expect $f '^  joined room .* as member' "joined"
  expect $f '^      Is the migration finished\?' "message read on beta"
  expect $f '^    by reviewer@[0-9a-f]+ at ' "handled report traced"
  expect $f 'Both daemons are stopped and both folders are removed; nothing is left running' "clean stop"
done
expect file 'The bytes fetched on beta are equal to the file sent from alpha' "file bytes equal"
grep -q 'bytes fetched' "$work/plain.out" && bad "plain run fetched a file"

# No real host name or home path.
host=$(hostname 2>/dev/null); short=${host%%.*}
for name in "$host" "$short"; do
  case $name in ''|localhost|?|??|???) continue ;; esac
  grep -Fiq -- "$name" "$work/plain.out" "$work/file.out" && bad "the host name \"$name\" appears in the output"
done
if [ -n "${HOME:-}" ] && [ "$HOME" != / ]; then
  grep -Fq -- "$HOME" "$work/plain.out" "$work/file.out" && bad "the home path appears in the output"
fi

# Nothing left: the folders named in the output, and the daemons whose pids it shows.
folders() { sed -n 's/^[a-z]*: *folder \(\/tmp\/a2p2demo\.[A-Za-z0-9]*\)$/\1/p' "$@"; }
for f in plain file; do
  for d in $(folders "$work/$f.out"); do
    [ -e "$d" ] && bad "$d still exists"
  done
  for pid in $(grep -Eo '  pid [0-9]+' "$work/$f.out" | grep -Eo '[0-9]+' | sort -u); do
    kill -0 "$pid" 2>/dev/null && bad "pid $pid is still alive ($f)"
  done
done
n=$(folders "$work/plain.out" "$work/file.out" | wc -l)
[ "$n" -eq 4 ] || bad "expected 4 folders in the output, found $n"

if [ "$fail" != 0 ]; then
  echo "--- plain run"; cat "$work/plain.out"; echo "--- file run"; cat "$work/file.out"
  exit 1
fi
echo "demo-test: ok"
