#!/usr/bin/env bash
# Check docs/TRACE.md against docs/REQUIREMENTS.md and the test suite.
# Fails if a requirement has no row, a row has no requirement, a named test
# does not exist, or a row says "met" without naming a test.
set -euo pipefail
cd "$(dirname "$0")/.."
req=$(grep -oE '^- \*\*[A-Z][0-9]+ ' docs/REQUIREMENTS.md | grep -oE '[A-Z][0-9]+' | grep -v '^P' | sort -u)
rows=$(grep -oE '^\| [A-Z][0-9]+ \|' docs/TRACE.md | grep -oE '[A-Z][0-9]+' | sort)
fail=0
dup=$(printf '%s\n' "$rows" | uniq -d)
[ -z "$dup" ] || { echo "trace: more than one row for: $dup" | tr '\n' ' '; echo; fail=1; }
missing=$(comm -23 <(printf '%s\n' "$req") <(printf '%s\n' "$rows" | sort -u))
[ -z "$missing" ] || { echo "trace: requirement without a row: $missing" | tr '\n' ' '; echo; fail=1; }
extra=$(comm -13 <(printf '%s\n' "$req") <(printf '%s\n' "$rows" | sort -u))
[ -z "$extra" ] || { echo "trace: row without a requirement: $extra" | tr '\n' ' '; echo; fail=1; }
tests=$(grep -rhoE '^func (Test|Fuzz)[A-Za-z0-9_]+' --include='*_test.go' cmd internal | awk '{print $2}' | sort -u)
while IFS= read -r line; do
  id=$(printf '%s' "$line" | awk -F'|' '{gsub(/ /,"",$2); print $2}')
  status=$(printf '%s' "$line" | awk -F'|' '{gsub(/^ +| +$/,"",$3); print $3}')
  named=$(printf '%s' "$line" | grep -oE '`(Test|Fuzz)[A-Za-z0-9_]+`' | tr -d '`' || true)
  for t in $named; do
    printf '%s\n' "$tests" | grep -qx "$t" || { echo "trace: $id names $t, which does not exist"; fail=1; }
  done
  if [ "$status" = met ] && [ -z "$named" ]; then echo "trace: $id is 'met' without a test"; fail=1; fi
  case "$status" in met|partly|untested|"not yet") ;; *) echo "trace: $id has unknown status '$status'"; fail=1 ;; esac
done < <(grep -E '^\| [A-Z][0-9]+ \|' docs/TRACE.md)
[ "$fail" = 0 ] || exit 1
total=$(printf '%s\n' "$rows" | wc -l | tr -d ' ')
printf 'trace: %s requirements;' "$total"
for s in met partly untested "not yet"; do
  n=$(grep -E '^\| [A-Z][0-9]+ \|' docs/TRACE.md | awk -F'|' -v s="$s" '{gsub(/^ +| +$/,"",$3); if ($3==s) n++} END{print n+0}')
  printf ' %s %s;' "$n" "$s"
done
echo
