#!/bin/sh
# The network benchmark: throughput and latency between a2p2 daemons in
# Docker containers, over links shaped with tc netem. See --help.
set -u

usage() {
  cat <<'EOF'
Usage: scripts/bench-net.sh [--quick] [--only REGEX] [--out DIR] [--plain-daemon] [--keep] [--help]

Starts real a2p2 daemons from a Linux build in separate Docker containers
on a private Docker network (no published ports, no host networking),
joins them into one room, shapes the links with tc netem inside the
containers and prints one table per section:

  baseline   two machines, no shaping, 200 B: 1,000 one after another,
             a burst of 5,000, 1, 10, 50 and 200 a second for 60 s; twice
  link       the burst and 10 a second at 10, 50, 150 and 400 ms one way,
             50 ms with jitter, 1% and 5% loss at 50 ms, 1 and 10 Mbit/s
  direction  sender listening and receiver outbound only; and the reverse
  size       16 KB messages: a burst, and 10, 50 and 200 a second
  members    3 and 5 machines all listening; 5 in a star through one
             listener: one machine's burst of 2,000, then every machine
             sending 10 a second
  files      10 MB and 100 MB, unshaped and at 50 ms with 10 Mbit/s

  --quick         the smallest cases, about five minutes
  --only REGEX    only the sections matching REGEX, e.g. 'baseline|files'
  --out DIR       results directory (default .bench-net/<time>/)
  --plain-daemon  run `a2p2 daemon` instead of a2p2-netd (no request counts)
  --keep          leave this run's containers, volumes, network and image

The full run takes about 35 minutes. Every Docker object carries the label
a2p2bnet.run=<run id>; they are removed at the end, on failure, and on
INT or TERM, and nothing else is touched. Needs Docker, Go and python3.
EOF
}

for a in "$@"; do
  case $a in -h|--help) usage; exit 0 ;; esac
done
here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here" || exit 2
command -v docker >/dev/null || { echo "bench-net: docker is not installed" >&2; exit 2; }
docker info >/dev/null 2>&1 || { echo "bench-net: docker is not running" >&2; exit 2; }

A2P2_BENCH_NET_RUN=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
export A2P2_BENCH_NET_RUN
keep=""
for a in "$@"; do [ "$a" = --keep ] && keep=1; done

# The driver removes what it made; this catches what is left if it dies.
cleanup() {
  [ -n "$keep" ] && return
  label="label=a2p2bnet.run=$A2P2_BENCH_NET_RUN"
  ids=$(docker ps -aq --filter "$label"); [ -n "$ids" ] && docker rm -f $ids >/dev/null
  ids=$(docker volume ls -q --filter "$label"); [ -n "$ids" ] && docker volume rm $ids >/dev/null
  ids=$(docker network ls -q --filter "$label"); [ -n "$ids" ] && docker network rm $ids >/dev/null
  ids=$(docker images -q --filter "$label"); [ -n "$ids" ] && docker image rm -f $ids >/dev/null
  return 0
}
trap cleanup EXIT
trap 'exit 130' INT TERM

python3 scripts/bench-net/bench_net.py "$@"
