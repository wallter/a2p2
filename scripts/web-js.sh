#!/usr/bin/env bash
# Run the web page's script (internal/web/static/app.js) in Node against the
# real handler: the test TestPageScriptDrawsTheSidebarInNode. Needs node on
# PATH. Without it this prints a line "gate: skip: REASON" and exits 0, and
# scripts/gate.sh shows the step as "skip" and counts it as skipped, so a
# skipped check is never reported as passed. With A2P2_REQUIRE_NODE=1 a missing node is a failure.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v node >/dev/null 2>&1; then
  if [ "${A2P2_REQUIRE_NODE:-}" = 1 ]; then
    echo "web-js: node is not on PATH and A2P2_REQUIRE_NODE=1"; exit 1
  fi
  echo "gate: skip: node is not on PATH, so the web page's script was not run"
  exit 0
fi
echo "web-js: $(node --version)"
A2P2_REQUIRE_NODE=1 go test -count=1 -run '^TestPageScriptDrawsTheSidebarInNode$' ./internal/web
