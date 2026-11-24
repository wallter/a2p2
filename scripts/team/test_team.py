#!/usr/bin/env python3
"""Tests for the team launcher with a fake harness and no model tokens.

Each test uses its own throwaway A2P2_HOME under /tmp (short socket paths)
and stops that daemon with `a2p2 stop`. Run: python3 scripts/team/test_team.py
(make team-test). The a2p2 binary is $A2P2_BIN or bin/a2p2.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.realpath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
BIN = os.environ.get("A2P2_BIN") or os.path.join(ROOT, "bin", "a2p2")
TEAM = os.path.join(HERE, "team")


def fake(label, mode, limit=20, **kw):
    return dict(label=label, harness="command", mcp_harness="fake", limit_seconds=limit,
                command=[sys.executable, "{team_dir}/fake_harness.py", mode], **kw)


class TeamTest(unittest.TestCase):
    def setUp(self):
        if not os.access(BIN, os.X_OK):
            self.fail(f"build first (make build): {BIN} not found")
        self.dir = tempfile.mkdtemp(prefix="a2p2t", dir="/tmp")
        os.chmod(self.dir, 0o700)
        self.home = os.path.join(self.dir, "home")
        self.manifest = os.path.join(self.dir, "team.json")

    def tearDown(self):
        subprocess.run([BIN, "stop"], env=dict(os.environ, A2P2_HOME=self.home),
                       capture_output=True, timeout=30)
        shutil.rmtree(self.dir, ignore_errors=True)

    def write(self, members, **kw):
        m = {"room": "crew", "a2p2_home": self.home, "a2p2": BIN, "state_dir": os.path.join(self.dir, "state"),
             "poll_seconds": 0.2, "silence_seconds": 30, "members": members}
        m.update(kw)
        with open(self.manifest, "w") as f:
            json.dump(m, f)

    def team(self, *args, ok=True):
        r = subprocess.run([sys.executable, TEAM, "-m", self.manifest, *args],
                           capture_output=True, text=True, timeout=60)
        if ok:
            self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        return r

    def roster(self):
        with open(os.path.join(self.dir, "state", "roster.json")) as f:
            return {x["label"]: x for x in json.load(f)["members"]}

    def watcher_posts(self):
        out = subprocess.run([BIN, "history", "--room", "crew", "-n", "200", "--json"],
                             env=dict(os.environ, A2P2_HOME=self.home), capture_output=True, text=True).stdout
        return [e["text"] for e in json.loads(out)["entries"] if e["from"].startswith("team@")]

    def wait_for(self, cond, timeout=15):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if cond():
                return
            time.sleep(0.2)
        self.fail("timed out")

    def test_watcher_reports_each_ending_and_the_run_is_measured(self):
        self.write([fake("okay", "ok"), fake("bad", "fail"), fake("stuck", "hang", limit=3, silence_seconds=1),
                    fake("asker", "ask"), fake("ghost", "noconnect"), fake("loud", "inject")], max_members=6)
        self.team("up", "--wait")
        r = self.roster()
        self.assertEqual({k: (v["state"], v["exit"]) for k, v in r.items()},
                         {"okay": ("done", 0), "bad": ("failed", 3), "stuck": ("limit", -15),
                          "asker": ("done", 0), "ghost": ("done", 0), "loud": ("done", 0)})
        posts = self.watcher_posts()

        def one(prefix):
            got = [p for p in posts if p.startswith(prefix)]
            self.assertEqual(len(got), 1, f"{prefix!r} in {posts}")
            return got[0]

        # started only after the tool server registered the label
        for label in ("okay", "bad", "stuck", "asker", "loud"):
            self.assertIn("tool server connected", one(f"started: {label} "))
        self.assertFalse([p for p in posts if p.startswith("started: ghost")])
        self.assertIn("never connected", one("done: ghost "))
        self.assertIn("final.txt", one("done: okay exited 0"))
        one("failed: bad exited 3")
        one("limit: stuck passed its 3s")
        one("silent: stuck has posted nothing")
        one("needs input: asker asked the person")
        # the injected text changed nothing: no stop, and okay is done, not stopped
        self.assertFalse([p for p in posts if p.startswith("stopped:")])

        with open(os.path.join(self.dir, "state", "report.json")) as f:
            rep = {m["label"]: m for m in json.load(f)["members"]}
        self.assertEqual(rep["okay"]["messages_sent"], 2)
        self.assertEqual(rep["okay"]["refusals_by_code"], {"not_found": 1})
        self.assertGreaterEqual(rep["okay"]["empty_inbox_polls_in_logs"], 1)
        self.assertEqual(rep["okay"]["final_report"], "DONE")
        self.assertEqual(rep["bad"]["final_report"], "BLOCKED")
        self.assertEqual(rep["asker"]["questions_to_person"], 1)
        self.assertIsNone(rep["ghost"]["connected_after_s"])
        self.assertGreaterEqual(rep["stuck"]["wall_clock_s"], 3)
        with open(os.path.join(self.dir, "state", "report.md")) as f:
            md = f.read()
        self.assertIn("| okay | command | - | done (0) |", md)

    def test_status_logs_and_stop_one_member(self):
        self.write([fake("stuck", "hang", limit=60), fake("okay", "ok")])
        self.team("up")
        self.wait_for(lambda: self.roster()["stuck"]["state"] == "running")
        out = self.team("status").stdout
        self.assertRegex(out, r"stuck\s+command\s+\d+\s+running")
        self.assertIn("whoami ->", self.team("logs", "stuck").stdout)
        self.team("stop", "stuck")
        self.wait_for(lambda: self.roster()["stuck"]["state"] == "stopped")
        self.wait_for(lambda: any(p.startswith("stopped: stuck by team stop") for p in self.watcher_posts()))
        self.wait_for(lambda: os.path.exists(os.path.join(self.dir, "state", "report.json")))
        self.assertIn("already holds a run", self.team("up", ok=False).stderr)

    def test_manifest_limits(self):
        self.write([fake("a", "ok"), fake("b", "ok")], max_members=1)
        self.assertIn("more than max_members", self.team("up", ok=False).stderr)
        self.write([fake("a", "ok"), fake("a", "ok")])
        self.assertIn("used twice", self.team("up", ok=False).stderr)
        self.write([fake("team", "ok")])
        self.assertIn("used twice", self.team("up", ok=False).stderr)
        self.assertFalse(os.path.exists(self.home), "a refused manifest must not start a daemon")


if __name__ == "__main__":
    unittest.main(verbosity=2)
