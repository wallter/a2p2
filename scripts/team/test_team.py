#!/usr/bin/env python3
"""Tests for the team launcher with a fake harness and no model tokens.

Each test uses its own throwaway A2P2_HOME under /tmp (short socket paths)
and stops that daemon with `a2p2 stop`. Run: python3 scripts/team/test_team.py
(make team-test). The a2p2 binary is $A2P2_BIN or bin/a2p2.

Any a2p2 command starts the daemon of its A2P2_HOME and creates the home, so
a process left over from a test would bring a stopped daemon back. tearDown
therefore waits for the supervisor to exit before it stops the daemon, and
when the run ends the program fails if a daemon or any process mentioning
one of its directories is alive, or a directory remains.
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


RUN_DIRS = []   # every directory this run created


def load_team():
    """The launcher as a module, for tests of one function."""
    import importlib.machinery
    import importlib.util
    loader = importlib.machinery.SourceFileLoader("team_under_test", TEAM)
    team = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    loader.exec_module(team)
    return team


def proc_lines():
    """(pid, command line) of every process."""
    out = subprocess.run(["ps", "-axo", "pid=,command="], capture_output=True, text=True).stdout
    return [(int(a), b) for a, b in (ln.strip().split(None, 1) for ln in out.splitlines() if ln.strip())]


def open_paths(pid):
    """Paths a process has open, from /proc or lsof; [] if it cannot be read."""
    try:
        return [os.readlink(f"/proc/{pid}/fd/{fd}") for fd in os.listdir(f"/proc/{pid}/fd")]
    except OSError:
        pass
    try:
        out = subprocess.run(["lsof", "-p", str(pid), "-Fn"], capture_output=True, text=True).stdout
    except OSError:
        return []
    return [ln[1:] for ln in out.splitlines() if ln.startswith("n")]


def leftovers():
    """What of this run is still alive or on disk, as readable lines."""
    found = []
    for pid, cmd in proc_lines():
        if pid == os.getpid():
            continue
        mine = [d for d in RUN_DIRS if d in cmd]
        if cmd.endswith(" daemon") and "a2p2" in cmd:
            mine += [d for p in open_paths(pid) for d in RUN_DIRS if p.startswith(d + os.sep)]
        if mine:
            found.append(f"process {pid} ({cmd[:100]}) uses {mine[0]}")
    found += [f"directory {d} remains" for d in RUN_DIRS if os.path.exists(d)]
    return found


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


class TeamCase(unittest.TestCase):
    """A throwaway home, state directory and manifest per test."""

    def setUp(self):
        if not os.access(BIN, os.X_OK):
            self.fail(f"build first (make build): {BIN} not found")
        self.dir = os.path.realpath(tempfile.mkdtemp(prefix="a2p2t", dir="/tmp"))
        os.chmod(self.dir, 0o700)
        RUN_DIRS.append(self.dir)
        self.supervisor = None
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
        # Everything the test started must be gone before the daemon is
        # stopped: a supervisor, watcher or member that is still running
        # would call a2p2 again and start the daemon and the home anew.
        self.sees_supervisor()
        if os.path.exists(os.path.join(self.state, "roster.json")):
            subprocess.run([sys.executable, TEAM, "-m", self.manifest, "stop"], env=self.env,
                           capture_output=True, timeout=60)
        deadline = time.time() + 30
        while self.supervisor and alive(self.supervisor) and time.time() < deadline:
            time.sleep(0.1)
        still = bool(self.supervisor and alive(self.supervisor))
        subprocess.run([BIN, "stop"], env=dict(os.environ, A2P2_HOME=self.home),
                       capture_output=True, timeout=30)
        shutil.rmtree(self.dir, ignore_errors=True)
        if still:
            self.fail(f"the supervisor (pid {self.supervisor}) was still running after team stop")

    def sees_supervisor(self):
        """Remember the supervisor's pid, as the first one the roster shows."""
        if self.supervisor is None:
            try:
                with open(os.path.join(self.state, "roster.json")) as f:
                    self.supervisor = json.load(f).get("supervisor_pid")
            except (OSError, ValueError):
                pass

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
            r = json.load(f)
        if self.supervisor is None and r.get("supervisor_pid"):
            self.supervisor = r["supervisor_pid"]
        return r

    def roster(self):
        return {x["label"]: x for x in self.roster_raw()["members"]}

    def watcher_posts(self):
        out = subprocess.run([BIN, "history", "--room", "crew", "-n", "200", "--json"],
                             env=dict(os.environ, A2P2_HOME=self.home), capture_output=True, text=True).stdout
        return [e["text"] for e in json.loads(out)["entries"] if e["from"].startswith("team@")]

    def wait_for(self, cond, timeout=30):
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


