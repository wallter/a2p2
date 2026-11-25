#!/usr/bin/env python3
"""Chaos run of real a2p2 daemons in Docker containers (requirements D1 to
D6, D9, N1). Python 3 standard library only.

    chaos.py run   [--nodes 3] [--duration 90] [--seed 1] [--settle 420]
                   [--out DIR] [--sabotage isolate] [--keep]
    chaos.py check DIR [--drop NODE:AUTHOR] [--corrupt] [--unverify KIND] [--empty]

`run` builds the static Linux binary into an image tagged for this run,
starts one daemon per container (its own home volume, one user-defined
network, the last node outbound only), forms one room by real invites and
confirmations answered at a pseudo-terminal inside the containers, runs a
workload on every node while a fault schedule made from the seed runs
(kill -9 and restart, pause, network disconnect, a partition into two
halves with a kill inside it), heals everything, waits for the room to
settle, and checks what every member holds and was shown.

Every Docker object a run makes carries a name with a random run id and
the label a2p2chaos.run=<run id>; a name already in use fails the run
instead of being reused. Cleanup removes only the objects Docker returned
to this run and those carrying its label, also on SIGINT and SIGTERM, and
says what it could not remove. --keep leaves them and prints them.

Exit status: 0 every check passed; 1 a check failed (a violation); 2 the
run could not be carried out, was interrupted, or its cleanup failed; 3
inconclusive (a fault, a member's inbox or a behaviour the checks rely on
was not shown to have happened).

`check DIR` evaluates the snapshot a run left in DIR again. To show that
it fails when it should: --drop NODE:AUTHOR deletes AUTHOR's records from
NODE's copy; --corrupt changes one message's text the same way on every
member; --unverify KIND marks every fault of that kind as not applied;
--empty removes every send, read and record (a run in which nothing got
through).
"""

import argparse
import collections
import datetime
import json
import os
import random
import re
import secrets
import shlex
import shutil
import signal
import subprocess
import sys
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HERE = os.path.dirname(os.path.abspath(__file__))
PREFIX = "a2p2chaos"
LABEL = "a2p2chaos.run"
ROOM = "chaos"
AGENT = "w"
TEXT_RE = re.compile(r"^([maorsf]) (n\d+) (\d+)(?: (.*))?$")
FAULTS = ("kill", "start", "pause", "unpause", "disconnect", "connect", "partition", "heal")
SCRUB_WAIT = 75  # seconds: the daemon scrubs its store every minute (DECISIONS D43)
T0 = time.monotonic()

# --------------------------------------------------------------------------
# Small helpers


def now():
    """Seconds since the driver started; every recorded time uses it."""
    return time.monotonic() - T0


class Log:
    """Timestamped lines to the run's log file and to stderr."""

    def __init__(self, path):
        self.f = open(path, "a", buffering=1)
        self.mu = threading.Lock()

    def __call__(self, *parts):
        line = "%7.1fs %s" % (now(), " ".join(str(p) for p in parts))
        with self.mu:
            self.f.write(line + "\n")
            print(line, file=sys.stderr, flush=True)


def sh(args, timeout=120, check=False, input=None):
    """Run a host command; returns (rc, stdout, stderr)."""
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=timeout, input=input)
    except subprocess.TimeoutExpired:
        return 124, "", "timeout after %ss" % timeout
    if check and p.returncode != 0:
        raise RuntimeError("%s: %s" % (" ".join(args), p.stderr.strip() or p.stdout.strip()))
    return p.returncode, p.stdout, p.stderr


def last_json(out):
    """The last line of out that is a JSON object (pty output mixes prompts in)."""
    for line in reversed(out.replace("\r", "").splitlines()):
        line = line.strip()
        i = line.find("{")
        if i >= 0:
            try:
                return json.loads(line[i:])
            except ValueError:
                continue
    return None


def iso_seconds(s):
    """An ISO time such as 2026-11-24T15:01:27.635Z as Unix seconds."""
    return datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%S.%fZ").replace(
        tzinfo=datetime.timezone.utc).timestamp()


class Interrupted(Exception):
    pass


# --------------------------------------------------------------------------
# What this run owns in Docker


class Owned:
    """The Docker objects this run created, by the ids Docker returned
    (volumes by name, which is their id), and their removal."""

    def __init__(self, run_id, log):
        self.run_id, self.log = run_id, log
        self.label = "%s=%s" % (LABEL, run_id)
        self.containers, self.volumes, self.networks, self.images = [], [], [], []

    def name(self, what):
        return "%s-%s-%s" % (PREFIX, self.run_id, what)

    def _absent(self, kind, name):
        rc, _, _ = sh(["docker", kind, "inspect", name])
        if rc == 0:
            raise RuntimeError("a %s named %s already exists; this run does not reuse it" % (kind, name))

    def image(self, ctx):
        tag = "%s:%s" % (PREFIX, self.run_id)
        self._absent("image", tag)
        rc, out, err = sh(["docker", "build", "-q", "--label", self.label, "-t", tag, ctx], timeout=600)
        if rc != 0:
            raise RuntimeError("docker build: " + err.strip())
        self.images.append(out.strip())
        return tag

    def network(self, subnet):
        name = self.name("net")
        self._absent("network", name)
        rc, out, err = sh(["docker", "network", "create", "--label", self.label, "--subnet", subnet, name])
        if rc != 0:
            return None, err
        self.networks.append(out.strip())
        return name, ""

    def volume(self, what):
        name = self.name(what)
        self._absent("volume", name)
        sh(["docker", "volume", "create", "--label", self.label, name], check=True)
        rc, out, _ = sh(["docker", "volume", "inspect", "-f", '{{index .Labels "%s"}}' % LABEL, name])
        if out.strip() != self.run_id:
            raise RuntimeError("volume %s does not carry this run's label; leaving it alone" % name)
        self.volumes.append(name)
        return name

    def container(self, args):
        rc, out, err = sh(["docker", "run", "-d", "--label", self.label] + args)
        if rc != 0:
            raise RuntimeError("docker run: " + err.strip())
        self.containers.append(out.strip())
        return out.strip()

    def describe(self):
        return {"label": self.label, "containers": self.containers, "volumes": self.volumes,
                "networks": self.networks, "images": self.images}

    def remove(self):
        """Removes what this run owns; returns the failures, as sentences."""
        failures = []

        def rm(args, what):
            rc, _, err = sh(args, timeout=120)
            if rc != 0 and "No such" not in err and "not found" not in err:
                failures.append("could not remove %s: %s" % (what, err.strip()[:200]))

        # By the label as well: a container docker run made before failing.
        listed = {}
        for kind, cmd in (("container", ["docker", "ps", "-aq", "--no-trunc"]),
                          ("volume", ["docker", "volume", "ls", "-q"]),
                          ("network", ["docker", "network", "ls", "-q", "--no-trunc"])):
            rc, out, _ = sh(cmd + ["--filter", "label=" + self.label])
            listed[kind] = out.split() if rc == 0 else []
        for c in dict.fromkeys(self.containers + listed["container"]):
            rm(["docker", "rm", "-f", c], "container " + c[:12])
        for v in dict.fromkeys(self.volumes + listed["volume"]):
            rm(["docker", "volume", "rm", v], "volume " + v)
        for n in dict.fromkeys(self.networks + listed["network"]):
            rm(["docker", "network", "rm", n], "network " + n[:12])
        for i in self.images:
            rm(["docker", "image", "rm", i], "image " + i[:19])
        rc, out, _ = sh(["docker", "images", "-q", "--filter", "label=" + self.label])
        for i in out.split():
            rm(["docker", "image", "rm", "-f", i], "image " + i)
        # What is left with the label is a failure even if every command passed.
        for kind, cmd in (("container", ["docker", "ps", "-aq"]), ("volume", ["docker", "volume", "ls", "-q"]),
                          ("network", ["docker", "network", "ls", "-q"]), ("image", ["docker", "images", "-q"])):
            rc, out, _ = sh(cmd + ["--filter", "label=" + self.label])
            if out.split():
                failures.append("%s(s) with label %s are still there: %s" % (kind, self.label, " ".join(out.split())))
        return failures


