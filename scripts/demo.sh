#!/bin/sh
# A two-machine walkthrough on one computer. See --help.
set -u

usage() {
  cat <<'EOF'
Usage: scripts/demo.sh [--pause] [--keep] [--file] [--help]

Two a2p2 machines, "alpha" and "beta", on this computer. Alpha creates a
room and invites beta. Beta joins, the fingerprints are compared and alpha
confirms. An agent on alpha asks for a handled report; an agent on beta
reads the message, replies and acks; alpha traces it.

  --file    also send a small generated file from alpha and fetch it on beta
  --pause   wait for Enter between steps (for a recording)
  --keep    leave both folders and the daemons; print how to stop and remove
  --help    this text

It uses bin/a2p2 (or A2P2_BIN; run `make build` first). It makes two
throwaway folders under /tmp, passes A2P2_HOME to every command, listens
only on 127.0.0.1 (alpha on a free port the system picks, or the port in
A2P2_DEMO_PORT_A, which must be free; beta listens on nothing), so two
runs at once do not clash. It never touches ~/.a2p2 or any harness
settings. On success, failure or Ctrl-C it stops both daemons with
`a2p2 stop` and removes the folders. Needs python3 (a2p2 asks for decisions only at a terminal; a small
helper, scripts/demo_pty.py, types "yes" on a pseudo-terminal).
EOF
}

pause=0 keep=0 withfile=0
for a in "$@"; do
  case $a in
    --pause) pause=1 ;;
    --keep) keep=1 ;;
    --file) withfile=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "demo: unknown argument: $a" >&2; usage >&2; exit 2 ;;
  esac
done

here=$(cd "$(dirname "$0")" && pwd)
BIN=${A2P2_BIN:-$here/../bin/a2p2}
PTY=$here/demo_pty.py
PORT=${A2P2_DEMO_PORT_A:-}

[ -x "$BIN" ] || { echo "demo: $BIN not found; run: make build" >&2; exit 2; }
BIN=$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")
command -v python3 >/dev/null 2>&1 || { echo "demo: python3 is needed" >&2; exit 2; }
[ -f "$PTY" ] || { echo "demo: $PTY not found" >&2; exit 2; }
# The port: the one asked for if it is free, else any free loopback port.
case $PORT in *[!0-9]*) echo "demo: A2P2_DEMO_PORT_A must be a number" >&2; exit 2 ;; esac
PORT=$(python3 -I -c '
import socket, sys
s = socket.socket()
want = int(sys.argv[1]) if sys.argv[1] else 0
try:
    s.bind(("127.0.0.1", want))
except OSError as e:
    sys.exit("demo: 127.0.0.1:%s is in use (%s); change A2P2_DEMO_PORT_A" % (want, e))
print(s.getsockname()[1])
' "$PORT") || exit 2

# Settings of this shell that could change what the commands do.
unset A2P2_NAME A2P2_HOME

HA=$(mktemp -d /tmp/a2p2demo.XXXXXX) || exit 2
HB=$(mktemp -d /tmp/a2p2demo.XXXXXX) || { rmdir "$HA"; exit 2; }
chmod 700 "$HA" "$HB"
status=0
JPID=""

cleanup() {
  trap '' INT TERM
  rc=$status
  if [ "$keep" = 1 ] && [ "$rc" = 0 ]; then
    echo
    echo "Kept. Both machines are still running. To stop and remove them:"
    echo "  A2P2_HOME=$HA $BIN stop"
    echo "  A2P2_HOME=$HB $BIN stop"
    echo "  rm -rf $HA $HB"
    exit "$rc"
  fi
  if [ "$rc" != 0 ]; then
    for h in "$HA" "$HB"; do
      if [ -f "$h/daemon.log" ]; then
        echo "--- last lines of $h/daemon.log" >&2
        tail -n 5 "$h/daemon.log" >&2
      fi
    done
  fi
  [ -n "$JPID" ] && kill "$JPID" 2>/dev/null
  for h in "$HA" "$HB"; do
    A2P2_HOME=$h "$BIN" stop >/dev/null 2>&1
    n=20
    while [ "$n" -gt 0 ]; do
      [ "$(A2P2_HOME=$h "$BIN" stop 2>&1)" = "not running" ] && break
      n=$((n - 1)); sleep 0.25
    done
    [ "$n" -gt 0 ] || { echo "demo: a daemon under $h did not stop" >&2; rc=1; }
  done
  rm -rf "$HA" "$HB"
  if [ -e "$HA" ] || [ -e "$HB" ]; then
    echo "demo: could not remove $HA or $HB" >&2; rc=1
  elif [ "$rc" = 0 ]; then
    echo; echo "Done. Both daemons are stopped and both folders are removed; nothing is left running."
  else
    echo; echo "Failed. Both daemons are stopped and both folders are removed; nothing is left running."
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'status=130; exit 130' INT
trap 'status=143; exit 143' TERM

die() { echo "demo: FAILED: $*" >&2; status=1; exit 1; }

# a2 SIDE ARGS...: run a2p2 as one machine. pty SIDE ARGS...: the same on a
# pseudo-terminal, answering "yes" where a person decides.
home_of() { case $1 in alpha) echo "$HA" ;; beta) echo "$HB" ;; esac; }
a2() { s=$1; shift; A2P2_HOME=$(home_of "$s") "$BIN" "$@"; }
pty() { s=$1; shift; A2P2_HOME=$(home_of "$s") python3 -I "$PTY" yes "$BIN" "$@"; }