class TeamTest(TeamCase):
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
            with open(os.path.join(self.state, "roster.json"), "rb") as f:
                before = f.read()
            r = self.team("stop", ok=False)
            self.assertEqual(r.returncode, 1, r.stdout)
            self.assertIn("no supervisor answered", r.stderr)
            self.assertIn("Nothing was signalled and the roster is unchanged", r.stderr)
            self.assertIsNone(bystander.poll(), "stop signalled a pid read from disk")
            with open(os.path.join(self.state, "roster.json"), "rb") as f:
                self.assertEqual(f.read(), before, "an unanswered stop changed the roster")
            self.assertFalse(self.roster_raw()["ended"])
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

    def test_a_run_marked_ended_makes_no_more_a2p2_calls(self):
        # The report calls a2p2, so it comes before "ended"; a call after
        # it would start a daemon that was stopped in the meantime.
        calls = os.path.join(self.dir, "calls")
        wrapper = os.path.join(self.dir, "a2p2-count")
        with open(wrapper, "w") as f:
            f.write(f'#!/bin/sh\necho "$1" >> "{calls}"\n[ "$1" = history ] && sleep 0.5\nexec "{BIN}" "$@"\n')
        os.chmod(wrapper, 0o755)
        self.bin = wrapper
        self.write([fake("okay", "ok")])
        self.team("up")
        self.wait_for(lambda: self.roster_raw().get("ended"), timeout=30)   # slow history calls
        self.assertTrue(os.path.exists(os.path.join(self.state, "report.json")), "ended before the report")
        with open(calls) as f:
            before = f.read()
        time.sleep(1)
        with open(calls) as f:
            self.assertEqual(f.read(), before, "a2p2 was called after the run was marked ended")
        self.wait_for(lambda: not alive(self.supervisor), timeout=10)

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
            self.write([{"label": "ag", "harness": "agy", "model": "m", "worktree": repo}], approve_a2p2_tools=True)
            err = self.team("up", ok=False).stderr
            self.assertIn("will not overwrite", err)
            with open(cfg) as f:
                self.assertEqual(f.read(), "keep")
        self.assertFalse(os.path.exists(self.home), "a refused member must not start a daemon")

    def test_a2p2_tools_are_approved_for_headless_members_only_when_the_manifest_says_so(self):
        # Without the key the run is refused before anything starts (see
        # test_manifest_refuses_bypass_modes_and_bad_limits). With it, each
        # member's command line approves a2p2's tools, and Claude Code's
        # names them one by one instead of a pattern.
        self.write([{"label": "cx", "harness": "codex", "model": "m", "limit_seconds": 5},
                    {"label": "cl", "harness": "claude", "model": "haiku", "limit_seconds": 5,
                     "allowed_tools": ["Read"]}], approve_a2p2_tools=True)
        self.team("up")
        self.sees_supervisor()
        specs = {}
        for label in ("cx", "cl"):
            with open(os.path.join(self.state, "members", label, "launch.json")) as f:
                specs[label] = json.load(f)["argv"]
        self.assertIn('mcp_servers.a2p2.default_tools_approval_mode="approve"', specs["cx"])
        for label in ("cx", "cl"):
            with open(os.path.join(self.state, "members", label, "launch.json")) as f:
                self.assertIs(json.load(f)["approve_a2p2_tools"], True)
        self.assertEqual(os.stat(self.state).st_mode & 0o777, 0o700)
        allowed = specs["cl"][specs["cl"].index("--allowedTools") + 1].split(",")
        self.assertEqual(allowed, ["mcp__a2p2__" + t for t in
                                   ("whoami", "peers", "send", "inbox", "reply", "ack", "ask_human")] + ["Read"])

    def test_status_and_stop_work_for_a_run_whose_manifest_lacks_the_approval(self):
        # A run started before the key existed, or whose manifest lost it
        # afterwards, can still be inspected and stopped: only `up` asks.
        self.write([{"label": "cx", "harness": "codex", "model": "m", "limit_seconds": 30}], approve_a2p2_tools=True)
        self.team("up")
        self.sees_supervisor()
        self.write([{"label": "cx", "harness": "codex", "model": "m", "limit_seconds": 30}])
        self.assertIn("cx", self.team("status").stdout)
        self.team("stop")
        self.team("logs", "cx")
        self.team("report")
        self.assertIn('"approve_a2p2_tools": true', self.team("up", ok=False).stderr)

    def test_supervisor_starts_no_headless_member_whose_spec_lacks_the_approval(self):
        # The consent goes with the saved command line: a spec without it,
        # such as one written for another manifest, is not started.
        team = load_team()
        d = os.path.join(self.state, "members", "ag")
        os.makedirs(d)
        with open(os.path.join(d, "launch.json"), "w") as f:
            json.dump({"argv": ["agy", "-p=x"], "cwd": self.dir, "env": {}}, f)
        sup = team.Supervisor.__new__(team.Supervisor)
        sup.man, sup.lock, sup.posts, sup.procs = {"state": self.state}, team.threading.Lock(), team.queue.Queue(), {}
        x = {"label": "ag", "harness": "agy", "state": "pending", "launched": None,
             "out": os.path.join(d, "out.log"), "err": os.path.join(d, "err.log")}
        sup.roster = {"members": [x]}
        os.makedirs(self.state, exist_ok=True)
        sup.launch(x)
        self.assertEqual(x["state"], "failed")
        self.assertIn("approve_a2p2_tools", x["note"])
        self.assertEqual(sup.procs, {})

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
            ([{"label": "x", "harness": "codex", "model": "m"}], {}, '"approve_a2p2_tools": true'),
            ([{"label": "c", "harness": "claude", "model": "haiku"}], {"approve_a2p2_tools": False},
             "send, reply, ack and ask_human included"),
            ([{"label": "g", "harness": "agy", "model": "m"}], {"approve_a2p2_tools": "yes"}, "must be true or false"),
            ([{"label": "g", "harness": "agy", "model": "m"}], {}, '"approve_a2p2_tools": true'),
            ([{"label": "c", "harness": "claude", "model": "--dangerously-skip-permissions"}],
             {"approve_a2p2_tools": True}, "model must be one line of text that does not start with '-'"),
            ([{"label": "c", "harness": "claude", "model": "haiku", "brief": "--permission-mode=bypassPermissions"}],
             {"approve_a2p2_tools": True, "prelude": "empty.md"}, "the prompt (prelude, then brief) starts with '-'"),
        ]
        # Each of these let Claude Code run any command: names joined into
        # one --allowedTools value, or a command that runs its arguments.
        for tool in ("Read,Bash", "Read Bash", "Bash(sh:*)", "Bash(bash -c:*)", "Bash(/bin/sh:*)",
                     "Bash(python3:*)", "Bash(*:*)", "Bash(env:*)", "Bash(xargs:*)", "Bash(git:*)",
                     "Bash(git -c core.pager=sh:*)", "Bash()", "bash", "Bash(sh)", "Read\nBash",
                     # "git *" is "git:*"; file names match without case on macOS
                     "Bash(git *)", "Bash(git * status)", "Bash( git *)", "Bash(git:* status)", "Bash(GIT:*)",
                     "Bash(Git -c a=b:*)", "Bash(Python3.11:*)", "Bash(/usr/bin/PYTHON3:*)",
                     # a trailing newline, a C1 control or a Unicode separator in an entry
                     "Read\n", "Bash(git status\x85:*)", "Bash(git status\u2028Bash)", "Read\u200b",
                     # "dangerous" stays refused in a tool's name
                     "dangerouslySkipPermissions", "DangerousTool(x)"):
            cases.append(([{"label": "c", "harness": "claude", "model": "haiku", "allowed_tools": [tool]}],
                          {"approve_a2p2_tools": True}, "skip Claude Code's permission checks"))
        open(os.path.join(self.dir, "empty.md"), "w").close()
        for members, extra, want in cases:
            with self.subTest(want=want, members=members):
                self.write(members, **extra)
                self.assertIn(want, self.team("up", ok=False).stderr)
        self.assertFalse(os.path.exists(self.home), "a refused manifest must not start a daemon")
        self.assertFalse(os.path.exists(self.state), "a refused manifest must not reserve the state")