# --------------------------------------------------------------------------
# The cluster: containers, network, faults


class Cluster:
    def __init__(self, owned, image, n, out, log):
        self.owned, self.image, self.n, self.out, self.log = owned, image, n, out, log
        self.names = ["n%d" % (i + 1) for i in range(n)]
        self.net = None
        self.subnet = None
        self.ip = {}
        self.cid = {}  # name -> container id Docker returned
        self.node = {}  # name -> full node id
        self.mu = threading.Lock()
        self.busy = {nm: threading.Lock() for nm in self.names}  # a driver read in progress
        self.down = set()  # killed, not started again yet
        self.paused = set()
        self.disconnected = set()
        self.blackhole = {nm: set() for nm in self.names}  # name -> names it drops
        self.exits = []  # (name, exit code) seen before each start
        self.faults = []  # {t, action, args, ok, detail}
        self.outbound = self.names[-1]  # no inbound route (N1)

    def routes_dir(self, nm):
        return os.path.join(self.out, "routes", nm)

    def create(self):
        for third in range(57, 120):
            subnet = "10.213.%d.0/24" % third
            name, err = self.owned.network(subnet)
            if name:
                self.net, self.subnet = name, subnet
                break
            if "overlap" not in err and "Pool" not in err:
                raise RuntimeError("docker network create: " + err.strip())
        if not self.net:
            raise RuntimeError("no free 10.213.x.0/24 subnet")
        base = self.subnet.split("/")[0].rsplit(".", 1)[0]
        for i, nm in enumerate(self.names):
            self.ip[nm] = "%s.%d" % (base, 11 + i)
            os.makedirs(self.routes_dir(nm), exist_ok=True)
            open(os.path.join(self.routes_dir(nm), "blackhole"), "w").close()
            vol = self.owned.volume(nm)
            self.cid[nm] = self.owned.container([
                "--name", self.owned.name(nm), "--hostname", nm, "--network", self.net, "--ip", self.ip[nm],
                "--cap-add", "NET_ADMIN", "-v", vol + ":/a2p2", "-v", self.routes_dir(nm) + ":/chaos:ro",
                self.image])
        self.log("network", self.net, self.subnet, "nodes", " ".join("%s=%s" % (k, v) for k, v in self.ip.items()))

    # -- commands inside a container

    def exec(self, nm, args, timeout=60):
        return sh(["docker", "exec", self.cid[nm]] + args, timeout=timeout)

    def a2p2(self, nm, *args, timeout=60):
        rc, out, err = self.exec(nm, ["a2p2"] + list(args), timeout=timeout)
        data = None
        if rc == 0:
            try:
                data = json.loads(out) if out.strip() else {}
            except ValueError:
                data = last_json(out)
        return rc, data, (err or out).strip()

    def at_terminal(self, nm, cmd, answers, timeout=90):
        """Run an a2p2 command that asks a person, on a pseudo-terminal made
        by busybox `script` inside the container, typing answers."""
        inner = "printf %s | script -q -c %s /dev/null" % (
            shlex.quote("".join(a + "\n" for a in answers)), shlex.quote(cmd))
        return sh(["docker", "exec", self.cid[nm], "sh", "-c", inner], timeout=timeout)

    def state(self, nm):
        rc, out, _ = sh(["docker", "inspect", "-f",
                         "{{.State.Running}} {{.State.Paused}} {{.State.ExitCode}} {{.State.Pid}} {{json .NetworkSettings.Networks}}",
                         self.cid[nm]])
        if rc != 0:
            return {}
        parts = out.strip().split(" ", 4)
        nets = json.loads(parts[4]) if len(parts) > 4 else {}
        return {"running": parts[0] == "true", "paused": parts[1] == "true", "exit": int(parts[2]),
                "pid": int(parts[3]), "ip": (nets.get(self.net) or {}).get("IPAddress")}

    def until(self, nm, pred, wait=10):
        """Polls the container's state until pred holds; returns the last state and whether it did."""
        deadline = time.monotonic() + wait
        while True:
            st = self.state(nm)
            if st and pred(st):
                return st, True
            if time.monotonic() > deadline:
                return st, False
            time.sleep(0.2)

    # -- faults: each is recorded as applied only once its effect is seen

    def record(self, action, args, ok, detail=""):
        self.faults.append({"t": round(now(), 2), "action": action, "args": args, "ok": ok, "detail": detail})
        self.log("fault", action, args, "applied" if ok else "NOT APPLIED", detail)
        return ok

    def write_routes(self, nm):
        path = os.path.join(self.routes_dir(nm), "blackhole")
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            for other in sorted(self.blackhole[nm]):
                f.write(self.ip[other] + "\n")
        os.replace(tmp, path)

    def apply_routes(self, nm):
        """Writes nm's routes and, if it runs, applies them and reads them
        back. Returns True (seen applied), False (failed) or None (down or
        paused: the entry point or unpause applies them)."""
        self.write_routes(nm)
        if nm in self.down or nm in self.paused:
            return None
        rc, out, err = self.exec(nm, ["/usr/local/bin/chaos-routes.sh"], timeout=20)
        if rc != 0:
            return False
        return self.routes_now(nm) == {self.ip[o] for o in self.blackhole[nm]}

    def routes_now(self, nm):
        rc, out, _ = self.exec(nm, ["ip", "route"], timeout=20)
        if rc != 0:
            return None
        return {l.split()[1] for l in out.splitlines() if l.startswith("blackhole ")}

    def kill(self, nm):
        with self.mu:
            if nm in self.paused or nm in self.down:
                return self.record("kill", [nm], False, "skipped: paused or down")
            self.down.add(nm)
        # A kill waits for a driver read on the node to finish (at most 30 s),
        # so a failed read cannot hide what the inbox showed.
        got = self.busy[nm].acquire(timeout=30)
        try:
            before = self.state(nm)
            sh(["docker", "kill", "-s", "KILL", self.cid[nm]])
            st, ok = self.until(nm, lambda s: not s["running"])
        finally:
            if got:
                self.busy[nm].release()
        return self.record("kill", [nm], ok and st.get("exit") == 137,
                           "pid %s gone, exit %s" % (before.get("pid"), st.get("exit")))

    def start(self, nm):
        st = self.state(nm)
        if st and not st["running"]:
            self.exits.append((nm, st["exit"]))
        self.write_routes(nm)
        sh(["docker", "start", self.cid[nm]])
        st2, ok = self.until(nm, lambda s: s["running"] and not s["paused"])
        with self.mu:
            self.down.discard(nm)
        return self.record("start", [nm], ok, "previous exit %s, pid %s" % (st.get("exit"), st2.get("pid")))

    def pause(self, nm):
        with self.mu:
            if nm in self.down or nm in self.paused:
                return self.record("pause", [nm], False, "skipped: paused or down")
            self.paused.add(nm)
        sh(["docker", "pause", self.cid[nm]])
        _, ok = self.until(nm, lambda s: s["paused"])
        return self.record("pause", [nm], ok)

    def unpause(self, nm):
        with self.mu:
            if nm not in self.paused:
                return False
        sh(["docker", "unpause", self.cid[nm]])
        _, ok = self.until(nm, lambda s: not s["paused"])
        with self.mu:
            self.paused.discard(nm)
        routes = self.apply_routes(nm)
        return self.record("unpause", [nm], ok and routes is not False)

    def disconnect(self, nm):
        with self.mu:
            if nm in self.disconnected:
                return self.record("disconnect", [nm], False, "skipped: already off")
            self.disconnected.add(nm)
        sh(["docker", "network", "disconnect", "-f", self.net, self.cid[nm]])
        _, ok = self.until(nm, lambda s: not s["ip"])
        return self.record("disconnect", [nm], ok)

    def connect(self, nm):
        with self.mu:
            if nm not in self.disconnected:
                return False
        sh(["docker", "network", "connect", "--ip", self.ip[nm], self.net, self.cid[nm]])
        _, ok = self.until(nm, lambda s: s["ip"] == self.ip[nm])
        with self.mu:
            self.disconnected.discard(nm)
        return self.record("connect", [nm], ok)

    def partition(self, side_a, side_b):
        for a in side_a:
            self.blackhole[a] |= set(side_b)
        for b in side_b:
            self.blackhole[b] |= set(side_a)
        seen = {nm: self.apply_routes(nm) for nm in self.names}
        # Each pair across the split is cut if at least one of the two drops
        # the other's packets (TCP then fails both ways); a node down or
        # paused gets its routes when it starts or wakes.
        uncut = [(a, b) for a in side_a for b in side_b if not (seen[a] or seen[b])]
        failed = [nm for nm, s in seen.items() if s is False]
        return self.record("partition", [side_a, side_b], not uncut and not failed,
                           "uncut pairs %s, failed %s" % (uncut, failed) if uncut or failed else "")

    def heal_partition(self):
        for nm in self.names:
            self.blackhole[nm] = set()
        seen = {nm: self.apply_routes(nm) for nm in self.names}
        failed = [nm for nm, s in seen.items() if s is False]
        return self.record("heal", [], not failed, "failed %s" % failed if failed else "")

    def heal_all(self, keep_isolated=None):
        for nm in list(self.paused):
            self.unpause(nm)
        for nm in list(self.down):
            self.start(nm)
        for nm in list(self.disconnected):
            self.connect(nm)
        for nm in self.names:
            self.blackhole[nm] = set()
        if keep_isolated:
            for other in self.names:
                if other != keep_isolated:
                    self.blackhole[keep_isolated].add(other)
                    self.blackhole[other].add(keep_isolated)
        bad = [nm for nm in self.names if self.apply_routes(nm) is not True]
        bad += [nm for nm in self.names if not self.state(nm).get("running") or self.state(nm).get("paused")
                or self.state(nm).get("ip") != self.ip[nm]]
        self.log("healed everything" + (" except %s (sabotage)" % keep_isolated if keep_isolated else ""),
                 "; NOT HEALED: %s" % sorted(set(bad)) if bad else "")
        return not bad


