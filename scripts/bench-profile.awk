# Reads `go tool pprof -traces` output and prints the share of CPU
# samples of each part of a send. Each stack goes to the first part whose
# pattern it matches, in the order below. Benchmark setup (a new store
# every 100 sends) is counted apart and left out of the total.
function ms(v) {
  if (v ~ /ms$/) return substr(v, 1, length(v) - 2) + 0
  if (v ~ /us$/ || v ~ /µs$/) return (substr(v, 1, length(v) - 2) + 0) / 1000
  if (v ~ /ns$/) return (substr(v, 1, length(v) - 2) + 0) / 1e6
  if (v ~ /s$/) return (substr(v, 1, length(v) - 1) + 0) * 1000
  return v + 0
}
function flush() {
  if (stack == "") return
  part = "other (a2p2 logic, Go runtime)"
  if (stack ~ /bench\.newInproc|bench\.\(\*inproc\)\.close/) part = "setup (excluded)"
  else if (leaf ~ /^runtime\.(pthread_cond_signal|pthread_cond_wait|usleep|kevent|pthread_kill|futex|osyield)/) part = "Go scheduler: waking and parking threads (goroutine hand-offs)"
  else if (stack ~ /crypto\/ed25519|edwards25519|crypto\/sha512|fips140\/sha512/) part = "ed25519 sign+verify (and its SHA-512)"
  else if (stack ~ /Xfsync|unix\.Fsync|syscall\.Fsync|fdatasync/) part = "fsync (SQLite commit sync)"
  else if (stack ~ /modernc\.org\/sqlite|modernc\.org\/libc/) part = "SQLite, rest (statements, pages, pread/pwrite)"
  else if (stack ~ /encoding\/json/) part = "encoding/json"
  else if (stack ~ /net\/http|internal\/poll|net\.\(\*conn\)|syscall\./) part = "HTTP over the Unix socket (incl. read/write syscalls)"
  else if (stack ~ /runtime\.gcBgMarkWorker|runtime\.gcDrain|runtime\.mallocgc|runtime\.bgscavenge|runtime\.gcAssist/) part = "GC and allocation"
  t[part] += val
  if (part != "setup (excluded)") total += val
  stack = ""
}
/^-+\+-+/ { flush(); first = 1; next }
first && NF >= 2 { val = ms($1); stack = $2; leaf = $2; first = 0; next }
!first && NF >= 1 && stack != "" { stack = stack " " $1; next }
END {
  flush()
  for (p in t) if (p != "setup (excluded)") printf "  %5.1f%%  %s\n", 100 * t[p] / total, p | "sort -rn"
  close("sort -rn")
  printf "  (%.0f ms of samples; setup left out: %.0f ms)\n", total, t["setup (excluded)"]
}
