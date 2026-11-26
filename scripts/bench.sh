#!/bin/sh
# The overhead benchmark of internal/bench. See --help.
set -u

usage() {
  cat <<'EOF'
Usage: scripts/bench.sh [--quick] [--runs N] [--only REGEX] [--help]

Measures a2p2 on this computer and prints one table per run:

  M1 local send over the Unix socket (serial; 4, 16 and 64 senders; 200 B,
     4 KB and the 16 KB maximum)
  M2 local send to visible (a waiting inbox of another label)
  M3 ping-pong between two labels
  M4 the same through the real `a2p2 mcp` process (JSON-RPC over pipes)
  M5 two daemons joined over 127.0.0.1 (pinned TLS); B connects out only
  M7 scale: 1, 10 and 100 rooms; 100, 10,000 and 100,000 messages held
  micro: Ed25519, record encode/parse, raw SQLite commit and fsync, the
     product's Surface.Send with and without the socket
  and a CPU profile of a serial 200 B send through the socket, in process.

  --quick     a tenth of the counts (about two minutes), for a smoke run
  --runs N    run the load and micro sets N times (default 2)
  --only RE   only the load tests TestM<RE>, e.g. 7 or 'M1|M2' (both forms work);
              no micro, no profile
  --help      this text

It builds bin/a2p2, and every daemon it starts has its own home made like
`mktemp -d /tmp/a2p2x.XXXXXX` (mode 700) passed as A2P2_HOME, with HOME
inside it too; a daemon is stopped with `a2p2 stop` and its home removed.
Networking is loopback only. It never touches ~/.a2p2 or harness settings.
Results (TSV, the micro output and the profile) are kept in BENCH_OUT, by
default a new /tmp/a2p2bench.XXXXXX directory, whose path is printed.
EOF
}

runs=2 quick="" only=""
while [ $# -gt 0 ]; do
  case $1 in
    --quick) quick=1 ;;
    --runs) runs=$2; shift ;;
    --only) only=$2; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "bench: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here" || exit 2
GO=${GO:-go}
out=${BENCH_OUT:-$(mktemp -d /tmp/a2p2bench.XXXXXX)} || exit 2
mkdir -p "$out" || exit 2
unset A2P2_HOME A2P2_NAME

make --no-print-directory build >/dev/null || exit 1
BIN=$here/bin/a2p2

echo "== machine"
case $(uname -s) in
  Darwin)
    echo "cpu      $(sysctl -n machdep.cpu.brand_string), $(sysctl -n hw.ncpu) cores, $(($(sysctl -n hw.memsize) / 1073741824)) GB"
    echo "os       macOS $(sw_vers -productVersion)"
    diskutil info / 2>/dev/null | grep -E '^ *(Solid State|Protocol|File System Personality):' |
      sed -E 's/^ *([^:]*): *(.*)$/disk     \1: \2/'
    ;;
  Linux)
    echo "cpu      $(sed -n 's/^model name[^:]*: //p' /proc/cpuinfo | head -n 1), $(nproc) cores"
    echo "os       $(uname -sr)"
    ;;
esac
echo "go       $($GO version | cut -d' ' -f3-)"
echo "binary   $($BIN version)"
echo "results  $out"

status=0
i=1
while [ "$i" -le "$runs" ]; do
  echo
  echo "== run $i: load (real daemons from bin/a2p2)"
  A2P2_BENCH_BIN=$BIN A2P2_BENCH_OUT=$out/load-$i.tsv A2P2_BENCH_QUICK=$quick \
    $GO test -tags bench -count=1 -timeout 90m -run "^TestM?(${only:-M})" -v ./internal/bench > "$out/load-$i.log" 2>&1 || status=1
  grep -E '^(RESULT|NOTE|--- FAIL|FAIL)' "$out/load-$i.log" | sed 's/^RESULT  //; s/^NOTE    /  note: /'
  if grep -q 'no tests to run' "$out/load-$i.log"; then
    echo "bench: --only '$only' matches no load test" >&2
    status=1
  fi
  if [ -z "$only" ]; then
    echo
    echo "== run $i: micro (in process)"
    bt=2s; [ -n "$quick" ] && bt=300ms
    $GO test -tags bench -count=1 -run '^$' -bench . -benchmem -benchtime "$bt" ./internal/bench > "$out/micro-$i.txt" 2>&1 || status=1
    grep -E '^Benchmark|FAIL' "$out/micro-$i.txt"
  fi
  i=$((i + 1))
done

if [ -z "$only" ]; then
  echo
  echo "== CPU profile: serial 200 B send through the socket, client and handler in one process"
  bt=10s; [ -n "$quick" ] && bt=2s
  $GO test -tags bench -count=1 -run '^$' -bench '^BenchmarkSocketSend$/^200$' -benchtime "$bt" \
    -cpuprofile "$out/send.cpu" -o "$out/bench.test" ./internal/bench > "$out/profile.txt" 2>&1 || status=1
  grep -E '^Benchmark' "$out/profile.txt"
  # CPU samples by part (scripts/bench-profile.awk). On macOS the Go CPU
  # profiler charges much of its time to thread wake-ups; read these with
  # the wall-time parts of the micro run (SurfaceSend, SocketSend,
  # RecordNewAndVerified, SQLiteCommit FULL minus OFF).
  [ "$(uname -s)" = Darwin ] && echo "  (macOS: CPU profile attribution is skewed; see the note in the script)"
  $GO tool pprof -traces "$out/bench.test" "$out/send.cpu" 2>/dev/null | awk -f scripts/bench-profile.awk
  $GO tool pprof -top -cum -nodecount=35 "$out/bench.test" "$out/send.cpu" > "$out/profile-top.txt" 2>/dev/null
  echo "  (top by cumulative time: $out/profile-top.txt)"
fi
ls -d /tmp/a2p2x.* 2>/dev/null && { echo "bench: a daemon home above was left behind" >&2; status=1; }
exit $status