step() {
  printf '\n== %s\n' "$*"
  if [ "$pause" = 1 ] && [ -r /dev/tty ]; then
    printf '(Enter to continue) '; read -r _ </dev/tty
  fi
}
show() { s=$1; shift; printf '\n%s$ a2p2 %s\n' "$s" "$*"; }
# emit TEXT [MAXLINES]: print text, cut to MAXLINES with a note.
emit() {
  if [ -n "${2:-}" ] && [ "$(printf '%s\n' "$1" | wc -l)" -gt "$2" ]; then
    printf '%s\n' "$1" | head -n "$2" | sed 's/^/  /'
    echo "  (trimmed: the rest of the output is not shown)"
  else
    printf '%s\n' "$1" | sed 's/^/  /'
  fi
}
# do SIDE MAXLINES ARGS...: show, run, print, stop the demo if it fails. Sets OUT.
do_() {
  s=$1; m=$2; shift 2
  show "$s" "$*"
  OUT=$(a2 "$s" "$@" 2>&1) || { emit "$OUT"; die "a2p2 $* on $s"; }
  emit "$OUT" "$m"
}
do_pty() {
  s=$1; m=$2; shift 2
  show "$s" "$*"
  OUT=$(pty "$s" "$@" 2>&1) || { emit "$OUT"; die "a2p2 $* on $s"; }
  emit "$OUT" "$m"
}
# poll SIDE PATTERN TRIES ARGS...: run until the output matches. Sets OUT.
poll() {
  s=$1; pat=$2; n=$3; shift 3
  while [ "$n" -gt 0 ]; do
    OUT=$(a2 "$s" "$@" 2>&1) && printf '%s\n' "$OUT" | grep -Eq "$pat" && return 0
    n=$((n - 1)); sleep 0.5
  done
  return 1
}
group() { printf '%s' "$1" | sed 's/..../& /g'; }

# ---------------------------------------------------------------------------
step "0. Two throwaway machines"
# The daemon names a machine after the host unless config.json says so.
printf '{"name":"%s","ui":"127.0.0.1:0"}\n' alpha > "$HA/config.json"
printf '{"name":"%s","ui":"127.0.0.1:0"}\n' beta > "$HB/config.json"
echo "alpha: folder $HA"
echo "beta:  folder $HB"
echo "Each one has a config.json with only {\"name\":\"alpha\" or \"beta\",\"ui\":\"127.0.0.1:0\"}:"
echo "the name is fixed so the real host name never appears, and the web page"
echo "takes any free loopback port. Every command below runs with A2P2_HOME set"
echo "to its machine's folder."

step "1. Start both and look at them"
do_ alpha 4 status
do_ beta 4 status

step "2. Turn networking on (a person types yes at a terminal; the demo types it)"
do_pty alpha 6 net on "127.0.0.1:$PORT"
do_pty beta 6 net on --outbound-only
echo
echo "alpha listens on 127.0.0.1:$PORT. beta only connects out."

step "3. Alpha creates a room and an invite"
do_ alpha 4 room create design-review
do_ alpha 6 room invite design-review --member
INVITE=$(printf '%s\n' "$OUT" | sed -n 's/^\(a2p2:[^ ]*\)$/\1/p' | head -n 1)
[ -n "$INVITE" ] || die "no invite in the output"

step "4. Beta previews the invite and joins"
# join waits for the admin, so it runs in the background and its output goes
# to a file; the file shows when it has finished.
show beta "join <invite>"
A2P2_HOME=$HB python3 -I "$PTY" yes "$BIN" join "$INVITE" > "$HB/join.out" 2>&1 &
JPID=$!
n=40
while [ "$n" -gt 0 ] && ! grep -q 'Waiting for approval' "$HB/join.out" 2>/dev/null; do
  n=$((n - 1)); sleep 0.25
done
[ "$n" -gt 0 ] || { emit "$(cat "$HB/join.out")"; die "join did not reach the approval wait"; }
emit "$(cat "$HB/join.out")"

step "5. Compare the fingerprint, then alpha confirms"
show beta "status --json    (only the node field is shown)"
NODE=$(a2 beta status --json 2>/dev/null | sed -n 's/^{"node":"\([0-9a-f]*\)".*/\1/p')
[ -n "$NODE" ] || die "no node value from beta"
echo "  beta's node: $(group "$NODE")"
do_ alpha 8 confirm
QID=$(printf '%s\n' "$OUT" | sed -n 's/^Request \(q_[0-9a-f]*\).*/\1/p' | head -n 1)
SEEN=$(printf '%s\n' "$OUT" | sed -n 's/.*add machine \([0-9a-f ]*[0-9a-f]\) (.*/\1/p' | head -n 1 | tr -d ' ')
[ -n "$QID" ] && [ -n "$SEEN" ] || die "no request from beta on alpha"
if [ "$SEEN" = "$NODE" ]; then
  echo "  The fingerprint alpha shows is the same as beta's node, ignoring spaces."
