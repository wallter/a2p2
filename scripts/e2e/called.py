"""called.py DIR TOOL: exit 0 when the agent run in DIR called the a2p2 tool.

MCP harnesses: DIR/mcp.log holds the JSON-RPC frames the harness sent to
a2p2 mcp (see logged in local.sh). Pi: DIR/pi.jsonl is pi --mode json
output, and TOOL "status" means a bash call that ran a2p2 status.
"""
import json
import os
import sys


def frames(path):
    if not os.path.exists(path):
        return
    with open(path, errors="replace") as f:
        for line in f:
            try:
                yield json.loads(line)
            except ValueError:
                continue


def main():
    d, tool = sys.argv[1], sys.argv[2]
    for m in frames(os.path.join(d, "mcp.log")):
        if isinstance(m, dict) and m.get("method") == "tools/call" and (m.get("params") or {}).get("name") == tool:
            return 0
    for e in frames(os.path.join(d, "pi.jsonl")):
        if not isinstance(e, dict) or e.get("type") != "tool_execution_start":
            continue
        cmd = str((e.get("args") or {}).get("command", ""))
        if "a2p2" in cmd and " " + tool in cmd:
            return 0
    return 1


sys.exit(main())