class TeamSecurityTest(TeamCase):
    """Another user of this machine, a manifest written by mistake, and text
    a member wrote reaching the operator's terminal."""

    def shared(self, name, mode=0o777):
        d = os.path.join(self.dir, name)
        os.makedirs(d)
        os.chmod(d, mode)
        return d

    def test_up_refuses_a_state_dir_another_user_could_replace(self):
        # A directory that every user may write, on the way to the state
        # directory: another user could rename the state directory and put
        # their own launch.json in its place before the supervisor reads it.
        shared = self.shared("shared")
        os.symlink(shared, os.path.join(self.dir, "link"))
        for state in (os.path.join(shared, "state"), os.path.join(self.dir, "link", "state")):
            with self.subTest(state=state):
                self.state = state
                self.write([fake("okay", "ok")])
                err = self.team("up", ok=False).stderr
                self.assertIn("can be changed by other users", err)
                self.assertFalse(os.path.exists(state), "a refused state_dir must not be created")
        self.assertFalse(os.path.exists(self.home), "a refused state_dir must not start a daemon")

    def test_up_refuses_an_existing_state_dir_others_could_write_and_writes_nothing_through_it(self):
        # Another user planted a symlink at supervisor.log while the
        # directory was open to them; making it 0700 now would not undo that.
        self.state = self.shared("state")
        victim = os.path.join(self.dir, "victim")
        with open(victim, "w") as f:
            f.write("keep")
        os.symlink(victim, os.path.join(self.state, "supervisor.log"))
        self.write([fake("okay", "ok")])
        self.assertIn("can be changed by other users", self.team("up", ok=False).stderr)
        with open(victim) as f:
            self.assertEqual(f.read(), "keep")
        self.assertEqual(sorted(os.listdir(self.state)), ["supervisor.log"])
        self.assertEqual(os.stat(self.state).st_mode & 0o7777, 0o777, "a refused state_dir must keep its mode")
        self.assertFalse(os.path.exists(self.home))

    def test_up_refuses_an_a2p2_home_another_user_could_replace(self):
        self.home = os.path.join(self.shared("shared"), "home")
        self.write([fake("okay", "ok")])
        self.assertIn("a2p2_home", self.team("up", ok=False).stderr)
        self.assertFalse(os.path.exists(self.home))
        self.assertFalse(os.path.exists(self.state), "the home is checked before the state is reserved")

    def finished_run_that_reruns_into(self, sentinel):
        """A run that ended, set back to pending, whose saved command line
        makes the file sentinel: what another user who could swap the
        state directory would plant."""
        self.write([fake("okay", "ok")])
        self.team("up", "--wait")
        spec_path = os.path.join(self.state, "members", "okay", "launch.json")
        with open(spec_path) as f:
            spec = json.load(f)
        spec["argv"] = [sys.executable, "-c", f"open({sentinel!r}, 'w').close()"]
        os.remove(spec_path)
        with open(spec_path, "w") as f:
            json.dump(spec, f)
        r = self.roster_raw()
        for x in r["members"]:
            x.update(state="pending", pid=None, launched=None, connected=None, ended=None, exit=None,
                     wall_clock=None, stop_requested=None)
        r["ended"] = None
        os.remove(os.path.join(self.state, "roster.json"))
        with open(os.path.join(self.state, "roster.json"), "w") as f:
            json.dump(r, f)

    def test_supervisor_refuses_a_state_dir_another_user_could_replace(self):
        # A populated state directory with a saved command line: one that
        # others could write (sticky or not), or under a directory they
        # could write, is refused and nothing in it runs.
        sentinel = os.path.join(self.dir, "ran")
        self.finished_run_that_reruns_into(sentinel)
        for target, mode in ((self.state, 0o1777), (self.state, 0o777), (self.dir, 0o777)):
            with self.subTest(target=target, mode=oct(mode)):
                before = os.stat(target).st_mode & 0o7777
                os.chmod(target, mode)
                try:
                    r = self.team("_supervise", self.state, ok=False)
                finally:
                    os.chmod(target, before)
                self.assertEqual(r.returncode, 1, r.stdout)
                self.assertIn("can be changed by other users", r.stderr)
                self.assertFalse(os.path.exists(sentinel), "the supervisor ran a saved command line")
        # The same state directory, private, does run it: the fixture is sound.
        self.team("_supervise", self.state)
        self.assertTrue(os.path.exists(sentinel))

    def test_the_supervisor_runs_the_manifest_up_checked_not_the_file_read_again(self):
        # The manifest is changed while `up` runs, after its checks: the
        # supervisor must not run the a2p2 binary the new one names.
        evil_calls = os.path.join(self.dir, "evil-calls")
        evil = os.path.join(self.dir, "a2p2-evil")
        with open(evil, "w") as f:
            f.write(f'#!/bin/sh\necho "$1" >> "{evil_calls}"\nexec "{BIN}" "$@"\n')
        evil_manifest = os.path.join(self.dir, "evil.json")
        self.bin = evil
        self.write([fake("okay", "ok")])
        os.rename(self.manifest, evil_manifest)
        swap = os.path.join(self.dir, "a2p2-swap")
        with open(swap, "w") as f:
            f.write(f'#!/bin/sh\n[ "$1" = send ] && cp "{evil_manifest}" "{self.manifest}"\nexec "{BIN}" "$@"\n')
        for p in (evil, swap):
            os.chmod(p, 0o755)
        self.bin = swap
        self.write([fake("okay", "ok")])
        self.team("up", "--wait")
        self.assertEqual(self.roster()["okay"]["state"], "done")
        self.assertFalse(os.path.exists(evil_calls), "the supervisor ran the a2p2 of the changed manifest")

    def test_commands_refuse_a_state_dir_others_could_write_and_write_nothing_through_it(self):
        self.write([fake("okay", "ok")])
        self.team("up", "--wait")
        victim = os.path.join(self.dir, "victim")
        with open(victim, "w") as f:
            f.write("keep")
        report = os.path.join(self.state, "report.json")
        # In a private state directory a symlink at a report is replaced,
        # never written through.
        os.remove(report)
        os.symlink(victim, report)
        self.team("report")
        self.assertFalse(os.path.islink(report))
        with open(victim) as f:
            self.assertEqual(f.read(), "keep")
        # One that others could write is refused by every command.
        os.remove(report)
        os.symlink(victim, report)
        listing = sorted(os.listdir(self.state))
        os.chmod(self.state, 0o777)
        try:
            for args in (("report",), ("status",), ("stop",), ("logs", "okay")):
                with self.subTest(args=args):
                    r = self.team(*args, ok=False)
                    self.assertEqual(r.returncode, 1, r.stdout)
                    self.assertIn("can be changed by other users", r.stderr)
                    self.assertEqual(r.stdout, "")
        finally:
            os.chmod(self.state, 0o700)
        with open(victim) as f:
            self.assertEqual(f.read(), "keep")
        self.assertEqual(sorted(os.listdir(self.state)), listing)

    def test_up_works_under_umask_002_with_directories_it_creates(self):
        # Many Linux systems give each user a private group and umask 002.
        # os.makedirs then made the directories above the last 0775, and
        # the check after it refused the state directory `up` had made.
        proj, proj2 = os.path.join(self.dir, "proj"), os.path.join(self.dir, "proj2")
        for d in (proj, proj2):
            os.makedirs(d)
            os.chmod(d, 0o755)
        self.state = os.path.join(proj, ".team", "team")
        self.home = os.path.join(proj2, "sub", "home")
        self.write([fake("okay", "ok")])
        old = os.umask(0o002)
        try:
            self.team("up", "--wait")
        finally:
            os.umask(old)
        self.assertEqual(self.roster()["okay"]["state"], "done")
        for d in (os.path.join(proj, ".team"), self.state, os.path.join(proj2, "sub"), self.home):
            self.assertEqual(os.stat(d).st_mode & 0o777, 0o700, d)

    def test_up_under_umask_002_in_a_project_directory_of_the_users_own_group(self):
        # mkdir and git clone under umask 002 make the project 0775. With a
        # group that is only this user, as many Linux systems give, `up`
        # works with the state next to the manifest; with a shared group,
        # such as staff on macOS, it is refused and says how to fix it.
        old = os.umask(0o002)
        try:
            proj = os.path.join(self.dir, "proj")
            os.mkdir(proj)
            self.assertEqual(os.stat(proj).st_mode & 0o777, 0o775)
            self.state = os.path.join(proj, ".team", "team")
            self.write([fake("okay", "ok")])
            if load_team().private_group(os.stat(proj).st_gid):
                self.team("up", "--wait")
                self.assertEqual(self.roster()["okay"]["state"], "done")
            else:
                err = self.team("up", ok=False).stderr
                self.assertIn("through its group", err)
                self.assertIn("chmod g-w", err)
                self.assertFalse(os.path.exists(self.state))
        finally:
            os.umask(old)

    def test_a_group_writable_directory_is_private_only_when_the_group_is_this_user_alone(self):
        # The same as above, whatever groups the test machine has: the
        # group database is replaced by one of each case.
        import types
        from unittest import mock
        team = load_team()
        proj = os.path.join(self.dir, "proj")
        os.mkdir(proj)
        os.chmod(proj, 0o775)
        gid, uid = os.stat(proj).st_gid, os.getuid()
        me = types.SimpleNamespace(pw_name="me", pw_uid=uid, pw_gid=gid)
        other = types.SimpleNamespace(pw_name="other", pw_uid=uid + 1, pw_gid=gid + 1)
        cases = [
            ("own group", gid, [], [me, other], None),
            ("own group, listed as its member", gid, ["me"], [me, other], None),
            ("another member", gid, ["me", "other"], [me, other], "through its group"),
            ("another account's primary group", gid,
             [], [me, types.SimpleNamespace(pw_name="other", pw_uid=uid + 1, pw_gid=gid)], "through its group"),
            ("not this user's primary group", gid + 1, [], [me, other], "through its group"),
        ]
        for name, mygid, mem, accounts, refused in cases:
            with self.subTest(case=name):
                fake_pwd = types.SimpleNamespace(getpwall=lambda a=accounts: a,
                                                 getpwuid=lambda u: me)
                fake_grp = types.SimpleNamespace(getgrgid=lambda g, m=mem: types.SimpleNamespace(gr_mem=m))
                state = os.path.join(proj, name.replace(" ", "-").replace(",", "").replace("'", ""), "state")
                with mock.patch.object(team, "pwd", fake_pwd), mock.patch.object(team, "grp", fake_grp), \
                        mock.patch.object(team.os, "getgid", lambda g=mygid: g):
                    if refused is None:
                        team.reserve({"state": state})
                        self.assertEqual(os.stat(state).st_mode & 0o777, 0o700)
                    else:
                        with self.assertRaises(team.Fail) as cm:
                            team.reserve({"state": state})
                        self.assertIn(refused, str(cm.exception))
                        self.assertIn("chmod g-w", str(cm.exception))
                        self.assertFalse(os.path.exists(state))
        # Others' write bit is never excused by the group.
        os.chmod(proj, 0o777)
        with mock.patch.object(team, "pwd", types.SimpleNamespace(getpwall=lambda: [me], getpwuid=lambda u: me)), \
                mock.patch.object(team, "grp", types.SimpleNamespace(getgrgid=lambda g: types.SimpleNamespace(gr_mem=[]))), \
                mock.patch.object(team.os, "getgid", lambda: gid):
            with self.assertRaises(team.Fail) as cm:
                team.reserve({"state": os.path.join(proj, "open", "state")})
        self.assertIn("chmod o-w", str(cm.exception))

    def test_up_checks_the_home_again_after_making_it(self):
        # The home's parent is missing at the first check; another user
        # makes it, open to all, in a sticky directory before `up` makes
        # the home. No a2p2 command, which would start a daemon there, runs.
        import argparse
        team = load_team()
        sticky = self.shared("sticky", 0o1777)
        parent = os.path.join(sticky, "newparent")
        self.home = os.path.join(parent, "home")
        self.write([fake("okay", "ok")])
        man = team.load_manifest(self.manifest)
        prepare, calls = team.prepare_dirs, []

        def racing(man_, m):
            if not os.path.exists(parent):
                os.mkdir(parent)
                os.chmod(parent, 0o777)
            return prepare(man_, m)

        def no_a2p2(_man, *args, **_kw):
            calls.append(args)
            raise team.Fail("a2p2 was called")

        team.prepare_dirs, team.a2p2 = racing, no_a2p2
        with self.assertRaises(team.Fail) as cm:
            team.cmd_up(man, argparse.Namespace(wait=False))
        self.assertIn("a2p2_home", str(cm.exception))
        self.assertIn(parent, str(cm.exception))
        self.assertEqual(calls, [])

    def test_a_state_dir_in_a_sticky_shared_dir_like_tmp_still_works(self):
        # /tmp is writable by everyone but sticky: only the owner of an
        # entry may rename it. An existing state directory of this user
        # that only it can write is used and made 0700.
        self.state = os.path.join(self.shared("sticky", 0o1777), "state")
        os.makedirs(self.state)
        os.chmod(self.state, 0o755)
        self.write([fake("okay", "ok")])
        self.team("up", "--wait")
        self.assertEqual(self.roster()["okay"]["state"], "done")
        self.assertEqual(os.stat(self.state).st_mode & 0o777, 0o700)

    def test_shared_path_problem_follows_symlinks_and_names_the_directory(self):
        team = load_team()
        shared = self.shared("shared")
        sticky = self.shared("sticky", 0o1777)
        private = os.path.join(self.dir, "private")
        os.makedirs(private)
        os.symlink(shared, os.path.join(private, "to-shared"))
        os.symlink(os.path.join("..", "private"), os.path.join(sticky, "to-private"))
        self.assertIsNone(team.shared_path_problem(os.path.join(private, "state")))
        self.assertIsNone(team.shared_path_problem(os.path.join(sticky, "state")))
        self.assertIsNone(team.shared_path_problem(os.path.join(sticky, "to-private", "state")))
        self.assertIn(shared, team.shared_path_problem(os.path.join(private, "to-shared", "state")))
        self.assertIn(shared, team.shared_path_problem(os.path.join(shared, "a", "b")))

    def test_scoped_tools_and_a_brief_that_starts_with_a_list_are_accepted(self):
        team = load_team()
        tools = ["Read", "Grep", "Bash(git status:*)", "Bash(git diff:*)", "Bash(npm run test:*)",
                 "Bash(go test:*)", "Read(./src/**)", "WebFetch(domain:example.com)", "mcp__other-server__tool",
                 # "dangerous" in a rule is a path, not a way past the checks
                 "Read(./notes/dangerous-ideas.md)"]
        self.write([{"label": "c", "harness": "claude", "model": "claude-haiku-4-5", "allowed_tools": tools,
                     "brief": "- read the kickoff\n- reply"}], approve_a2p2_tools=True)
        man = team.load_manifest(self.manifest)
        self.assertEqual(man["members"][0]["allowed_tools"], tools)
        team.require_safe_prompts(man)

    def test_the_prompt_launched_is_the_prompt_checked(self):
        # The prelude is read once: a change to the file after the check
        # does not reach the prompt. And the prompt with the real lead and
        # kickoff ids is checked again as it is saved for launch.
        team = load_team()
        prelude = os.path.join(self.dir, "prelude.md")
        with open(prelude, "w") as f:
            f.write("{kickoff} is the kickoff.\n")
        self.write([{"label": "c", "harness": "claude", "model": "haiku"}], approve_a2p2_tools=True, prelude=prelude)
        man = team.load_manifest(self.manifest)
        team.read_prelude(man)
        team.require_safe_prompts(man)
        with open(prelude, "w") as f:
            f.write("--permission-mode=bypassPermissions\n")
        m = man["members"][0]
        self.assertTrue(team.prompt_for(man, m, "k1", "lead@n").startswith("k1 is the kickoff."))
        os.makedirs(team.mdir(man, "c"))
        with self.assertRaises(team.Fail) as cm:
            team.launch_spec(man, m, team.prompt_for(man, m, "--dangerously-skip-permissions", "lead@n"), self.dir)
        self.assertIn("starts with '-'", str(cm.exception))
        self.assertFalse(os.path.exists(os.path.join(team.mdir(man, "c"), "launch.json")))

    def test_a_kickoff_that_starts_with_a_dash_is_posted_as_its_text(self):
        # Before, the kickoff went to `a2p2 send` where a flag may stand,
        # so "--as=human..." was read as a flag and `up` failed half way.
        self.write([fake("okay", "ok")], kickoff="--as=human: posted as text")
        self.team("up", "--wait")
        out = subprocess.run([BIN, "history", "--room", "crew", "-n", "200", "--json"],
                             env=dict(os.environ, A2P2_HOME=self.home), capture_output=True, text=True).stdout
        lead = [e for e in json.loads(out)["entries"] if e["from"].startswith("lead@")]
        self.assertEqual(len(lead), 1)
        self.assertTrue(lead[0]["text"].startswith("--as=human: posted as text\n"), lead[0]["text"])

    def test_status_and_logs_escape_terminal_controls_a_member_wrote(self):
        self.write([fake("loud", "controls")])
        self.team("up", "--wait")
        status = self.team("status").stdout
        self.assertIn("\\u{1b}[31m", status)
        self.assertIn("\\u{9b}31m", status)
        self.assertIn("\\u{202e}evil", status)
        logs = subprocess.run([sys.executable, TEAM, "-m", self.manifest, "logs", "loud"],
                              capture_output=True, timeout=60, env=self.env)
        self.assertEqual(logs.returncode, 0, logs.stderr)
        raw = subprocess.run([sys.executable, TEAM, "-m", self.manifest, "logs", "loud", "--raw"],
                             capture_output=True, timeout=60, env=self.env).stdout
        for out in (status.encode(), logs.stdout):
            for c in (b"\x1b", "\u009b".encode(), "‮".encode(), b"\xff"):
                self.assertNotIn(c, out)
        self.assertIn(b"\\u{202e}evil \\x{ff} end\n", logs.stdout)
        self.assertIn(b"\x1b[31m", raw)
        self.assertIn("‮evil".encode() + b" \xff end\n", raw)
        self.assertIn(b"whoami ->", logs.stdout)

    def test_the_escape_covers_the_daemons_table_and_keeps_plain_text(self):
        # The daemon's table is the reference (internal/node/escape.go,
        # decision D58); the launcher may escape more, never less.
        import re
        team = load_team()
        with open(os.path.join(ROOT, "internal", "node", "escape.go")) as f:
            src = f.read()
        table = src[src.index("var hiddenRanges"):]
        table = table[:table.index("\n}\n")]
        ranges = [(int(a, 16), int(b, 16)) for a, b in re.findall(r"\{0x([0-9a-f]+), 0x([0-9a-f]+)\}", table)]
        self.assertGreater(len(ranges), 20)
        missed = [hex(cp) for a, b in ranges for cp in range(a, b + 1) if team.escape(chr(cp)) == chr(cp)]
        self.assertEqual(missed, [])
        for plain in ("plan: say hello", "DONE: café, 日本語, עברית, 👍", "tab\there"):
            self.assertEqual(team.escape(plain, lines=True), plain)
        self.assertEqual(team.escape("a\\u{41}"), "a\\u{5c}u{41}")

    def test_the_escape_is_the_daemons_with_its_tables_and_its_cases(self):
        # D58: one set of rules. The launcher's tables equal escape.go's,
        # and it gives what EscapeDisplay (line) and EscapeDisplayText
        # (text) give for every row of the cases the page shares too.
        import re
        team = load_team()
        with open(os.path.join(ROOT, "internal", "node", "escape.go")) as f:
            src = f.read()
        for go, py in (("hiddenRanges", team.HIDDEN), ("emojiRanges", team.EMOJI),
                       ("joiningRanges", team.JOINING), ("rtlLetterRanges", team.RTL_LETTERS),
                       ("spacelessRanges", team.SPACELESS), ("hanRanges", team.HAN)):
            table = src[src.index(f"var {go} = "):]
            table = table[:table.index("\n}\n")]
            got = [(int(a, 16), int(b, 16)) for a, b in re.findall(r"\{0x([0-9a-f]+), 0x([0-9a-f]+)\}", table)]
            self.assertEqual(list(py), got, go)
        with open(os.path.join(ROOT, "internal", "node", "testdata", "escape_cases.json")) as f:
            cases = json.load(f)
        n = 0
        for c in cases:
            for key, lines in (("line", False), ("text", True)):
                if key in c:
                    n += 1
                    self.assertEqual(team.escape(c["in"], lines=lines), c[key], f"{c['name']} ({key})")
        self.assertGreater(n, 60)

    def test_status_and_logs_escape_like_the_daemon_and_drop_nothing(self):
        # Before, status stripped controls at either end of a message, the
        # Persian non-joiner was escaped, a lone carriage return became a
        # new line in logs, and the log's heading printed its path raw.
        import argparse
        import contextlib
        import io
        team = load_team()
        out = os.path.join(self.dir, "out\x1b[2J.log")
        with open(out, "wb") as f:
            f.write(b"plain\r\nover\rwrite\n" + "تیم‌ما\n".encode() + b"last")
        x = {"label": "m", "id": "m@node", "harness": "command", "pid": 1, "state": "running", "exit": None,
             "wall_clock": 1, "launched": 1, "out": out, "err": os.path.join(self.dir, "missing.log")}
        team.load_roster = lambda _man: {"members": [x], "ended": 1, "node": NODE}
        team.history = lambda _man: [{"from": "m@node", "from_node": NODE, "created": "2026-01-01T00:00:00Z",
                                      "text": "\x1c hi \u0085\nتیم‌ما"}]

        def run(fn, n=0):
            buf = io.BytesIO()
            w = io.TextIOWrapper(buf, encoding="utf-8", write_through=True)
            with contextlib.redirect_stdout(w):
                fn({"state": self.dir}, argparse.Namespace(label="m", n=n, raw=False))
            w.flush()
            return buf.getvalue().decode()

        status = run(team.cmd_status)
        self.assertIn("\\u{1c} hi \\u{85} تیم‌ما", status)
        logs = run(team.cmd_logs)
        self.assertIn("==> " + os.path.join(self.dir, "out\\u{1b}[2J.log") + " <==\n", logs)
        self.assertIn("plain\nover\\u{d}write\nتیم‌ما\nlast", logs)
        self.assertNotIn("\x1b", logs)
        self.assertNotIn("\r", logs)
        tail = run(team.cmd_logs, n=2)
        self.assertTrue(tail.endswith("<==\nتیم‌ما\nlast==> "
                                      + os.path.join(self.dir, "missing.log") + " <==\n"), tail)

    def test_logs_n_reads_the_end_of_a_log_not_all_of_it(self):
        import io
        team = load_team()

        class Counting(io.BytesIO):
            read_bytes = 0

            def read(self, size=-1):
                b = super().read(size)
                self.read_bytes += len(b)
                return b

        data = b"".join(b"line %d\n" % i for i in range(200000))
        for body, n in ((data, 1), (data, 3), (data, 900), (data + b"tail", 2), (b"a\nb\n", 5), (b"", 1), (b"x", 1)):
            with self.subTest(size=len(body), n=n):
                f = Counting(body)
                got = team.tail_lines(f, n, block=4096)
                lines = body.splitlines(keepends=True)   # no \r in these bodies
                self.assertEqual(got, lines[-n:])
                self.assertLessEqual(f.read_bytes, len(b"".join(lines[-n:])) + 2 * 4096)