else
  die "the fingerprints differ: alpha shows $SEEN, beta has $NODE"
fi
do_pty alpha 12 confirm "$QID"
n=60
while [ "$n" -gt 0 ] && ! grep -q '^joined room' "$HB/join.out" 2>/dev/null; do
  n=$((n - 1)); sleep 0.5
done
grep -q '^joined room' "$HB/join.out" || die "beta did not finish joining"
wait "$JPID"; jrc=$?; JPID=""
[ "$jrc" = 0 ] || die "join exited with status $jrc"
echo
echo "beta, from the join command:"
emit "$(grep '^joined room' "$HB/join.out")"
show beta "room list design-review"
OUT=$(a2 beta room list design-review 2>&1) || { emit "$OUT"; die "a2p2 room list design-review on beta"; }
# The single-admin warning is long; show its first sentence.
OUT=$(printf '%s\n' "$OUT" | awk '
  /^ *limit: / { i = index($0, ". "); if (i > 0) $0 = substr($0, 1, i)
                 $0 = $0 " (trimmed; see the README limits)" }
  { print }')
emit "$OUT" 8

step "6. Each agent chooses to follow every message in the room"
do_ alpha 3 inbox --level all --room design-review --as planner
do_ beta 3 inbox --level all --room design-review --as reviewer

step "7. The planner on alpha asks for a handled report"
do_ alpha 3 send --as planner --room design-review --ask-handled "Is the migration finished?"
MID=$(printf '%s\n' "$OUT" | grep -Eo '^[0-9a-f]{32}$' | head -n 1)
[ -n "$MID" ] || die "no message id"

step "8. The reviewer on beta reads, replies and acks"
show beta "inbox --as reviewer --room design-review --wait 5    (repeated until the message arrives)"
poll beta 'migration' 20 inbox --as reviewer --room design-review --wait 5 || { emit "$OUT"; die "beta never received the message"; }
emit "$OUT"
do_ beta 3 send --as reviewer --room design-review --reply-to "$MID" "Yes, it is finished."
do_ beta 3 ack --as reviewer "$MID"

step "9. Alpha reads the reply and traces the message"
show alpha "inbox --as planner --room design-review --wait 5    (repeated until the reply arrives)"
poll alpha 'finished' 20 inbox --as planner --room design-review --wait 5 || { emit "$OUT"; die "alpha never received the reply"; }
emit "$OUT"
show alpha "trace $MID    (repeated until the handled report arrives)"
poll alpha 'handled report' 40 trace "$MID" && ! printf '%s\n' "$OUT" | grep -q 'handled: no report yet' || { emit "$OUT"; die "no handled report arrived"; }
emit "$OUT"

if [ "$withfile" = 1 ]; then
  step "10. A file from alpha to beta (a person confirms the bytes on alpha)"
  mkdir "$HA/outgoing" || die "mkdir"
  printf 'demo file\nline two\n' > "$HA/outgoing/note.txt"
  SENT=$(cat "$HA/outgoing/note.txt")
  do_ alpha 3 send --as planner --room design-review --file "$HA/outgoing/note.txt" "A small file"
  FQID=$(printf '%s\n' "$OUT" | sed -n 's/^request \(q_[0-9a-f]*\) waiting.*/\1/p' | head -n 1)
  [ -n "$FQID" ] || die "no request for the file"
  do_pty alpha 12 confirm "$FQID"
  FMID=$(printf '%s\n' "$OUT" | sed -n 's/^.*approved (\([0-9a-f]\{32\}\)).*/\1/p' | head -n 1)
  [ -n "$FMID" ] || die "no message id for the file"
  # The message can arrive before its file list does, and a plain inbox would
  # mark it seen without the list; --peek leaves it unread until it is whole.
  show beta "inbox --peek --as reviewer --room design-review    (repeated until the file list arrives)"
  poll beta 'sha256' 40 inbox --peek --as reviewer --room design-review || { emit "$OUT"; die "beta never received the file message"; }
  emit "$OUT"
  SHA=$(printf '%s\n' "$OUT" | sed -n 's/^ *sha256 \([0-9a-f]\{64\}\)$/\1/p' | head -n 1)
  [ -n "$SHA" ] || die "no sha256 in the inbox"
  do_ beta 3 fetch "$SHA" --as reviewer --room design-review --message "$FMID" --wait 20
  GOT=$(printf '%s\n' "$OUT" | sed -n 's/^\(\/.*\)$/\1/p' | head -n 1)
  [ -f "$GOT" ] || die "fetch printed no file path"
  if [ "$(cat "$GOT")" = "$SENT" ] && cmp -s "$GOT" "$HA/outgoing/note.txt"; then
    echo "  The bytes fetched on beta are equal to the file sent from alpha."
  else
    die "the fetched file differs from the one sent"
  fi
fi

step "Result"
echo "All steps passed."