# --------------------------------------------------------------------------
# The fault schedule, made from the seed before anything runs


def make_schedule(seed, names, duration):
    """A list of (seconds from start, action, args). Every kind of fault
    appears at least once; episodes follow each other with short gaps, and
    extra kills fall on nodes that are not paused at that moment."""
    rng = random.Random("faults-%s" % seed)
    ev = []
    kinds = ["kill", "pause", "disconnect", "partition"]
    rng.shuffle(kinds)
    t = rng.uniform(3, 6)
    paused = []  # (node, start, end)
    i = 0
    while i < len(kinds) or t < duration - 5:
        kind = kinds[i] if i < len(kinds) else rng.choice(["kill", "kill", "pause", "disconnect", "partition"])
        i += 1
        nm = rng.choice(names)
        if kind == "kill":
            down = rng.uniform(2, 8)
            ev += [(t, "kill", [nm]), (t + down, "start", [nm])]
            t += down
        elif kind == "pause":
            d = rng.uniform(5, 15)
            ev += [(t, "pause", [nm]), (t + d, "unpause", [nm])]
            paused.append((nm, t, t + d))
            t += d
        elif kind == "disconnect":
            d = rng.uniform(5, 15)
            ev += [(t, "disconnect", [nm]), (t + d, "connect", [nm])]
            t += d
        else:
            order = names[:]
            rng.shuffle(order)
            half = len(order) // 2
            a, b = sorted(order[:half]), sorted(order[half:])
            d = rng.uniform(15, 30)
            victim = rng.choice(names)
            k = t + d * rng.uniform(0.2, 0.5)
            down = rng.uniform(2, 6)
            ev += [(t, "partition", [a, b]), (k, "kill", [victim]), (k + down, "start", [victim]),
                   (t + d, "heal", [])]
            t += d
        t += rng.uniform(1, 4)
    # Extra kills that overlap the episodes, about one per 30 s.
    for _ in range(max(1, int(duration // 30))):
        at = rng.uniform(5, max(6, duration - 10))
        nm = rng.choice(names)
        down = rng.uniform(1, 5)
        if any(p == nm and s - 1 <= at + down and at <= e + 1 for p, s, e in paused):
            continue
        busy = [x for x in ev if x[1] in ("kill", "start") and x[2] == [nm] and abs(x[0] - at) < 10]
        if busy:
            continue
        ev += [(at, "kill", [nm]), (at + down, "start", [nm])]
    ev.sort(key=lambda x: x[0])
    return [(round(a, 2), b, c) for a, b, c in ev]


def run_faults(cl, schedule, t0, stop):
    for at, action, args in schedule:
        while not stop.is_set() and now() - t0 < at:
            time.sleep(0.05)
        if stop.is_set():
            return
        try:
            if action == "kill":
                cl.kill(args[0])
            elif action == "start":
                if args[0] in cl.down:
                    cl.start(args[0])
            elif action == "pause":
                cl.pause(args[0])
            elif action == "unpause":
                cl.unpause(args[0])
            elif action == "disconnect":
                cl.disconnect(args[0])
            elif action == "connect":
                cl.connect(args[0])
            elif action == "partition":
                cl.partition(args[0], args[1])
            elif action == "heal":
                cl.heal_partition()
        except Exception as e:  # recorded as not applied, not fatal
            cl.record(action, args, False, "error: %r" % e)


# --------------------------------------------------------------------------
# Forming the room


def form_room(cl):
    log = cl.log
    for nm in cl.names:
        for _ in range(50):
            rc, st, _ = cl.a2p2(nm, "status", "--json")
            if rc == 0:
                cl.node[nm] = st["node"]
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("daemon on %s did not answer" % nm)
        cmd = "a2p2 net on --outbound-only" if nm == cl.outbound else "a2p2 net on"
        rc, out, err = cl.at_terminal(nm, cmd, ["yes"])
        if rc != 0 or ("listening on" not in out and "outbound only" not in out):
            raise RuntimeError("net on %s: %s %s" % (nm, out, err))
        log("net on", nm, "outbound only" if nm == cl.outbound else "listening", cl.node[nm][:8])
    first = cl.names[0]
    rc, room, err = cl.a2p2(first, "room", "create", ROOM, "--json")
    if rc != 0:
        raise RuntimeError("room create: " + err)
    room_id = room["id"]
    log("room", room_id, "created on", first)
    members = [first]
    for nm in cl.names[1:]:
        rc, inv, err = cl.a2p2(first, "room", "invite", ROOM, "--json")
        if rc != 0:
            raise RuntimeError("invite: " + err)
        rc, out, err = cl.at_terminal(nm, "a2p2 join %s --no-wait" % inv["token"], ["yes"])
        if "accepted" not in out and "joined" not in out:
            raise RuntimeError("join %s: %s %s" % (nm, out, err))
        log("join", nm, "accepted on the joiner; waiting for the admins")
        deadline, tries = now() + 180, collections.Counter()
        while now() < deadline:
            if in_room(cl, nm, room_id, nm):
                break
            for adm in members:
                rc, qs, _ = cl.a2p2(adm, "confirm", "--json")
                for q in (qs or {}).get("requests", []):
                    if q.get("room") != room_id or q.get("node") != cl.node[nm]:
                        continue
                    if q["fingerprint"].replace(" ", "") != cl.node[nm]:
                        raise RuntimeError("fingerprint of %s does not match its node id" % nm)
                    tries[q["id"]] += 1
                    # A person waits and asks again when the admin has not
                    # caught up; after three refusals they accept the risk.
                    answers = ["yes", "yes"] if tries[q["id"]] > 3 else ["yes"]
                    rc, out, err = cl.at_terminal(adm, "a2p2 confirm %s --json" % q["id"], answers)
                    res = last_json(out) or {}
                    log("confirm on", adm, "for", nm, q.get("action"), res.get("state"), res.get("reason", ""),
                        "(override not_caught_up)" if len(answers) == 2 and "WARNING" in out else "")
            time.sleep(2)
        else:
            raise RuntimeError("%s did not become a member" % nm)
        members.append(nm)
        log("member", nm)
    # Every node sees every member before the workload starts.
    deadline = now() + 120
    while now() < deadline:
        if all(len(room_members(cl, nm, room_id)) == cl.n for nm in cl.names):
            break
        time.sleep(2)
    else:
        raise RuntimeError("membership did not converge")
    for nm in cl.names:  # register the workload agent before anything is sent
        cl.a2p2(nm, "inbox", "--json", "--as", AGENT, "--room", ROOM)
    roles = room_members(cl, first, room_id)
    log("room formed:", ", ".join("%s %s" % (nm, "admin" if roles.get(cl.node[nm]) else "member") for nm in cl.names))
    return room_id


def room_members(cl, nm, room_id):
    rc, rl, _ = cl.a2p2(nm, "room", "list", ROOM, "--json")
    for r in (rl or {}).get("rooms", []):
        if r["id"] == room_id:
            return {m["node"]: m.get("admin", False) for m in r["members"]}
    return {}


def in_room(cl, nm, room_id, who):
    return cl.node[who] in room_members(cl, nm, room_id)


# --------------------------------------------------------------------------
# The workload


class Book:
    """What the driver did and saw, for the checker."""

    def __init__(self):
        self.mu = threading.Lock()
        self.sends = []  # {node, kind, n, text, id?, ok, err, reply_to, t0, t1, wall0, wall1}
        self.drains = collections.defaultdict(list)  # node -> [entry...] from successful reads
        self.failed_drains = collections.Counter()
        self.acks = []  # {node, agent, id, handled}
        self.withdrawn = []  # {node, id, secret, ok, shown_before}
        self.replaced = []  # {node, id, text, ok, shown_before}

    def add(self, field, item):
        with self.mu:
            getattr(self, field).append(item)

    def shown_elsewhere(self, key, author):
        with self.mu:
            return any(e.get("id") == key for nm, es in self.drains.items() if nm != author for e in es)


def send(cl, book, nm, kind, n, extra="", reply_to=None, ask=False, replaces=None, text=None):
    if text is None:
        text = "%s %s %d%s" % (kind, nm, n, (" " + extra) if extra else "")
    args = ["send", "--json", "--as", AGENT, "--room", ROOM]
    if reply_to:
        args += ["--reply-to", reply_to]
    if ask:
        args.append("--ask-handled")
    if replaces:
        args += ["--replaces", replaces]
    t0, wall0 = now(), time.time()
    rc, res, err = cl.a2p2(nm, *(args + ([text] if text else [])), timeout=60)
    rec = {"node": nm, "kind": kind, "n": n, "text": text, "ok": rc == 0 and bool(res and res.get("id")),
           "id": (res or {}).get("id"), "err": "" if rc == 0 else err[-300:], "reply_to": reply_to,
           "ask": ask, "replaces": replaces, "t0": round(t0, 2), "t1": round(now(), 2),
           "wall0": wall0, "wall1": time.time()}
    if not replaces:
        book.add("sends", rec)
    return rec


def drain(cl, book, nm, me):
    """Read the agent's inbox (and confirm it, as the CLI does), then report
    handled what may be: room-wide posts that asked, and replies to us."""
    with cl.busy[nm]:
        rc, res, err = cl.a2p2(nm, "inbox", "--json", "--as", AGENT, "--room", ROOM, "--limit", "100", timeout=60)
        if rc != 0:
            # Refused before it ran (paused or stopped container): nothing was
            # shown or confirmed. Otherwise the read may have been confirmed
            # without the driver seeing it: this member's inbox result is
            # then inconclusive.
            if "is paused" not in err and "is not running" not in err:
                with book.mu:
                    book.failed_drains[nm] += 1
            return None
        entries = (res or {}).get("entries", [])
        with book.mu:
            book.drains[nm].extend(entries)
        for e in entries:
            if e.get("kind") != "message":
                continue
            m = TEXT_RE.match(e.get("text") or "")
            if (m and m.group(1) == "a") or (me in (e.get("to") or [])):
                rc, r, err = cl.a2p2(nm, "ack", "--json", "--as", AGENT, e["id"])
                if rc == 0 and r and r.get("handled"):
                    book.add("acks", {"node": nm, "agent": me, "id": e["id"], "handled": r["handled"]})
        return res


def wait_shown(book, key, author, stop, wait=45, read=None):
    # Only makes the notice path likely; the checker decides from what the
    # inbox reads show whether it was exercised.
    """Waits until another member's agent was shown key, so that the replace
    or withdraw that follows has to reach it as a notice. The waiting agent
    keeps reading its own inbox (read): every member reaches this point at
    about the same moment, and if none read while waiting, each would wait
    for the others and no original would ever be shown."""
    deadline = now() + wait
    while now() < deadline and not stop.is_set():
        if book.shown_elsewhere(key, author):
            return True
        if read:
            read()
        time.sleep(0.5)
    return False


def workload(cl, book, nm, seed, duration, t0, stop, me):
    rng = random.Random("work-%s-%s" % (seed, nm))
    n = 0
    seen = []  # message ids this agent was shown, to reply to
    did_replace = did_withdraw = False

    def read():
        res = drain(cl, book, nm, me)
        for e in (res or {}).get("entries", []):
            if e.get("kind") == "message":
                seen.append(e["id"])

    while not stop.is_set() and now() - t0 < duration:
        time.sleep(rng.uniform(0.3, 1.5))
        frac = (now() - t0) / duration
        if not did_replace and frac > 0.3:
            did_replace = True
            n += 1
            r = send(cl, book, nm, "o", n, "original")
            if r["ok"]:
                shown = wait_shown(book, r["id"], nm, stop, read=read)
                new = "o %s %d replaced" % (nm, n)
                x = send(cl, book, nm, "o", n, replaces=r["id"], text=new)
                book.add("replaced", {"node": nm, "id": r["id"], "text": new, "ok": x["ok"], "err": x["err"],
                                      "shown_before": shown})
            continue
        if not did_withdraw and frac > 0.6:
            did_withdraw = True
            n += 1
            secret = "secret-%s-%08x" % (nm, rng.getrandbits(32))
            r = send(cl, book, nm, "s", n, secret)
            if r["ok"]:
                shown = wait_shown(book, r["id"], nm, stop, read=read)
                x = send(cl, book, nm, "s", n, replaces=r["id"], text="")
                book.add("withdrawn", {"node": nm, "id": r["id"], "secret": secret, "ok": x["ok"], "err": x["err"],
                                       "shown_before": shown})
            continue
        x = rng.random()
        if x < 0.45:
            n += 1
            send(cl, book, nm, "m", n)
        elif x < 0.55:
            n += 1
            send(cl, book, nm, "a", n, ask=True)
        elif x < 0.70 and seen:
            n += 1
            send(cl, book, nm, "r", n, reply_to=rng.choice(seen[-20:]))
        else:
            read()
    return n


# --------------------------------------------------------------------------
# Settling and the snapshot


def history(cl, nm):
    out, before = [], None
    while True:
        args = ["history", "--json", "--room", ROOM, "-n", "500"]
        if before:
            args += ["--before", before]
        rc, res, err = cl.a2p2(nm, *args)
        if rc != 0:
            return None
        page = res.get("entries", [])
        out = page + out
        if len(page) < 500:
            return out
        before = page[0]["id"]


def record_key(e):
    """A record as every member should hold it: every field."""
    return json.dumps(e, sort_keys=True)


def settle(cl, book, me, timeout, log):
    """Drain and compare until every node holds the same records and every
    accepted message, or timeout. Returns seconds taken, or None."""
    t0 = now()
    expected = {s["id"] for s in book.sends if s["ok"]} - {w["id"] for w in book.withdrawn}
    while now() - t0 < timeout:
        for nm in cl.names:
            res = drain(cl, book, nm, me[nm])
            while res and res.get("entries"):
                res = drain(cl, book, nm, me[nm])
        hs = {nm: history(cl, nm) for nm in cl.names}
        if all(h is not None for h in hs.values()):
            sets = {nm: {record_key(e) for e in h} for nm, h in hs.items()}
            keys = {nm: {e["key"] for e in h} for nm, h in hs.items()}
            same = len({frozenset(s) for s in sets.values()}) == 1
            missing = {nm: len(expected - k) for nm, k in keys.items()}
            log("settling: same records %s, missing accepted %s" % (same, missing))
            if same and not any(missing.values()):
                return now() - t0
        time.sleep(5)
    return None


def snapshot(cl, book, me, room_id, log, settled_at):
    snap = {"nodes": cl.names, "node_ids": cl.node, "agents": me, "room": room_id, "outbound": cl.outbound,
            "history": {}, "traces": {}, "logs": {}, "running": {}, "db_secrets": {}, "waiting": {}}
    for nm in cl.names:  # a last read: everything delivered is shown
        res = drain(cl, book, nm, me[nm])
        while res and res.get("entries"):
            res = drain(cl, book, nm, me[nm])
        snap["waiting"][nm] = (res or {}).get("waiting", "read failed")
    snap.update({"sends": book.sends, "acks": book.acks, "withdrawn": book.withdrawn, "replaced": book.replaced,
                 "drains": dict(book.drains), "failed_drains": dict(book.failed_drains),
                 "exits": cl.exits, "faults": cl.faults})
    for nm in cl.names:
        snap["history"][nm] = history(cl, nm)
        rc, out, err = sh(["docker", "logs", cl.cid[nm]])
        text = out + err
        with open(os.path.join(cl.out, "daemon-%s.log" % nm), "w") as f:
            f.write(text)
        snap["logs"][nm] = {"lines": text.count("\n"),
                            "panic": [l for l in text.splitlines() if "panic" in l or "fatal error" in l][:20],
                            "errors": sum(1 for l in text.splitlines() if '"level":"ERROR"' in l)}
        snap["running"][nm] = bool(cl.state(nm).get("running"))
    # Handled reports: wait a little for the last acks to travel.
    deadline = now() + 90
    while True:
        traces, short = {}, 0
        for a in book.acks:
            for nm in cl.names:
                rc, t, _ = cl.a2p2(nm, "trace", "--json", a["id"])
                traces["%s %s" % (nm, a["id"])] = t if rc == 0 else None
                if a["agent"] not in [h["agent"] for h in ((t or {}).get("handled") or [])]:
                    short += 1
        if short == 0 or now() > deadline:
            break
        log("waiting for %d handled reports" % short)
        time.sleep(5)
    snap["traces"] = traces
    # Withdrawn text must be out of every file of the store once the daemon
    # has scrubbed it (every minute, DECISIONS D43).
    secrets_ = [w["secret"] for w in book.withdrawn if w["ok"]]
    if secrets_:
        wait = settled_at + SCRUB_WAIT - now()
        if wait > 0:
            log("waiting %.0fs for the store's scrub before reading its files" % wait)
            time.sleep(wait)
        pat = "|".join(secrets_)
        for nm in cl.names:
            rc, out, _ = cl.exec(nm, ["sh", "-c", "for f in /a2p2/a2p2.db*; do printf '%%s %%s\\n' \"$f\" "
                                      "\"$(grep -c -E %s \"$f\")\"; done" % shlex.quote(pat)])
            snap["db_secrets"][nm] = {l.split()[0]: int(l.split()[1]) for l in out.splitlines()
                                      if len(l.split()) == 2 and l.split()[1].isdigit()} if rc == 0 else None
    return snap


# --------------------------------------------------------------------------
# The checker


def notices_exercised(snap, which):
    """How many accepted replaces (or withdraws) some reader was shown the
    original of, as the inbox reads show: those whose notice is checked."""
    n = 0
    for x in snap[which]:
        if not x["ok"]:
            continue
        for es in snap["drains"].values():
            if any(e.get("id") == x["id"] and e.get("kind") == "message" and
                   (which == "withdrawn" or e.get("text") != x["text"]) for e in es):
                n += 1
                break
    return n


def evaluate(snap):
    """(violations, inconclusive): what the snapshot shows broken, and why
    it may not show enough to pass."""
    v, inc = [], []
    nodes = snap["nodes"]
    hist = snap["history"]
    agents = snap["agents"]
    for nm in nodes:
        if hist.get(nm) is None:
            v.append("%s: history could not be read" % nm)
    nodes_ok = [nm for nm in nodes if hist.get(nm) is not None]
    sends = snap["sends"]
    accepted = {s["id"]: s for s in sends if s["ok"]}
    # A withdraw whose command failed may or may not have been written:
    # such a message is expected neither present nor absent.
    withdrawn = {w["id"] for w in snap["withdrawn"]}
    replaced = {r["id"]: r for r in snap["replaced"]}

    # -- Inconclusive: faults that did not happen, an empty run.
    faults = snap.get("faults") or []
    for kind in ("kill", "pause", "disconnect", "partition"):
        if not any(f["action"] == kind and f["ok"] for f in faults):
            inc.append("no %s was seen applied" % kind)
    last_fault = max([f["t"] for f in faults] or [0])
    heals = sorted(f["t"] for f in faults if f["action"] == "heal")
    good_split = []
    for f in faults:
        if f["action"] != "partition" or not f["ok"]:
            continue
        end = next((h for h in heals if h >= f["t"]), last_fault)
        side = {nm: i for i, half in enumerate(f["args"]) for nm in half}
        sent = {side[s["node"]] for s in sends if s["ok"] and s["node"] in side and f["t"] <= s["t0"] and s["t1"] <= end}
        if sent == {0, 1}:
            good_split.append(f["t"])
    if not good_split:
        inc.append("no partition was seen applied with an accepted send on both sides during it")
    for nm in nodes:
        if not any(s["ok"] and s["node"] == nm and s["t0"] >= last_fault for s in sends):
            inc.append("%s had no accepted send after the last fault" % nm)
    if not any(s["ok"] and s["kind"] == "r" for s in sends):
        inc.append("no reply in a thread was accepted")
    if not snap["acks"]:
        inc.append("no handled report was made")
    if not any(r["ok"] for r in snap["replaced"]):
        inc.append("no replace was accepted")
    if not any(w["ok"] for w in snap["withdrawn"]):
        inc.append("no withdraw was accepted")
    if not notices_exercised(snap, "replaced"):
        inc.append("no reader was shown an original before its replace, so no replace notice was checked")
    if not notices_exercised(snap, "withdrawn"):
        inc.append("no reader was shown a message before its withdraw, so no withdraw notice was checked")
    for nm, n in (snap.get("failed_drains") or {}).items():
        if n:
            inc.append("%s: %d inbox read(s) failed partway, so its inbox cannot be checked for completeness" % (nm, n))
    if not nodes_ok:
        return v, inc

    # 1. The same records on every member, every field.
    sets = {nm: {record_key(e) for e in hist[nm]} for nm in nodes_ok}
    union = set().union(*sets.values())
    for nm in nodes_ok:
        lack = union - sets[nm]
        if lack:
            v.append("%s lacks %d record(s) another member holds, e.g. %s" % (nm, len(lack), sorted(lack)[:2]))
    # 2. Exactly once per member.
    for nm in nodes_ok:
        c = collections.Counter(e["key"] for e in hist[nm])
        dup = [k for k, x in c.items() if x > 1]
        if dup:
            v.append("%s holds %d message(s) more than once: %s" % (nm, len(dup), dup[:3]))
    # 3. Nothing a sender accepted is missing anywhere, and what is held is
    # what the driver sent (so the same corruption on every member shows).
    by_node = {nm: {e["key"]: e for e in hist[nm]} for nm in nodes_ok}
    attempted = collections.defaultdict(set)  # text -> nodes that tried to send it
    for s in sends:
        attempted[s["text"]].add(s["node"])
    for r in snap["replaced"]:
        attempted[r["text"]].add(r["node"])
    inv = {v_: k for k, v_ in snap["node_ids"].items()}
    for nm in nodes_ok:
        held = by_node[nm]
        miss = sorted(set(accepted) - withdrawn - set(held))
        if miss:
            v.append("%s is missing %d accepted message(s): %s" % (
                nm, len(miss), ", ".join("%s (%s)" % (m, accepted[m]["text"]) for m in miss[:5])))
        for key, e in held.items():
            author = inv.get(e.get("from_node"))
            if e.get("from") != agents.get(author):
                v.append("%s: %s is from %s on %s, not a workload agent" % (nm, key, e.get("from"), e.get("from_node")))
            if e.get("text") not in attempted or author not in attempted[e.get("text")]:
                v.append("%s: %s holds text %r that %s never sent" % (nm, key, e.get("text"), author))
            s = accepted.get(key)
            if not s:
                continue
            want = s["text"]
            if key in replaced:
                r = replaced[key]
                want = r["text"] if r["ok"] else None  # a failed replace may or may not have been written
            if want is not None and e.get("text") != want:
                v.append("%s: %s reads %r, but %r was sent" % (nm, key, e.get("text"), want))
            if e.get("from_node") != snap["node_ids"][s["node"]]:
                v.append("%s: %s names author %s, but %s sent it" % (nm, key, e.get("from_node"), s["node"]))
            try:
                created = iso_seconds(e["created"])
                if not (s["wall0"] - 2 <= created <= s["wall1"] + 2):
                    v.append("%s: %s was created at %s, outside its send" % (nm, key, e["created"]))
            except (KeyError, ValueError):
                v.append("%s: %s has no valid created time" % (nm, key))
            if s["kind"] == "r":
                parent = held.get(s["reply_to"]) or {}
                if e.get("parent") != s["reply_to"]:
                    v.append("%s: reply %s names parent %s, not %s" % (nm, key, e.get("parent"), s["reply_to"]))
                if parent and e.get("thread") != (parent.get("thread") or parent["key"]):
                    v.append("%s: reply %s is in thread %s, not its parent's" % (nm, key, e.get("thread")))
                if parent and e.get("to") != [parent.get("from")]:
                    v.append("%s: reply %s is addressed to %s, not the parent's author" % (nm, key, e.get("to")))
            elif e.get("to") or e.get("parent"):
                v.append("%s: room-wide post %s carries addressees or a parent" % (nm, key))
    # 4. Each author's messages in the author's order: in history, and in
    # the order the inbox showed them (arrival); nothing marked late.
    for nm in nodes_ok:
        last = {}
        for e in hist[nm]:
            m = TEXT_RE.match(e.get("text") or "")
            if m:
                a, n = m.group(2), int(m.group(3))
                if a in last and n <= last[a]:
                    v.append("%s: history shows %s's message %d after %d" % (nm, a, n, last[a]))
                last[a] = n
            if e.get("late"):
                v.append("%s: %s is marked late" % (nm, e["key"]))
    for nm in nodes:
        entries = snap["drains"].get(nm, [])
        shown = collections.Counter()
        last = {}
        for e in entries:
            if e.get("late"):
                v.append("%s: inbox entry %s is marked late" % (nm, e["key"]))
            if e.get("kind") != "message":
                continue
            shown[e["key"]] += 1
            m = TEXT_RE.match(e.get("text") or "")
            if m:
                a, n = m.group(2), int(m.group(3))
                if a in last and n <= last[a]:
                    v.append("%s: inbox showed %s's message %d after %d" % (nm, a, n, last[a]))
                last[a] = max(n, last.get(a, 0))
        # A read whose command failed is not recorded, and a kill waits for
        # a read in progress, so nothing can be shown here twice.
        dup = [k for k, x in shown.items() if x > 1]
        if dup:
            v.append("%s: inbox showed %d message(s) twice: %s" % (nm, len(dup), dup[:3]))
        if (snap.get("failed_drains") or {}).get(nm) or nm not in nodes_ok:
            continue  # inconclusive, said above
        mine = agents[nm]
        to = {e["key"]: e.get("to") or [] for e in hist[nm]}
        others = {i for i, s in accepted.items() if s["node"] != nm and (not to.get(i) or mine in to[i])} - withdrawn
        unshown = others - set(shown)
        if unshown:
            v.append("%s: inbox never showed %d accepted message(s): %s" % (nm, len(unshown), sorted(unshown)[:3]))
    # 5. Every handled report arrived on every member.
    for a in snap["acks"]:
        for nm in nodes_ok:
            t = snap["traces"].get("%s %s" % (nm, a["id"]))
            if a["agent"] not in [h["agent"] for h in ((t or {}).get("handled") or [])]:
                v.append("%s: no handled report by %s for %s" % (nm, a["agent"], a["id"]))
    # 6. Withdrawn text is gone from history, from what readers are shown
    # after the notice, and from the store's files; a reader shown the
    # original is told. The replacement is what readers see in history and
    # in the inbox: a reader shown the original is shown the replacement.
    for w in snap["withdrawn"]:
        if not w["ok"]:
            continue
        for nm in nodes_ok:
            if any(e["key"] == w["id"] or w["secret"] in (e.get("text") or "") for e in hist[nm]):
                v.append("%s still shows withdrawn message %s in history" % (nm, w["id"]))
        for nm in nodes:
            es = [e for e in snap["drains"].get(nm, []) if e.get("id") == w["id"]]  # a notice's key is its own record
            kinds = [e["kind"] for e in es]
            if "message" in kinds:
                notice = [i for i, k in enumerate(kinds) if k == "withdrawn"]
                if not notice:
                    v.append("%s: the agent was shown %s but never told it was withdrawn" % (nm, w["id"]))
                elif any(w["secret"] in (e.get("text") or "") for e in es[notice[0]:]):
                    v.append("%s: the inbox showed withdrawn text after the notice for %s" % (nm, w["id"]))
        for nm, files in (snap.get("db_secrets") or {}).items():
            if files is None:
                inc.append("%s: the store's files could not be read" % nm)
                continue
            for f, n in files.items():
                if n:
                    v.append("%s: withdrawn text is still in %s (%d line(s)) after the scrub" % (nm, f, n))
    for r in snap["replaced"]:
        if not r["ok"]:
            continue
        for nm in nodes_ok:
            got = [e.get("text") for e in hist[nm] if e["key"] == r["id"]]
            if got != [r["text"]]:
                v.append("%s shows %s as %r, not the replacement %r" % (nm, r["id"], got, r["text"]))
        for nm in nodes:
            es = [e for e in snap["drains"].get(nm, []) if e.get("id") == r["id"]]
            if es and es[-1].get("text") != r["text"]:
                v.append("%s: the agent was last shown %s as %r, not the replacement" % (nm, r["id"], es[-1].get("text")))
            if any(e["kind"] == "message" and e.get("text") != r["text"] for e in es) and \
                    not any(e["kind"] == "replaced" for e in es):
                v.append("%s: the agent was shown the original of %s but never told it was replaced" % (nm, r["id"]))
    for nm, w in (snap.get("waiting") or {}).items():
        if w:
            v.append("%s: the inbox still waits for %s" % (nm, w))
    # 7. No daemon panicked; every daemon runs; kills were the only exits.
    for nm in nodes:
        if snap["logs"].get(nm, {}).get("panic"):
            v.append("%s logged a panic: %s" % (nm, snap["logs"][nm]["panic"][0][:200]))
        if not snap["running"].get(nm):
            v.append("%s is not running at the end" % nm)
    for nm, code in snap["exits"]:
        if code != 137:
            v.append("%s exited with %s, not by the kill (137)" % (nm, code))
    return v, inc


def summarize(snap, violations, inconclusive, extra):
    sends = snap["sends"]
    faults = snap.get("faults") or []
    applied = collections.Counter(f["action"] for f in faults if f["ok"])
    missed = collections.Counter(f["action"] for f in faults if not f["ok"])
    lines = ["a2p2 chaos run"]
    for k, val in extra.items():
        lines.append("%-22s %s" % (k, val))
    lines += [
        "%-22s %s" % ("faults applied", dict(applied)),
        "%-22s %s" % ("faults not applied", dict(missed) or "none"),
        "%-22s %d accepted of %d tried" % ("sends", sum(1 for s in sends if s["ok"]), len(sends)),
        "%-22s %d" % ("replies accepted", sum(1 for s in sends if s["ok"] and s["kind"] == "r")),
        "%-22s %d" % ("handled reports", len(snap["acks"])),
        "%-22s %d (notice checked: %d)" % ("replaced", sum(1 for r in snap["replaced"] if r["ok"]),
                                           notices_exercised(snap, "replaced")),
        "%-22s %d (notice checked: %d)" % ("withdrawn", sum(1 for w in snap["withdrawn"] if w["ok"]),
                                           notices_exercised(snap, "withdrawn")),
        "%-22s %s" % ("records per node", {nm: len(h or []) for nm, h in snap["history"].items()}),
        "%-22s %s" % ("failed reads", snap.get("failed_drains") or "none"),
        "%-22s %s" % ("daemon exits", dict(collections.Counter(c for _, c in snap["exits"]))),
        "%-22s %s" % ("error log lines", {nm: l["errors"] for nm, l in snap["logs"].items()}),
        "%-22s %s" % ("withdrawn text in files", {nm: sum((f or {}).values()) for nm, f in
                                                  (snap.get("db_secrets") or {}).items()} or "n/a"),
        "",
        "violations: %d" % len(violations),
    ]
    lines += ["  - " + x for x in violations]
    lines += ["inconclusive: %d" % len(inconclusive)]
    lines += ["  - " + x for x in inconclusive]
    return "\n".join(lines) + "\n"


def verdict(violations, inconclusive):
    return 1 if violations else 3 if inconclusive else 0


# --------------------------------------------------------------------------
# Commands


def build(log, owned):
    rc, arch, err = sh(["docker", "version", "--format", "{{.Server.Arch}}"])
    if rc != 0:
        raise RuntimeError("docker is not available: " + err.strip())
    ctx = os.path.join(ROOT, ".chaos", "image-" + owned.run_id)
    os.makedirs(ctx, exist_ok=True)
    try:
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch.strip())
        p = subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", os.path.join(ctx, "a2p2"),
                            "./cmd/a2p2"], cwd=ROOT, env=env, capture_output=True, text=True)
        if p.returncode != 0:
            raise RuntimeError("go build: " + p.stderr)
        for f in ("Dockerfile", "entry.sh", "routes.sh"):
            shutil.copy(os.path.join(HERE, f), ctx)
        tag = owned.image(ctx)
    finally:
        shutil.rmtree(ctx, ignore_errors=True)
    rc, size, _ = sh(["docker", "image", "inspect", "-f", "{{.Size}}", tag])
    log("image", tag, "%.1f MB" % (int(size.strip() or 0) / 1e6), "linux/%s" % arch.strip())
    return tag, int(size.strip() or 0)


