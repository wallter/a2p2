#!/bin/sh
# Requirement I4: a2p2 has few direct dependencies, each one chosen on
# purpose. Fail if go.mod requires a direct dependency that is not on the
# allow-list below, and print where each one's licence file is in the
# module cache. Adding a dependency means a DECISIONS.md entry and a line
# here. Works offline once the module cache holds the dependencies.
set -eu
cd "$(dirname "$0")/.."

allowed="github.com/modelcontextprotocol/go-sdk
modernc.org/sqlite"

# Direct requirements of go.mod as `go mod edit -fmt` writes it: in a
# require block or on a single require line, without "// indirect".
direct=$(go mod edit -print | awk '
	/^require \($/ { inblock = 1; next }
	inblock && /^\)/ { inblock = 0; next }
	/\/\/ indirect/ { next }
	inblock && NF >= 2 { print $1 }
	/^require [^(]/ { print $2 }')
status=0
for m in $direct; do
	if ! printf '%s\n' "$allowed" | grep -qxF "$m"; then
		echo "licences: $m is a direct dependency that is not on the allow-list in scripts/licences.sh" >&2
		status=1
		continue
	fi
	dir=$(go list -m -f '{{.Dir}}' "$m" 2>/dev/null || true)
	lic=""
	if [ -n "$dir" ]; then
		lic=$(ls "$dir" | grep -iE '^(licen[cs]e|copying)' | head -1 || true)
	fi
	if [ -n "$lic" ]; then
		echo "$m: $dir/$lic"
	else
		echo "$m: no licence file found in the module cache (run: go mod download $m)"
	fi
done
exit $status
