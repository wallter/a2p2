#!/usr/bin/env bash
# The race tests of the whole module, with the slow packages split into
# shards (docs/TESTING.md, "How make test runs").
#
#   scripts/gotest.sh                 run every test once; exit 1 on any failure
#   TIMINGS_OUT=FILE scripts/gotest.sh   also write the measured times to FILE
#
# A package whose tests took SHARD_MIN seconds or more in total according to
# scripts/test-timings.tsv is compiled once (go test -race -c) and its
# top-level tests (from -test.list) are split into SHARDS groups balanced by
# their recorded times; tests with no recorded time are dealt round-robin.
# Each group runs as its own process of that binary, in the package
# directory, with -test.run set to an anchored alternation of its names and
# its own TMPDIR. The runner fails if the groups are not exactly the listed
# tests, or if a group that passed did not report each of its tests once.
# Every other package runs in one `go test`, as before. The same flags as
# `go test -race -count=1 -timeout 30m` apply everywhere.
#
# Environment: GO (go), SHARDS (default: CPUs, at most 12), SHARD_MIN (20),
# TIMINGS (scripts/test-timings.tsv), TIMINGS_OUT (unset), PKGS (./...).
set -uo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}
ncpu=$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 2)
SHARDS=${SHARDS:-$(( ncpu < 12 ? ncpu : 12 ))}
[ "$SHARDS" -ge 1 ] || SHARDS=1
SHARD_MIN=${SHARD_MIN:-20}
TIMINGS=${TIMINGS:-scripts/test-timings.tsv}
PKGS=${PKGS:-./...}
TIMEOUT=30m

work=$(mktemp -d "${TMPDIR:-/tmp}/a2p2test.XXXXXX") || exit 2
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
trap 'exit 130' INT TERM

mod=$($GO list -m)
all=$($GO list $PKGS) || exit 2
[ -f "$TIMINGS" ] || : > "$work/empty.tsv"
[ -f "$TIMINGS" ] || TIMINGS=$work/empty.tsv

# Packages to shard: listed in the timings file with a total at or above
# SHARD_MIN seconds, and part of this run.
# Slowest first, so that its shards start first.
slow=$(awk -F'\t' -v min="$SHARD_MIN" '!/^#/ && NF >= 3 { t[$1] += $3 } END { for (p in t) if (t[p] >= min) printf "%d %s\n", t[p], p }' "$TIMINGS" | sort -rn | cut -d' ' -f2)
sharded=""; rest=""
for p in $slow; do
  if grep -qxF -- "$p" <<< "$all"; then sharded="$sharded $p"; fi
done
for p in $all; do
  grep -qxF -- "$p" <<< "$slow" || rest="$rest $p"
done

# Parse `go test -v` output on stdin into "package<TAB>test<TAB>seconds" for
# top-level tests. $1 is the package when the output is one test binary's;
# otherwise the "ok"/"FAIL" line that ends each package's block names it.
times() {
  awk -v pkg="${1:-}" '
    /^--- (PASS|FAIL|SKIP): [^ \/]+ \([0-9.]+s\)$/ {
      name = $3; d = $4; gsub(/[()s]/, "", d); n++; tn[n] = name; td[n] = d; next }
    /^(ok|FAIL)[ \t]+[^ \t]+[ \t]/ && pkg == "" {
      for (i = 1; i <= n; i++) printf "%s\t%s\t%s\n", $2, tn[i], td[i]; n = 0; next }
    END { if (pkg != "") for (i = 1; i <= n; i++) printf "%s\t%s\t%s\n", pkg, tn[i], td[i] }'
}

pids=(); names=(); logs=()
start_job() { # name log command...
  local name=$1 log=$2; shift 2
  ( "$@" > "$log" 2>&1; rc=$?; date +%s > "$log.end"; exit $rc ) &
  pids+=($!); names+=("$name"); logs+=("$log")
}

fail=0
t0=$(date +%s)
note() { echo "gotest: $*"; }

# Every other package in one go test, as before, started first so that it
# runs while the slow packages compile.
if [ -n "${rest// /}" ]; then
  start_job "other packages" "$work/rest.log" $GO test -race -count=1 -timeout "$TIMEOUT" -v $rest
fi