def cmd_run(o):
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    run_id = secrets.token_hex(4)
    out = os.path.abspath(o.out or os.path.join(ROOT, ".chaos", "%s-seed%s-n%d" % (stamp, o.seed, o.nodes)))
    os.makedirs(out, exist_ok=True)
    log = Log(os.path.join(out, "run.log"))
    log("run", run_id, "seed", o.seed, "nodes", o.nodes, "duration", o.duration, "out", out)
    if o.nodes < 3:
        log("need at least 3 nodes")
        return 2
    owned = Owned(run_id, log)
    cleaning = threading.Event()

    def on_signal(signum, frame):
        if cleaning.is_set():
            log("signal %d during cleanup: ignored until cleanup ends" % signum)
            return
        raise Interrupted("signal %d" % signum)

    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)
    status = 2
    cl = None
    try:
        image, size = build(log, owned)
        cl = Cluster(owned, image, o.nodes, out, log)
        schedule = make_schedule(o.seed, cl.names, o.duration)
        # Every kind of fault runs at least once, even in a short run.
        o.duration = max(o.duration, schedule[-1][0] + 3)
        with open(os.path.join(out, "schedule.json"), "w") as f:
            json.dump(schedule, f, indent=1)
        for at, action, args in schedule:
            log("schedule %6.1fs %s %s" % (at, action, args))
        cl.create()
        t_form = now()
        room_id = form_room(cl)
        log("room formed in %.0fs" % (now() - t_form))
        me = {nm: "%s@%s" % (AGENT, cl.node[nm][:8]) for nm in cl.names}
        book = Book()
        stop = threading.Event()
        t0 = now()
        threads = [threading.Thread(target=run_faults, args=(cl, schedule, t0, stop), daemon=True)]
        counts = {}

        def work(nm):
            try:
                counts[nm] = workload(cl, book, nm, o.seed, o.duration, t0, stop, me[nm])
            except Exception as e:
                log("workload on", nm, "stopped:", repr(e))
                counts[nm] = 10000

        threads += [threading.Thread(target=work, args=(nm,), daemon=True) for nm in cl.names]
        for t in threads:
            t.start()
        for t in threads[1:]:
            t.join()
        stop.set()
        threads[0].join()
        log("workload and faults done after %.0fs" % (now() - t0))
        isolated = cl.names[1] if o.sabotage == "isolate" else None
        healed = cl.heal_all(keep_isolated=isolated)
        cl.faults.append({"t": round(now(), 2), "action": "heal-all", "args": [], "ok": healed, "detail": ""})
        # After the heal, every node writes once more and must reach all.
        for nm in cl.names:
            for _ in range(30):
                if send(cl, book, nm, "f", counts.get(nm, 0) + 1, "final")["ok"]:
                    break
                time.sleep(1)
        took = settle(cl, book, me, o.settle, log)
        log("settled in %.0fs" % took if took is not None else "did not settle in %ds" % o.settle)
        snap = snapshot(cl, book, me, room_id, log, now())
        snap["meta"] = {"seed": o.seed, "nodes": o.nodes, "duration": o.duration, "settle": took,
                        "sabotage": o.sabotage, "image_bytes": size, "run_id": run_id}
        with open(os.path.join(out, "snapshot.json"), "w") as f:
            json.dump(snap, f, indent=1)
        violations, inconclusive = evaluate(snap)
        if took is None:
            violations.insert(0, "the room did not settle within %ds of the heal" % o.settle)
        if not healed and not isolated:
            inconclusive.insert(0, "the final heal was not seen applied on every node")
        summary = summarize(snap, violations, inconclusive, {
            "run": run_id, "seed": o.seed, "nodes": o.nodes, "duration": "%.0fs" % o.duration,
            "settled after": "%.0fs" % took if took is not None else "no",
            "image": "%.1f MB" % (size / 1e6), "sabotage": o.sabotage or "none", "out": out})
        with open(os.path.join(out, "summary.txt"), "w") as f:
            f.write(summary)
        print(summary)
        status = verdict(violations, inconclusive)
    except Interrupted as e:
        log("INTERRUPTED (%s): cleaning up" % e)
        status = 2
    except Exception as e:
        log("RUN FAILED:", e)
        if cl:
            for nm, cid in cl.cid.items():
                rc, out_, err = sh(["docker", "logs", cid])
                with open(os.path.join(out, "daemon-%s.log" % nm), "w") as f:
                    f.write(out_ + err)
        status = 2
    finally:
        cleaning.set()
        if o.keep:
            log("--keep: left in Docker:", json.dumps(owned.describe()))
        else:
            failures = owned.remove()
            for x in failures:
                log("CLEANUP FAILED:", x)
            if failures:
                status = status or 2
                if status == 3:
                    status = 2
            else:
                log("removed everything run", run_id, "created:", json.dumps(owned.describe()))
    log("exit status", status)
    return status


