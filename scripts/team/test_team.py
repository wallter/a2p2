#!/usr/bin/env python3
"""Tests for the team launcher with a fake harness and no model tokens.

Each test uses its own throwaway A2P2_HOME under /tmp (short socket paths)
and stops that daemon with `a2p2 stop`. Run: python3 scripts/team/test_team.py
(make team-test). The a2p2 binary is $A2P2_BIN or bin/a2p2.
"""
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.realpath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
BIN = os.environ.get("A2P2_BIN") or os.path.join(ROOT, "bin", "a2p2")
TEAM = os.environ.get("TEAM_SCRIPT") or os.path.join(HERE, "team")


def fake(label, mode, limit=20, **kw):
    return dict(label=label, harness="command", mcp_harness="fake", limit_seconds=limit,
                command=[sys.executable, "{team_dir}/fake_harness.py", mode], **kw)


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


class TeamTest(unittest.TestCase):
    def setUp(self):
        if not os.access(BIN, os.X_OK):
            self.fail(f"build first (make build): {BIN} not found")
        self.dir = os.path.realpath(tempfile.mkdtemp(prefix="a2p2t", dir="/tmp"))
        os.chmod(self.dir, 0o700)
        self.home = os.path.join(self.dir, "home")
        self.manifest = os.path.join(self.dir, "team.json")
        self.state = os.path.join(self.dir, "state")
        self.bin = BIN
        # Real harness CLIs are shadowed: no test may start a model run,
        # even if a refusal it checks for were to regress.
        stubs = os.path.join(self.dir, "stubs")
        os.makedirs(stubs)
        for name in ("claude", "codex", "agy"):
            with open(os.path.join(stubs, name), "w") as f:
                f.write("#!/bin/sh\necho 'stub: no real harness in tests' >&2\nexit 97\n")
            os.chmod(os.path.join(stubs, name), 0o755)
        self.env = dict(os.environ, PATH=stubs + os.pathsep + os.environ.get("PATH", ""))

    def tearDown(self):
        # Let a live supervisor finish before the daemon goes away.
        deadline = time.time() + 15
        while time.time() < deadline and os.path.exists(os.path.join(self.state, "roster.json")):
            r = self.roster_raw()
            if r.get("ended") or not r.get("supervisor_pid") or not alive(r["supervisor_pid"]):
                break
            time.sleep(0.2)
        subprocess.run([BIN, "stop"], env=dict(os.environ, A2P2_HOME=self.home),
                       capture_output=True, timeout=30)
        shutil.rmtree(self.dir, ignore_errors=True)

    def write(self, members, **kw):
        m = {"room": "crew", "a2p2_home": self.home, "a2p2": self.bin, "state_dir": self.state,
             "poll_seconds": 0.2, "silence_seconds": 30, "members": members}
        m.update(kw)
        with open(self.manifest, "w") as f:
            json.dump(m, f)

    def team(self, *args, ok=True):
        r = subprocess.run([sys.executable, TEAM, "-m", self.manifest, *args],
                           capture_output=True, text=True, timeout=90, env=self.env)
        if ok:
            self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        return r

    def roster_raw(self):
        with open(os.path.join(self.state, "roster.json")) as f:
            return json.load(f)

    def roster(self):
        return {x["label"]: x for x in self.roster_raw()["members"]}

    def watcher_posts(self):
        out = subprocess.run([BIN, "history", "--room", "crew", "-n", "200", "--json"],
                             env=dict(os.environ, A2P2_HOME=self.home), capture_output=True, text=True).stdout
        return [e["text"] for e in json.loads(out)["entries"] if e["from"].startswith("team@")]

    def wait_for(self, cond, timeout=15):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if cond():
                return
            time.sleep(0.1)
        self.fail("timed out")

    def one(self, posts, prefix):
        got = [p for p in posts if p.startswith(prefix)]
        self.assertEqual(len(got), 1, f"{prefix!r} in {posts}")
        return got[0]

    def test_watcher_reports_each_ending_and_the_run_is_measured(self):
        self.write([fake("okay", "ok"), fake("bad", "fail"), fake("stuck", "hang", limit=3, silence_seconds=1),
                    fake("asker", "ask"), fake("ghost", "noconnect"), fake("loud", "inject")], max_members=6)
        self.team("up", "--wait")
        r = self.roster()
        self.assertEqual({k: (v["state"], v["exit"]) for k, v in r.items()},
                         {"okay": ("done", 0), "bad": ("failed", 3), "stuck": ("limit", 143),
                          "asker": ("done", 0), "ghost": ("not_ready", 0), "loud": ("done", 0)})
        posts = self.watcher_posts()
        for label in ("okay", "bad", "stuck", "asker", "loud"):
            self.assertIn("tool server connected", self.one(posts, f"started: {label} "))
        self.assertFalse([p for p in posts if p.startswith("started: ghost")])
        self.assertFalse([p for p in posts if p.startswith("done: ghost")])
        self.assertIn("never connected", self.one(posts, "not ready: ghost exited 0"))
        self.assertIn("final.txt", self.one(posts, "done: okay exited 0"))
        self.one(posts, "failed: bad exited 3")
        self.one(posts, "limit: stuck passed its 3s")
        self.one(posts, "silent: stuck has posted nothing")
        self.one(posts, "needs input: asker asked the person")
        # the injected text changed nothing: no stop
        self.assertFalse([p for p in posts if p.startswith("stopped:")])

        with open(os.path.join(self.state, "report.json")) as f:
            rep = {m["label"]: m for m in json.load(f)["members"]}
        self.assertEqual(rep["okay"]["messages_sent"], 2)
        self.assertEqual(rep["okay"]["refusals_by_code"], {"not_found": 1})
        self.assertGreaterEqual(rep["okay"]["empty_inbox_polls_in_logs"], 1)
        self.assertEqual(rep["okay"]["final_report"], "DONE")
        self.assertEqual(rep["bad"]["final_report"], "BLOCKED")
        self.assertEqual(rep["asker"]["questions_to_person"], 1)
        self.assertIsNone(rep["ghost"]["connected_within_s"])
        self.assertGreaterEqual(rep["stuck"]["wall_clock_s"], 3)
        with open(os.path.join(self.state, "report.md")) as f:
            self.assertIn("| okay | command | - | done (0) |", f.read())

    def test_status_logs_and_stop_one_member_through_the_supervisor(self):
        self.write([fake("stuck", "hang", limit=60), fake("okay", "ok")])
        self.team("up")
        self.wait_for(lambda: self.roster()["stuck"]["state"] == "running")
        self.assertRegex(self.team("status").stdout, r"stuck\s+command\s+\d+\s+running")
        self.assertIn("whoami ->", self.team("logs", "stuck").stdout)
        self.assertIn("stopped 1 member(s): stuck", self.team("stop", "stuck").stdout)
        self.wait_for(lambda: self.roster()["stuck"]["state"] == "stopped")
        self.wait_for(lambda: any(p.startswith("stopped: stuck by team stop") for p in self.watcher_posts()))
        self.wait_for(lambda: os.path.exists(os.path.join(self.state, "report.json")))
        self.assertIn("already holds a run", self.team("up", ok=False).stderr)

    def test_stop_never_signals_a_pid_read_from_disk(self):
        self.write([fake("stuck", "hang", limit=60)])
        self.team("up")
        self.wait_for(lambda: self.roster()["stuck"]["state"] == "running")
        work = self.roster()["stuck"]["workdir"]
        self.wait_for(lambda: os.path.exists(os.path.join(work, "pid")))
        with open(os.path.join(work, "pid")) as f:
            member = int(f.read())
        # The supervisor dies without cleaning up; its guard ends the member.
        os.kill(self.roster_raw()["supervisor_pid"], signal.SIGKILL)
        self.wait_for(lambda: not alive(member), timeout=10)
        # A process that now happens to hold the recorded pid's place.
        bystander = subprocess.Popen(["sleep", "30"], start_new_session=True)
        try:
            r = self.roster_raw()
            r["members"][0]["pid"] = bystander.pid
            r["supervisor_pid"] = bystander.pid
            with open(os.path.join(self.state, "roster.json"), "w") as f:
                json.dump(r, f)
            out = self.team("stop").stdout
            self.assertIn("not live", out)
            self.assertIsNone(bystander.poll(), "stop signalled a pid read from disk")
            self.assertEqual(self.roster()["stuck"]["state"], "lost")
            self.assertTrue(self.roster_raw()["ended"])
        finally:
            bystander.kill()
            bystander.wait()

    def test_a_terminated_supervisor_stops_its_members_first(self):
        self.write([fake("stuck", "hang", limit=60)])
        self.team("up")
        self.wait_for(lambda: self.roster()["stuck"]["state"] == "running")
        os.kill(self.roster_raw()["supervisor_pid"], signal.SIGTERM)
        self.wait_for(lambda: self.roster_raw().get("ended"))
        self.assertEqual(self.roster()["stuck"]["state"], "stopped")
        self.one(self.watcher_posts(), "stopped: stuck because the supervisor ended")

    def test_two_ups_at_once_one_wins(self):
        self.write([fake("okay", "ok")])
        procs = [subprocess.Popen([sys.executable, TEAM, "-m", self.manifest, "up"], stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, text=True, env=self.env) for _ in range(2)]
        results = [p.communicate(timeout=60) + (p.returncode,) for p in procs]
        self.assertEqual(sorted(r[2] for r in results), [0, 1], results)
        self.assertIn("already holds a run", next(r[1] for r in results if r[2] == 1))
        self.wait_for(lambda: self.roster_raw().get("ended"))

    def test_a_reused_label_refreshed_by_the_cli_is_not_ready(self):
        # The label was registered earlier under the same harness name, from another directory.
        subprocess.run([BIN, "mcp", "--harness", "fake", "--name", "ghost"], cwd=self.dir, stdin=subprocess.DEVNULL,
                       env=dict(os.environ, A2P2_HOME=self.home), capture_output=True, timeout=30)
        self.write([fake("ghost", "clionly")])
        self.team("up", "--wait")
        self.assertEqual(self.roster()["ghost"]["state"], "not_ready")
        posts = self.watcher_posts()
        self.assertFalse([p for p in posts if p.startswith("started: ghost")], posts)
        self.one(posts, "not ready: ghost exited 0")

    def test_limit_is_enforced_while_room_calls_block(self):
        mark = os.path.join(self.dir, "slow")
        wrapper = os.path.join(self.dir, "a2p2-slow")
        with open(wrapper, "w") as f:
            f.write(f'#!/bin/sh\nif [ -e "{mark}" ] && [ "$1" = history ]; then sleep 4; fi\nexec "{BIN}" "$@"\n')
        os.chmod(wrapper, 0o755)
        self.bin = wrapper
        self.write([fake("stuck", "hang", limit=1)], poll_seconds=30)
        self.team("up")
        open(mark, "w").close()
        t0 = time.time()
        self.wait_for(lambda: self.roster()["stuck"]["exit"] is not None, timeout=5)
        self.assertLess(time.time() - t0, 3)
        self.assertEqual(self.roster()["stuck"]["exit"], 143)
        self.assertLess(self.roster()["stuck"]["wall_clock"], 2.5)
        os.remove(mark)
        self.wait_for(lambda: self.roster_raw().get("ended"))
        self.assertEqual(self.roster()["stuck"]["state"], "limit")

    def test_antigravity_config_in_a_checkout_is_never_overwritten(self):
        outside = os.path.join(self.dir, "outside.json")
        with open(outside, "w") as f:
            f.write("keep")
        for kind in ("file", "symlink"):
            repo = os.path.join(self.dir, "repo-" + kind)
            os.makedirs(os.path.join(repo, ".agents"))
            cfg = os.path.join(repo, ".agents", "mcp_config.json")
            if kind == "file":
                with open(cfg, "w") as f:
                    f.write("keep")
            else:
                os.symlink(outside, cfg)
            git = ["git", "-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t"]
            subprocess.run(git[:3] + ["init", "-q"], check=True)
            subprocess.run(git + ["add", "-A"], check=True)
            subprocess.run(git + ["commit", "-q", "-m", "x"], check=True)
            self.state = os.path.join(self.dir, "state-" + kind)
            self.write([{"label": "ag", "harness": "agy", "model": "m", "worktree": repo}])
            err = self.team("up", ok=False).stderr
            self.assertIn("will not overwrite", err)
            with open(cfg) as f:
                self.assertEqual(f.read(), "keep")
        self.assertFalse(os.path.exists(self.home), "a refused member must not start a daemon")

    def test_manifest_refuses_bypass_modes_and_bad_limits(self):
        cases = [
            ([fake("a", "ok"), fake("b", "ok")], {"max_members": 1}, "more than max_members"),
            ([fake("a", "ok"), fake("a", "ok")], {}, "used twice"),
            ([fake("team", "ok")], {}, "used twice"),
            ([{"label": "c", "harness": "claude", "model": "haiku", "permission_mode": "bypassPermissions"}], {},
             "'default', 'acceptEdits', 'plan'"),
            ([{"label": "c", "harness": "claude", "model": "haiku", "allowed_tools": ["Bash"]}], {},
             "skip Claude Code's permission checks"),
            ([{"label": "x", "harness": "codex", "model": "m", "sandbox": "danger-full-access"}], {},
             "read-only, workspace-write"),
            ([{"label": "x", "harness": "codex", "model": "m", "dangerously_bypass": True}], {}, "keys not used"),
            ([{"label": "x", "harness": "codex", "model": "m", "permission_mode": "plan"}], {}, "keys not used"),
            ([fake("a", "ok", limit=0)], {}, "limit_seconds must be a finite number above zero"),
            ([fake("a", "ok", limit=-5)], {}, "limit_seconds must be a finite number above zero"),
            ([fake("a", "ok", limit=float("inf"))], {}, "limit_seconds must be a finite number above zero"),
            ([fake("a", "ok", limit=float("nan"))], {}, "limit_seconds must be a finite number above zero"),
            ([fake("a", "ok", limit="10")], {}, "limit_seconds must be a finite number above zero"),
            ([fake("a", "ok")], {"poll_seconds": 0}, "poll_seconds must be a finite number above zero"),
        ]
        for members, extra, want in cases:
            with self.subTest(want=want, members=members):
                self.write(members, **extra)
                self.assertIn(want, self.team("up", ok=False).stderr)
        self.assertFalse(os.path.exists(self.home), "a refused manifest must not start a daemon")
        self.assertFalse(os.path.exists(self.state), "a refused manifest must not reserve the state")


if __name__ == "__main__":
    unittest.main(verbosity=2)