# Compile the sharded packages at the same time, then list each one's
# tests and split them.
cpids=()
for p in $sharded; do
  rel=${p#"$mod"}; rel=.${rel}; [ "$rel" = . ] && rel=./
  key=$(printf '%s' "$rel" | tr '/.' '__')
  ( $GO test -race -c -o "$work/$key.test" "$rel" > "$work/$key.build.log" 2>&1; echo $? > "$work/$key.build.rc" ) &
  cpids+=($!)
done
[ "${#cpids[@]}" = 0 ] || wait "${cpids[@]}"
for p in $sharded; do
  rel=${p#"$mod"}; rel=.${rel}; [ "$rel" = . ] && rel=./
  key=$(printf '%s' "$rel" | tr '/.' '__')
  bin=$work/$key.test
  if [ "$(cat "$work/$key.build.rc" 2>/dev/null)" != 0 ]; then
    note "FAIL build of $p"; cat "$work/$key.build.log"; fail=1; continue
  fi
  if [ ! -x "$bin" ]; then note "$p has no tests"; continue; fi
  (cd "$rel" && "$bin" -test.list '.*') > "$work/$key.list" 2> "$work/$key.list.err" || {
    note "FAIL listing the tests of $p"; cat "$work/$key.list.err"; fail=1; continue; }
  # Benchmarks do not run without -bench; tests, fuzz seeds and examples do.
  grep -E '^(Test|Fuzz|Example)' "$work/$key.list" | sort > "$work/$key.names"
  total=$(wc -l < "$work/$key.names" | tr -d ' ')
  [ "$total" -gt 0 ] || { note "$p lists no tests"; continue; }
  n=$SHARDS; [ "$n" -le "$total" ] || n=$total
  # Longest recorded first onto the least loaded shard, then unknown tests
  # round-robin. Output: shard<TAB>name.
  awk -F'\t' -v pkg="$p" -v n="$n" '
    FNR == NR { if ($1 == pkg) d[$2] = $3 + 0; next }
    { if ($0 in d) { k++; kn[k] = $0 } else { u++; un[u] = $0 } }
    END {
      for (i = 2; i <= k; i++) { x = kn[i]; j = i - 1
        while (j > 0 && (d[kn[j]] < d[x] || (d[kn[j]] == d[x] && kn[j] > x))) { kn[j+1] = kn[j]; j-- }
        kn[j+1] = x }
      for (s = 1; s <= n; s++) load[s] = 0
      for (i = 1; i <= k; i++) { b = 1
        for (s = 2; s <= n; s++) if (load[s] < load[b]) b = s
        load[b] += d[kn[i]]; print b "\t" kn[i] }
      for (i = 1; i <= u; i++) print ((i - 1) % n) + 1 "\t" un[i]
    }' "$TIMINGS" "$work/$key.names" > "$work/$key.plan"
  # Every listed test is in exactly one shard.
  if [ "$(cut -f2 "$work/$key.plan" | sort)" != "$(cat "$work/$key.names")" ]; then
    note "FAIL the shards of $p are not exactly its $total tests"; fail=1; continue
  fi
  for s in $(seq 1 "$n"); do
    awk -F'\t' -v s="$s" '$1 == s { print $2 }' "$work/$key.plan" | sort > "$work/$key.$s.names"
    [ -s "$work/$key.$s.names" ] || continue
    re="^($(paste -sd'|' "$work/$key.$s.names"))\$"
    tmp=$(mktemp -d "${TMPDIR:-/tmp}/a2p2s.XXXXXX")
    start_job "$p shard $s/$n" "$work/$key.$s.log" \
      env TMPDIR="$tmp" sh -c 'cd "$1" && "$2" -test.paniconexit0 -test.count=1 -test.timeout="$3" -test.v -test.run "$4"; rc=$?; rm -rf "$5"; exit $rc' \
      sh "$rel" "$bin" "$TIMEOUT" "$re" "$tmp"
    eval "names_of_$((${#pids[@]} - 1))=\$work/\$key.\$s.names"
    eval "pkg_of_$((${#pids[@]} - 1))=\$p"
  done
  note "$p: $total tests in $n shards"
done

for i in "${!pids[@]}"; do
  if wait "${pids[$i]}"; then rc=0; else rc=$?; fi
  log=${logs[$i]}; name=${names[$i]}
  eval "nf=\${names_of_$i:-}"; eval "pk=\${pkg_of_$i:-}"
  if [ "$rc" = 0 ] && [ -n "$nf" ]; then
    # A shard that passed reported each of its tests exactly once.
    got=$(times "$pk" < "$log" | cut -f2 | sort)
    if [ "$got" != "$(cat "$nf")" ]; then
      rc=1; echo "gotest: $name exited 0 but did not report each of its tests once" >> "$log"
    fi
  fi
  if [ "$rc" = 0 ]; then
    note "ok   $name ($(( $(cat "$log.end" 2>/dev/null || date +%s) - t0 ))s)"
  else
    fail=1; note "FAIL $name (exit $rc); its output:"
    grep -v -E '^(=== (RUN|PAUSE|CONT|NAME)|--- PASS: |    --- PASS: )' "$log" | sed -n '1,4000p'
  fi
done

if [ -n "${TIMINGS_OUT:-}" ]; then
  {
    echo "# package<TAB>test<TAB>seconds under -race, written by make test-timings; scripts/gotest.sh shards by it"
    for i in "${!pids[@]}"; do
      eval "pk=\${pkg_of_$i:-}"
      times "$pk" < "${logs[$i]}"
    done | sort -t"$(printf '\t')" -k1,1 -k2,2
  } > "$TIMINGS_OUT"
  note "times written to $TIMINGS_OUT"
fi
if [ "$fail" != 0 ]; then
  echo "gotest: FAIL"; exit 1
fi
echo "gotest: ok"