def cmd_check(o):
    with open(os.path.join(o.dir, "snapshot.json")) as f:
        snap = json.load(f)
    if o.drop:
        node, author = o.drop.split(":")
        before = len(snap["history"][node])
        snap["history"][node] = [e for e in snap["history"][node]
                                 if not (TEXT_RE.match(e.get("text") or "") and
                                         TEXT_RE.match(e["text"]).group(2) == author)]
        print("dropped %d of %s's records from %s's copy" % (before - len(snap["history"][node]), author, node))
    if o.corrupt:
        key = next(e["key"] for e in snap["history"][snap["nodes"][0]] if (e.get("text") or "").startswith("m "))
        for nm in snap["nodes"]:
            for e in snap["history"][nm]:
                if e["key"] == key:
                    e["text"] = e["text"] + " (changed)"
        print("changed the text of %s the same way on every member" % key)
    if o.unverify:
        n = 0
        for f in snap.get("faults") or []:
            if f["action"] == o.unverify:
                f["ok"], n = False, n + 1
        print("marked %d %s fault(s) as not applied" % (n, o.unverify))
    if o.empty:
        snap["sends"], snap["acks"], snap["replaced"], snap["withdrawn"] = [], [], [], []
        snap["history"] = {nm: [] for nm in snap["nodes"]}
        snap["drains"], snap["traces"], snap["db_secrets"] = {}, {}, {}
        print("removed every send, read and record: a run in which nothing got through")
    violations, inconclusive = evaluate(snap)
    for title, xs in (("violations", violations), ("inconclusive", inconclusive)):
        print("%s: %d" % (title, len(xs)))
        for x in xs[:20]:
            print("  - " + x[:300])
        if len(xs) > 20:
            print("  ... and %d more" % (len(xs) - 20))
    return verdict(violations, inconclusive)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--nodes", type=int, default=3)
    r.add_argument("--duration", type=float, default=90, help="seconds of workload and faults")
    r.add_argument("--seed", default="1")
    r.add_argument("--settle", type=float, default=420, help="seconds the room may take to settle after the heal")
    r.add_argument("--out")
    r.add_argument("--sabotage", choices=["isolate"], help="break delivery on purpose: one node stays cut off")
    r.add_argument("--keep", action="store_true", help="leave this run's containers, volumes, network and image")
    c = sub.add_parser("check")
    c.add_argument("dir")
    c.add_argument("--drop", help="NODE:AUTHOR, delete AUTHOR's records from NODE's copy first")
    c.add_argument("--corrupt", action="store_true", help="change one message's text on every member first")
    c.add_argument("--unverify", choices=FAULTS, help="mark every fault of this kind as not applied first")
    c.add_argument("--empty", action="store_true", help="remove every send, read and record first")
    o = p.parse_args()
    sys.exit(cmd_run(o) if o.cmd == "run" else cmd_check(o))


if __name__ == "__main__":
    main()
