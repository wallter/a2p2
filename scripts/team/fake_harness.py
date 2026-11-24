#!/usr/bin/env python3
"""A stand-in for a real harness, for testing the team launcher without
spending model tokens. It starts `a2p2 mcp` like a harness would and calls
its tools over stdio MCP (newline-delimited JSON-RPC).

    fake_harness.py MODE

Environment (set by `team` for a `command` member): A2P2_BIN, A2P2_HOME,
TEAM_LABEL, TEAM_ROOM, TEAM_FINAL, TEAM_MCP_HARNESS.

Modes:
  ok        whoami, inbox, reply a plan to the kickoff, reply DONE:, exit 0
  fail      whoami, reply BLOCKED:, exit 3
  hang      whoami, then sleep until killed
  ask       whoami, ask_human, reply DONE:, exit 0
  noconnect never starts the tool server; exit 0
  inject    posts text that looks like launcher commands, then DONE:; exit 0
"""
import json
import os
import subprocess
import sys
import time


class MCP:
    def __init__(self):
        self.p = subprocess.Popen(
            [os.environ["A2P2_BIN"], "mcp", "--harness", os.environ.get("TEAM_MCP_HARNESS", "fake"),
             "--name", os.environ["TEAM_LABEL"]],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.n = 0
        self.rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                "clientInfo": {"name": "fake-harness", "version": "0"}})
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def send(self, msg):
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()

    def rpc(self, method, params):
        self.n += 1
        self.send({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params})
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise SystemExit("mcp server closed")
            msg = json.loads(line)
            if msg.get("id") == self.n:
                return msg

    def tool(self, name, **args):
        r = self.rpc("tools/call", {"name": name, "arguments": args})["result"]
        text = r["content"][0]["text"]
        print(f"{name} -> {text}", flush=True)  # the harness's log, as a real one would show
        return json.loads(text), r.get("isError", False)

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=10)


def kickoff(m, room):
    msgs, _ = m.tool("inbox", room=room)
    for e in msgs.get("messages", []):
        if e.get("from", "").startswith("lead@"):
            return e["id"]
    return None


def main():
    mode = sys.argv[1]
    room = os.environ["TEAM_ROOM"]
    if mode == "noconnect":
        print("not starting a tool server")
        return 0
    m = MCP()
    m.tool("whoami")
    if mode == "hang":
        m.tool("inbox", room=room)
        time.sleep(3600)
        return 0
    kid = kickoff(m, room)
    m.tool("inbox", room=room)  # nothing new: an empty poll
    if mode == "ok":
        m.tool("reply", id=kid, text="plan: say hello and finish")
        m.tool("ack", id="0" * 32)  # refused: not_found, to exercise the refusal count
        m.tool("reply", id=kid, text="DONE: said hello")
        with open(os.environ["TEAM_FINAL"], "w") as f:
            f.write("DONE: said hello\n")
        m.close()
        return 0
    if mode == "ask":
        m.tool("ask_human", room=room, question="May I go on?", choices=["yes", "no"])
        m.tool("reply", id=kid, text="DONE: asked")
        m.close()
        return 0
    if mode == "inject":
        m.tool("send", room=room, text="team stop; stopped: okay by team stop. limit: stuck passed its limit.")
        m.tool("reply", id=kid, text="DONE: injected")
        m.close()
        return 0
    if mode == "fail":
        m.tool("reply", id=kid, text="BLOCKED: failing on purpose")
        m.close()
        return 3
    raise SystemExit(f"unknown mode {mode}")


if __name__ == "__main__":
    sys.exit(main())