NODE = "ab12cd34" + "0" * 56      # this machine
TWIN = "ab12cd34" + "f" * 56      # another machine with the same short id
OTHER = "99887766" + "1" * 56     # another machine


def entry(eid, label, node, shown, to=(), age=0, text="hi", parent=None):
    """A history entry as `a2p2 history --json` gives it: "from" with the
    id as shown (short, or full when shared), "from_node" the full key."""
    return {"id": eid, "kind": "message", "from": f"{label}@{shown}", "from_node": node,
            "from_this_machine": node == NODE, "to": list(to), "parent": parent, "text": text,
            "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - age))}


class TeamIdentityTest(TeamCase):
    """The launcher tells this machine's members and person by full key and
    exact id, not by the short id another machine can share (D65)."""

    def observe(self, hist, st, silence=30, connected_ago=1):
        """One observer poll over hist with status st; the posts it made."""
        import types
        team = load_team()
        posts = []
        x = {"label": "okay", "id": "okay@ab12cd34", "harness": "command", "server_harness": "fake",
             "model": "", "state": "running", "launched": time.time() - 200,
             "connected": time.time() - connected_ago, "silence": silence, "workdir": self.dir}
        sup = types.SimpleNamespace(man={"poll": 1}, roster={"node": NODE, "members": [x]},
                                    lock=team.threading.Lock(), save=lambda: None)
        team.post = lambda _man, text: posts.append(text)
        team.a2p2_json = lambda _man, *args: st
        team.history = lambda _man, **_kw: hist
        team.Observer(sup).poll()
        return posts

    def test_a_twin_machines_question_to_its_person_is_not_this_members(self):
        # Another machine with the same short id posts as "okay" to its own
        # person; this machine's member, written in full since the ids
        # collide, asks this machine's person and another machine's person.
        st = {"node": NODE, "human": "human@" + NODE, "agents": []}
        hist = [entry("e-twin", "okay", TWIN, TWIN, to=["human@" + TWIN]),
                entry("e-mine", "okay", NODE, NODE, to=["human@" + NODE]),
                entry("e-theirs", "okay", NODE, NODE, to=["human@" + OTHER[:8]])]
        posts = self.observe(hist, st)
        self.assertEqual([p for p in posts if p.startswith("needs input")],
                         ["needs input: okay asked the person a question (message e-mine)."])

    def test_a_twin_machines_old_style_short_ids_are_not_this_members(self):
        # A daemon that does not write shared ids in full (0.2.3) shows the
        # twin's member under this machine's short id: from_node tells them
        # apart, so its question and its activity are not this member's.
        st = {"node": NODE, "human": "human@ab12cd34", "agents": []}
        hist = [entry("e-twin", "okay", TWIN, "ab12cd34", to=["human@ab12cd34"])]
        posts = self.observe(hist, st, silence=10, connected_ago=100)
        self.assertFalse([p for p in posts if p.startswith("needs input")], posts)
        self.assertEqual(len([p for p in posts if p.startswith("silent: okay")]), 1, posts)

    def test_this_members_activity_written_with_its_full_id_counts(self):
        # Once a twin appears, this machine's member is written okay@<full
        # key>: it is still this member's, so it is not called silent.
        st = {"node": NODE, "human": "human@" + NODE, "agents": []}
        posts = self.observe([entry("e1", "okay", NODE, NODE)], st, silence=10, connected_ago=100)
        self.assertFalse([p for p in posts if p.startswith("silent")], posts)

    def test_honest_short_ids_still_work(self):
        st = {"node": NODE, "human": "human@ab12cd34", "agents": []}
        posts = self.observe([entry("e1", "okay", NODE, "ab12cd34", to=["human@ab12cd34"])], st,
                             silence=10, connected_ago=100)
        self.assertEqual(posts, ["needs input: okay asked the person a question (message e1)."])
        team = load_team()
        self.assertEqual(team.shown_id({"node": NODE, "human": "human@ab12cd34"}), "ab12cd34")
        self.assertEqual(team.shown_id({"node": NODE, "human": "human@" + NODE}), NODE)

    def test_report_and_status_count_this_machines_messages_only(self):
        import argparse
        import contextlib
        import io
        team = load_team()
        x = {"label": "okay", "id": "okay@ab12cd34", "harness": "command", "model": "", "state": "done",
             "exit": 0, "wall_clock": 1, "launched": time.time() - 100, "connected": time.time() - 99,
             "pid": 1, "final": os.path.join(self.dir, "none"), "out": os.path.join(self.dir, "none"),
             "err": os.path.join(self.dir, "none")}
        roster = {"room": "crew", "kickoff": "k", "lead": "lead@ab12cd34", "watcher": "team@ab12cd34",
                  "node": NODE, "started": time.time() - 100, "ended": time.time(), "members": [x]}
        hist = [entry("t1", "okay", TWIN, "ab12cd34", to=["human@ab12cd34"], text="twin says"),
                entry("t2", "human", TWIN, "ab12cd34", text="twin person"),
                entry("t3", "lead", TWIN, "ab12cd34", text="twin lead"),
                entry("m1", "okay", NODE, NODE, to=["human@" + NODE], text="DONE: mine", age=5),
                entry("m2", "human", NODE, NODE, text="my person", age=5),
                entry("m3", "lead", NODE, NODE, text="my lead", age=5)]
        team.load_roster = lambda _man: roster
        team.history = lambda _man, **_kw: hist
        team.a2p2_json = lambda _man, *args: {"node": NODE, "human": "human@" + NODE, "rooms": []}
        rep = team.build_report({"state": self.dir})
        m = rep["members"][0]
        self.assertEqual((m["messages_sent"], m["questions_to_person"], m["final_report"]), (1, 1, "DONE"))
        self.assertEqual((rep["messages_from_person"], rep["messages_from_lead"]), (1, 1))
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            team.cmd_status({"state": self.dir}, argparse.Namespace())
        self.assertIn("DONE: mine", buf.getvalue())
        self.assertNotIn("twin says", buf.getvalue())

    def test_report_counts_only_this_machines_person_lead_watcher_and_inboxes(self):
        team = load_team()
        x = {"label": "okay", "id": "okay@ab12cd34", "harness": "command", "model": "", "state": "done",
             "exit": 0, "wall_clock": 1, "launched": time.time() - 100, "connected": time.time() - 99,
             "pid": 1, "final": os.path.join(self.dir, "none"), "out": os.path.join(self.dir, "none"),
             "err": os.path.join(self.dir, "none")}
        roster = {"room": "crew", "kickoff": "k", "lead": "lead@ab12cd34", "watcher": "team@ab12cd34",
                  "node": NODE, "started": time.time() - 100, "ended": time.time(), "members": [x]}
        hist = [
            # This member asks the person of another machine and of the twin:
            # neither is this machine's person.
            entry("q1", "okay", NODE, NODE, to=["human@99887766"], age=5),
            entry("q2", "okay", NODE, NODE, to=["human@" + OTHER], age=5),
            entry("q3", "okay", NODE, NODE, to=["human@" + TWIN], age=5),
            entry("q4", "okay", NODE, NODE, to=["human@" + NODE], age=5),
            # A reply that names this member by its full id and one other
            # inbox reaches one inbox; a reply to two others reaches two.
            entry("r1", "okay", NODE, NODE, to=["okay@" + NODE, "lead@" + NODE], parent="k", age=5),
            entry("r2", "okay", NODE, NODE, to=["lead@" + NODE, "okay@" + TWIN], parent="k", age=5),
            # The twin's lead and watcher, written with the short id the
            # roster holds; this machine's, written in full.
            entry("t1", "lead", TWIN, "ab12cd34"),
            entry("t2", "lead", TWIN, "ab12cd34"),
            entry("t3", "team", TWIN, "ab12cd34"),
            entry("t4", "team", TWIN, "ab12cd34"),
            entry("m1", "lead", NODE, NODE, age=5),
            entry("m2", "team", NODE, NODE, age=5)]
        team.load_roster = lambda _man: roster
        team.history = lambda _man, **_kw: hist
        team.a2p2_json = lambda _man, *args: {"node": NODE, "human": "human@" + NODE, "rooms": []}
        rep = team.build_report({"state": self.dir})
        m = rep["members"][0]
        self.assertEqual((m["messages_sent"], m["questions_to_person"], m["replies_to_more_than_one_inbox"]),
                         (6, 1, 1))
        self.assertEqual((rep["messages_from_lead"], rep["watcher_messages"]), (1, 1))


class TeamStopTest(TeamCase):
    """`team stop` says what happened and exits non-zero when it did not."""

    def run_stop(self, members, answer=None, label=None):
        """cmd_stop on a roster of members; answer(roster, request) plays
        the supervisor, or none answers. Returns (stdout, Fail or None)."""
        import argparse
        import contextlib
        import io
        import threading
        team = load_team()
        team.ACK_WAIT, team.STOP_WAIT = 0.5, 0.5
        man = {"state": self.state}
        os.makedirs(team.control_dir(man))
        team.save_roster(man, {"ended": None, "members": members})
        stop = threading.Event()

        def supervisor():
            while not stop.is_set():
                for name in os.listdir(team.control_dir(man)):
                    if name.startswith("req-") and name.endswith(".json"):
                        path = os.path.join(team.control_dir(man), name)
                        with open(path) as f:
                            req = json.load(f)
                        os.remove(path)
                        hit = answer(man, req)
                        with open(os.path.join(team.control_dir(man), "ack-" + name[4:]), "w") as f:
                            json.dump({"stopping": hit}, f)
                        return
                time.sleep(0.02)

        t = threading.Thread(target=supervisor) if answer else None
        if t:
            t.start()
        buf, err = io.StringIO(), None
        try:
            with contextlib.redirect_stdout(buf):
                team.cmd_stop(man, argparse.Namespace(label=label))
        except team.Fail as e:
            err = e
        finally:
            stop.set()
            if t:
                t.join()
        self.after = team.load_roster(man)
        self.requests = [n for n in os.listdir(team.control_dir(man)) if n.startswith("req-")]
        return buf.getvalue(), err

    def test_a_stop_whose_member_is_still_running_after_its_wait_fails(self):
        out, err = self.run_stop([{"label": "stuck", "state": "stopping"}], answer=lambda man, req: ["stuck"])
        self.assertIsNotNone(err, out)
        self.assertIn("still running after", str(err))
        self.assertNotIn("stopped 1", out)

    def test_a_stop_whose_member_ends_reports_it(self):
        team = load_team()

        def answer(man, req):
            team.save_roster(man, {"ended": None, "members": [{"label": "stuck", "state": "stopped"}]})
            return ["stuck"]
        out, err = self.run_stop([{"label": "stuck", "state": "running"}], answer=answer)
        self.assertIsNone(err)
        self.assertIn("stopped 1 member(s): stuck", out)

    def test_an_unanswered_stop_fails_and_leaves_the_roster_as_it_was(self):
        # A supervisor that is busy, such as writing the report, or dead:
        # the roster is not marked ended or lost, and stop exits non-zero.
        members = [{"label": "stuck", "state": "running"}]
        out, err = self.run_stop(members)
        self.assertIsNotNone(err, out)
        self.assertIn("no supervisor answered", str(err))
        self.assertIn("stuck", str(err))
        self.assertEqual(self.after, {"ended": None, "members": members})
        self.assertEqual(self.requests, [], "an unanswered request was left behind")

    def test_a_second_stop_says_no_member_is_running(self):
        members = [{"label": "stuck", "state": "stopped"}, {"label": "okay", "state": "done"}]
        out, err = self.run_stop(members)
        self.assertIsNone(err)
        self.assertIn("no member to stop: stuck is stopped, okay is done", out)
        self.assertEqual(self.after, {"ended": None, "members": members})
        self.assertEqual(self.requests, [])


def main():
    prog = unittest.main(verbosity=2, exit=False)
    left = []
    deadline = time.time() + 15   # a stopped daemon needs a moment to exit
    while time.time() < deadline:
        left = leftovers()
        if not left:
            break
        time.sleep(0.3)
    for line in left:
        print(f"LEFTOVER from this run: {line}", file=sys.stderr)
    return 0 if prog.result.wasSuccessful() and not left else 1


if __name__ == "__main__":
    sys.exit(main())
