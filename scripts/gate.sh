#!/usr/bin/env bash
# Run the steps of a gate at the same time, each to its own log.
#
#   scripts/gate.sh [-l DIR] NAME=COMMAND ...
#
# Prints one line per step as it ends, with its time; then the full log of
# every step that failed, and a last line naming every failed step. Exits 0
# only if every step exited 0. With -l the logs are kept in DIR; otherwise
# they go to a temporary directory that is removed at the end.
set -u
logdir=""
if [ "${1:-}" = -l ]; then logdir=$2; shift 2; fi
[ "$#" -gt 0 ] || { echo "usage: gate.sh [-l DIR] NAME=COMMAND ..." >&2; exit 2; }
own=0
if [ -z "$logdir" ]; then logdir=$(mktemp -d "${TMPDIR:-/tmp}/a2p2gate.XXXXXX") || exit 2; own=1; fi
mkdir -p "$logdir" || exit 2

names=(); pids=(); done_=()
t0=$(date +%s)
for arg in "$@"; do
  name=${arg%%=*}; cmd=${arg#*=}
  ( sh -c "$cmd" > "$logdir/$name.log" 2>&1 ) &
  names+=("$name"); pids+=($!); done_+=(0)
done
stop() {
  # Ctrl-C at a terminal reaches every step (one process group); a TERM
  # sent to this script alone is passed on to the steps.
  for p in "${pids[@]}"; do kill -TERM "$p" 2>/dev/null; done
  echo "gate: interrupted"; exit 130
}
trap stop INT TERM

failed=""; left=${#pids[@]}
while [ "$left" -gt 0 ]; do
  for i in "${!pids[@]}"; do
    [ "${done_[$i]}" = 0 ] || continue
    kill -0 "${pids[$i]}" 2>/dev/null && continue
    wait "${pids[$i]}"; rc=$?
    done_[$i]=1; left=$((left - 1))
    secs=$(( $(date +%s) - t0 ))
    if [ "$rc" = 0 ]; then
      printf 'ok    %-12s %4ss\n' "${names[$i]}" "$secs"
    else
      printf 'FAIL  %-12s %4ss  (exit %s)\n' "${names[$i]}" "$secs" "$rc"
      failed="$failed ${names[$i]}"
    fi
  done
  [ "$left" -gt 0 ] && sleep 1
done

for n in $failed; do
  echo; echo "=== $n (full log: $logdir/$n.log)"; cat "$logdir/$n.log"
done
total=$(( $(date +%s) - t0 ))
if [ -n "$failed" ]; then
  echo; echo "gate: FAILED after ${total}s:$failed"
  exit 1
fi
[ "$own" = 1 ] && rm -rf "$logdir"
echo "gate: all $# steps passed in ${total}s"
