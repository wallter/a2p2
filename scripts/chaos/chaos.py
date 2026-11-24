#!/usr/bin/env python3
"""Chaos run of real a2p2 daemons in Docker containers (requirements D1 to
D6, D9, N1). Python 3 standard library only.

    chaos.py run   [--nodes 3] [--duration 90] [--seed 1] [--settle 420]
                   [--out DIR] [--sabotage isolate] [--keep]
    chaos.py check DIR [--drop NODE:AUTHOR]

`run` builds the static Linux binary and a small image, starts one daemon
per container (its own home volume, one user-defined network, the last
node outbound only), forms one room by real invites and confirmations
answered at a pseudo-terminal inside the containers, runs a workload on
every node while a fault schedule made from the seed runs (kill -9 and
restart, pause, network disconnect, a partition into two halves with a
kill inside it), heals everything, waits for the room to settle, and
checks what every member holds. Everything it created is removed at the
end unless --keep. The exit status is 0 when no check failed, 1 when one
did, 2 when the run itself could not be carried out.

`check` runs the checker again on the snapshot a run left in DIR;
--drop NODE:AUTHOR first deletes AUTHOR's records from NODE's copy, to
show that the checker fails when delivery is broken.
"""

import argparse
import collections
import datetime
import hashlib
import json
import os
import random
import re
import shlex
import shutil
import subprocess
import sys
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HERE = os.path.dirname(os.path.abspath(__file__))
PREFIX = "a2p2chaos"
IMAGE = PREFIX + ":run"
ROOM = "chaos"
AGENT = "w"
TEXT_RE = re.compile(r"^([maorsf]) (n\d+) (\d+)(?: (.*))?$")

# --------------------------------------------------------------------------
# Small helpers


def now():
    return time.monotonic()


class Log:
    """Timestamped lines to the run's log file and to stderr."""

    def __init__(self, path):
        self.f = open(path, "a", buffering=1)
        self.t0 = now()
        self.mu = threading.Lock()

    def __call__(self, *parts):
        line = "%7.1fs %s" % (now() - self.t0, " ".join(str(p) for p in parts))
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


# --------------------------------------------------------------------------
# The cluster: containers, network, faults


class Cluster:
    def __init__(self, run_id, n, out, log):
        self.run_id, self.n, self.out, self.log = run_id, n, out, log
        self.names = ["n%d" % (i + 1) for i in range(n)]
        self.net = "%s-%s-net" % (PREFIX, run_id)
        self.subnet = None
        self.ip = {}
        self.node = {}  # name -> full node id
        self.mu = threading.Lock()
        self.down = set()  # killed, not started again yet
        self.paused = set()
        self.disconnected = set()
        self.blackhole = {nm: set() for nm in self.names}  # name -> names it drops
        self.exits = []  # (name, exit code) seen before each start
        self.outbound = self.names[-1]  # no inbound route (N1)

    def cname(self, nm):
        return "%s-%s-%s" % (PREFIX, self.run_id, nm)

    def vol(self, nm):
        return "%s-%s-%s" % (PREFIX, self.run_id, nm)

    def routes_dir(self, nm):
        return os.path.join(self.out, "routes", nm)

    def create(self):
        for third in range(57, 90):
            subnet = "10.213.%d.0/24" % third
            rc, _, err = sh(["docker", "network", "create", "--subnet", subnet, self.net])
            if rc == 0:
                self.subnet = subnet
                break
            if "overlap" not in err and "Pool" not in err:
                raise RuntimeError("docker network create: " + err.strip())
        if not self.subnet:
            raise RuntimeError("no free 10.213.x.0/24 subnet")
        base = self.subnet.split("/")[0].rsplit(".", 1)[0]
        for i, nm in enumerate(self.names):
            self.ip[nm] = "%s.%d" % (base, 11 + i)
            os.makedirs(self.routes_dir(nm), exist_ok=True)
            open(os.path.join(self.routes_dir(nm), "blackhole"), "w").close()
            sh(["docker", "volume", "create", self.vol(nm)], check=True)
            sh(["docker", "run", "-d", "--name", self.cname(nm), "--hostname", nm,
                "--network", self.net, "--ip", self.ip[nm], "--cap-add", "NET_ADMIN",
                "-v", self.vol(nm) + ":/a2p2", "-v", self.routes_dir(nm) + ":/chaos:ro",
                "--label", PREFIX + "=" + self.run_id, IMAGE], check=True)
        self.log("network", self.net, self.subnet, "nodes", " ".join("%s=%s" % (k, v) for k, v in self.ip.items()))

    def destroy(self):
        for nm in self.names:
            sh(["docker", "rm", "-f", self.cname(nm)])
        for nm in self.names:
            sh(["docker", "volume", "rm", "-f", self.vol(nm)])
        sh(["docker", "network", "rm", self.net])

    # -- commands inside a container

    def exec(self, nm, args, timeout=60):
        return sh(["docker", "exec", self.cname(nm)] + args, timeout=timeout)

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
        return sh(["docker", "exec", self.cname(nm), "sh", "-c", inner], timeout=timeout)

    # -- faults

    def write_routes(self, nm):
        path = os.path.join(self.routes_dir(nm), "blackhole")
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            for other in sorted(self.blackhole[nm]):
                f.write(self.ip[other] + "\n")
        os.replace(tmp, path)

    def apply_routes(self, nm):
        self.write_routes(nm)
        if nm in self.down or nm in self.paused:
            return  # applied by the entry point at start, or after unpause
        self.exec(nm, ["/usr/local/bin/chaos-routes.sh"], timeout=20)

    def kill(self, nm):
        with self.mu:
            if nm in self.paused or nm in self.down:
                self.log("fault skip kill", nm, "(paused or down)")
                return False
            self.down.add(nm)
        sh(["docker", "kill", "-s", "KILL", self.cname(nm)])
        self.log("fault kill -9", nm)
        return True

    def start(self, nm):
        rc, out, _ = sh(["docker", "inspect", "-f", "{{.State.Running}} {{.State.ExitCode}}", self.cname(nm)])
        running, code = (out.split() + ["", ""])[:2]
        if running == "false":
            self.exits.append((nm, int(code or -1)))
        self.write_routes(nm)
        sh(["docker", "start", self.cname(nm)])
        with self.mu:
            self.down.discard(nm)
        self.log("fault start", nm, "(previous exit %s)" % code)

    def pause(self, nm):
        with self.mu:
            if nm in self.down or nm in self.paused:
                return False
            self.paused.add(nm)
        sh(["docker", "pause", self.cname(nm)])
        self.log("fault pause", nm)
        return True

    def unpause(self, nm):
        with self.mu:
            if nm not in self.paused:
                return
        sh(["docker", "unpause", self.cname(nm)])
        with self.mu:
            self.paused.discard(nm)
        self.apply_routes(nm)
        self.log("fault unpause", nm)

    def disconnect(self, nm):
        with self.mu:
            if nm in self.disconnected:
                return False
            self.disconnected.add(nm)
        sh(["docker", "network", "disconnect", "-f", self.net, self.cname(nm)])
        self.log("fault disconnect", nm)
        return True

    def connect(self, nm):
        with self.mu:
            if nm not in self.disconnected:
                return
        sh(["docker", "network", "connect", "--ip", self.ip[nm], self.net, self.cname(nm)])
        with self.mu:
            self.disconnected.discard(nm)
        self.log("fault connect", nm)

    def partition(self, side_a, side_b):
        for a in side_a:
            self.blackhole[a] |= set(side_b)
        for b in side_b:
            self.blackhole[b] |= set(side_a)
        for nm in self.names:
            self.apply_routes(nm)
        self.log("fault partition", "+".join(side_a), "|", "+".join(side_b))

    def heal_partition(self):
        for nm in self.names:
            self.blackhole[nm] = set()
            self.apply_routes(nm)
        self.log("fault heal partition")

    def isolate(self, nm):
        """Sabotage: cut nm off from every other node and leave it so."""
        for other in self.names:
            if other != nm:
                self.blackhole[nm].add(other)
                self.blackhole[other].add(nm)
        for x in self.names:
            self.apply_routes(x)
        self.log("SABOTAGE isolate", nm, "for the rest of the run")

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
        for nm in self.names:
            self.apply_routes(nm)
        self.log("healed everything" + (" except %s" % keep_isolated if keep_isolated else ""))


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
        except Exception as e:  # a fault that could not be applied is logged, not fatal
            cl.log("fault error", action, args, e)


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
        self.sends = []  # {node, kind, n, text, id?, ok, err, replaces?, reply_to?}
        self.drains = collections.defaultdict(list)  # node -> [entry...] from successful drains
        self.failed_drains = collections.Counter()
        self.acks = []  # {node, agent, id, handled}
        self.withdrawn = []  # {node, id, secret}
        self.replaced = []  # {node, id, text}

    def add(self, field, item):
        with self.mu:
            getattr(self, field).append(item)


def send(cl, book, nm, kind, n, extra="", reply_to=None, ask=False):
    text = "%s %s %d%s" % (kind, nm, n, (" " + extra) if extra else "")
    args = ["send", "--json", "--as", AGENT, "--room", ROOM]
    if reply_to:
        args += ["--reply-to", reply_to]
    if ask:
        args.append("--ask-handled")
    rc, res, err = cl.a2p2(nm, *(args + [text]), timeout=60)
    rec = {"node": nm, "kind": kind, "n": n, "text": text, "ok": rc == 0 and bool(res and res.get("id")),
           "id": (res or {}).get("id"), "err": "" if rc == 0 else err[-300:], "reply_to": reply_to}
    book.add("sends", rec)
    return rec


def drain(cl, book, nm, me):
    """Read the agent's inbox (and confirm it, as the CLI does), then report
    handled what may be: room-wide posts that asked, and replies to us."""
    rc, res, err = cl.a2p2(nm, "inbox", "--json", "--as", AGENT, "--room", ROOM, "--limit", "100", timeout=60)
    if rc != 0:
        # Refused before it ran (paused or stopped container): nothing was
        # shown or confirmed. Otherwise the read may have been confirmed
        # without the driver seeing it, so this node's inbox cannot be
        # checked for completeness.
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
        want = (m and m.group(1) == "a") or (me in (e.get("to") or []))
        if want:
            rc, r, err = cl.a2p2(nm, "ack", "--json", "--as", AGENT, e["id"])
            if rc == 0 and r and r.get("handled"):
                book.add("acks", {"node": nm, "agent": me, "id": e["id"], "handled": r["handled"]})
    return res


def workload(cl, book, nm, seed, duration, t0, stop, me):
    rng = random.Random("work-%s-%s" % (seed, nm))
    n = 0
    seen = []  # message ids this agent was shown, to reply to
    did_replace = did_withdraw = False
    while not stop.is_set() and now() - t0 < duration:
        time.sleep(rng.uniform(0.3, 1.5))
        frac = (now() - t0) / duration
        if not did_replace and frac > 0.3:
            did_replace = True
            n += 1
            r = send(cl, book, nm, "o", n, "original")
            if r["ok"]:
                time.sleep(rng.uniform(0.5, 3))
                rc, res, err = cl.a2p2(nm, "send", "--json", "--as", AGENT, "--room", ROOM, "--replaces", r["id"],
                                       "o %s %d replaced" % (nm, n))
                book.add("replaced", {"node": nm, "id": r["id"], "text": "o %s %d replaced" % (nm, n),
                                      "ok": rc == 0, "err": "" if rc == 0 else err[-300:]})
            continue
        if not did_withdraw and frac > 0.6:
            did_withdraw = True
            n += 1
            secret = "secret-%s-%08x" % (nm, rng.getrandbits(32))
            r = send(cl, book, nm, "s", n, secret)
            if r["ok"]:
                time.sleep(rng.uniform(0.5, 3))
                rc, res, err = cl.a2p2(nm, "send", "--json", "--as", AGENT, "--room", ROOM, "--replaces", r["id"])
                book.add("withdrawn", {"node": nm, "id": r["id"], "secret": secret, "ok": rc == 0,
                                       "err": "" if rc == 0 else err[-300:]})
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
            res = drain(cl, book, nm, me)
            for e in (res or {}).get("entries", []):
                if e.get("kind") == "message":
                    seen.append(e["id"])
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
    return (e.get("key"), e.get("version"), e.get("kind"), e.get("from"), e.get("text"),
            e.get("parent"), e.get("thread"), tuple(e.get("to") or []))


def settle(cl, book, me, timeout, log, skip=()):
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
            complete = all(expected <= k for k in keys.values())
            missing = {nm: len(expected - k) for nm, k in keys.items()}
            log("settling: same records %s, missing accepted %s" % (same, missing))
            if same and complete:
                return now() - t0
        time.sleep(5)
    return None


def snapshot(cl, book, me, room_id, log):
    snap = {"nodes": cl.names, "node_ids": cl.node, "agents": me, "room": room_id,
            "outbound": cl.outbound, "sends": book.sends, "acks": book.acks,
            "withdrawn": book.withdrawn, "replaced": book.replaced,
            "drains": dict(book.drains), "failed_drains": dict(book.failed_drains),
            "exits": cl.exits, "history": {}, "traces": {}, "logs": {}, "running": {}, "db_secrets": {},
            "waiting": {}}
    for nm in cl.names:  # a last read: everything delivered is shown
        res = drain(cl, book, nm, me[nm])
        while res and res.get("entries"):
            res = drain(cl, book, nm, me[nm])
        snap["waiting"][nm] = (res or {}).get("waiting", "read failed")
    snap["drains"], snap["failed_drains"], snap["acks"] = dict(book.drains), dict(book.failed_drains), book.acks
    for nm in cl.names:
        snap["history"][nm] = history(cl, nm)
        rc, out, err = sh(["docker", "logs", cl.cname(nm)])
        text = out + err
        with open(os.path.join(cl.out, "daemon-%s.log" % nm), "w") as f:
            f.write(text)
        snap["logs"][nm] = {"lines": text.count("\n"),
                            "panic": [l for l in text.splitlines() if "panic" in l or "fatal error" in l][:20],
                            "errors": sum(1 for l in text.splitlines() if '"level":"ERROR"' in l)}
        rc, out, _ = sh(["docker", "inspect", "-f", "{{.State.Running}}", cl.cname(nm)])
        snap["running"][nm] = out.strip() == "true"
        secrets = [w["secret"] for w in book.withdrawn if w["ok"]]
        if secrets:
            pat = "|".join(secrets)
            rc, out, _ = cl.exec(nm, ["sh", "-c", "grep -c -E %s /a2p2/a2p2.db* || true" % shlex.quote(pat)])
            snap["db_secrets"][nm] = out.strip()
    # Handled reports: wait a little for the last acks to travel.
    deadline = now() + 90
    while True:
        traces, short = {}, 0
        for a in book.acks:
            for nm in cl.names:
                rc, t, _ = cl.a2p2(nm, "trace", "--json", a["id"])
                traces["%s %s" % (nm, a["id"])] = t if rc == 0 else None
                agents = [h["agent"] for h in ((t or {}).get("handled") or [])]
                if a["agent"] not in agents:
                    short += 1
        if short == 0 or now() > deadline:
            break
        log("waiting for %d handled reports" % short)
        time.sleep(5)
    snap["traces"] = traces
    return snap


# --------------------------------------------------------------------------
# The checker


def check(snap):
    """Every violation found in the snapshot, as sentences."""
    v = []
    nodes = snap["nodes"]
    hist = snap["history"]
    for nm in nodes:
        if hist.get(nm) is None:
            v.append("%s: history could not be read" % nm)
    nodes_ok = [nm for nm in nodes if hist.get(nm) is not None]
    if not nodes_ok:
        return v
    # 1. The same records on every member.
    sets = {nm: {record_key(e) for e in hist[nm]} for nm in nodes_ok}
    union = set().union(*sets.values())
    for nm in nodes_ok:
        lack = union - sets[nm]
        if lack:
            v.append("%s lacks %d record(s) another member holds, e.g. %s" % (nm, len(lack), sorted(map(str, lack))[:3]))
    # 2. Exactly once per member.
    for nm in nodes_ok:
        c = collections.Counter(e["key"] for e in hist[nm])
        dup = [k for k, x in c.items() if x > 1]
        if dup:
            v.append("%s holds %d message(s) more than once: %s" % (nm, len(dup), dup[:3]))
    # 3. Nothing a sender accepted is missing anywhere.
    # A withdraw whose command failed may or may not have been written:
    # such a message is expected neither present nor absent.
    withdrawn = {w["id"] for w in snap["withdrawn"]}
    accepted = {s["id"]: s for s in snap["sends"] if s["ok"]}
    for nm in nodes_ok:
        keys = {e["key"] for e in hist[nm]}
        miss = sorted(set(accepted) - withdrawn - keys)
        if miss:
            v.append("%s is missing %d accepted message(s): %s" % (
                nm, len(miss), ", ".join("%s (%s)" % (m, accepted[m]["text"]) for m in miss[:5])))
    # 4. Each author's messages in the author's order: in history, and in
    # the order the inbox showed them (arrival); shown at most once.
    for nm in nodes_ok:
        last = {}
        for e in hist[nm]:
            m = TEXT_RE.match(e.get("text") or "")
            if not m:
                continue
            a, n = m.group(2), int(m.group(3))
            if a in last and n <= last[a]:
                v.append("%s: history shows %s's message %d after %d" % (nm, a, n, last[a]))
            last[a] = n
        for e in hist[nm]:
            if e.get("late"):
                v.append("%s: %s is marked late" % (nm, e["key"]))
    for nm, entries in snap["drains"].items():
        shown = collections.Counter()
        last = {}
        for e in entries:
            if e.get("kind") != "message":
                continue
            shown[e["key"]] += 1
            m = TEXT_RE.match(e.get("text") or "")
            if m:
                a, n = m.group(2), int(m.group(3))
                if a in last and n <= last[a]:
                    v.append("%s: inbox showed %s's message %d after %d" % (nm, a, n, last[a]))
                last[a] = max(n, last.get(a, 0))
            if e.get("late"):
                v.append("%s: inbox entry %s is marked late" % (nm, e["key"]))
        # A read whose command failed is not recorded, so even a crash
        # between showing and confirming (D3's exception) cannot put a
        # message here twice.
        dup = [k for k, x in shown.items() if x > 1]
        if dup:
            v.append("%s: inbox showed %d message(s) twice: %s" % (nm, len(dup), dup[:3]))
        # Every accepted message of another author was shown here, unless a
        # read failed partway (its entries may have been confirmed unseen by us).
        mine = snap["agents"][nm]
        to = {e["key"]: e.get("to") or [] for e in hist.get(nm) or []}
        others = {i for i, s in accepted.items() if s["node"] != nm and (not to.get(i) or mine in to[i])} - withdrawn
        unshown = others - set(shown)
        if unshown and not snap["failed_drains"].get(nm):
            v.append("%s: inbox never showed %d accepted message(s): %s" % (nm, len(unshown), sorted(unshown)[:3]))
    # 5. Every handled report arrived on every member.
    for a in snap["acks"]:
        for nm in nodes_ok:
            t = snap["traces"].get("%s %s" % (nm, a["id"]))
            agents = [h["agent"] for h in ((t or {}).get("handled") or [])]
            if a["agent"] not in agents:
                v.append("%s: no handled report by %s for %s" % (nm, a["agent"], a["id"]))
    # 6. Withdrawn text is gone; the replacement is what readers see.
    for w in snap["withdrawn"]:
        if not w["ok"]:
            continue
        for nm in nodes_ok:
            for e in hist[nm]:
                if e["key"] == w["id"] or w["secret"] in (e.get("text") or ""):
                    v.append("%s still shows withdrawn message %s" % (nm, w["id"]))
    for r in snap["replaced"]:
        if not r["ok"]:
            continue
        for nm in nodes_ok:
            got = [e.get("text") for e in hist[nm] if e["key"] == r["id"]]
            if got != [r["text"]]:
                v.append("%s shows %s as %r, not the replacement %r" % (nm, r["id"], got, r["text"]))
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
    return v


def summarize(snap, violations, extra):
    sends = snap["sends"]
    lines = ["a2p2 chaos run"]
    for k, val in extra.items():
        lines.append("%-18s %s" % (k, val))
    lines += [
        "%-18s %d accepted of %d tried" % ("sends", sum(1 for s in sends if s["ok"]), len(sends)),
        "%-18s %d" % ("handled reports", len(snap["acks"])),
        "%-18s %d" % ("replaced", sum(1 for r in snap["replaced"] if r["ok"])),
        "%-18s %d" % ("withdrawn", sum(1 for w in snap["withdrawn"] if w["ok"])),
        "%-18s %s" % ("records per node", {nm: len(h or []) for nm, h in snap["history"].items()}),
        "%-18s %s" % ("failed reads", snap["failed_drains"]),
        "%-18s %s" % ("daemon exits", collections.Counter(c for _, c in snap["exits"])),
        "%-18s %s" % ("error log lines", {nm: l["errors"] for nm, l in snap["logs"].items()}),
        "%-18s %s" % ("withdrawn bytes in db", snap.get("db_secrets") or "n/a"),
        "",
        "violations: %d" % len(violations),
    ]
    lines += ["  - " + x for x in violations]
    return "\n".join(lines) + "\n"


# --------------------------------------------------------------------------
# Commands


def build(log, out):
    rc, arch, err = sh(["docker", "version", "--format", "{{.Server.Arch}}"])
    if rc != 0:
        raise RuntimeError("docker is not available: " + err.strip())
    ctx = os.path.join(ROOT, ".chaos", "image")
    os.makedirs(ctx, exist_ok=True)
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch.strip())
    p = subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", os.path.join(ctx, "a2p2"), "./cmd/a2p2"],
                       cwd=ROOT, env=env, capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError("go build: " + p.stderr)
    for f in ("Dockerfile", "entry.sh", "routes.sh"):
        shutil.copy(os.path.join(HERE, f), ctx)
    sh(["docker", "build", "-q", "-t", IMAGE, ctx], check=True, timeout=600)
    rc, size, _ = sh(["docker", "image", "inspect", "-f", "{{.Size}}", IMAGE])
    log("image", IMAGE, "%.1f MB" % (int(size.strip() or 0) / 1e6), "linux/%s" % arch.strip())
    return int(size.strip() or 0)


def cmd_run(o):
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    run_id = "%s%x" % (stamp[-6:], os.getpid() % 4096)
    out = os.path.abspath(o.out or os.path.join(ROOT, ".chaos", "%s-seed%s-n%d" % (stamp, o.seed, o.nodes)))
    os.makedirs(out, exist_ok=True)
    log = Log(os.path.join(out, "run.log"))
    log("run", run_id, "seed", o.seed, "nodes", o.nodes, "duration", o.duration, "out", out)
    if o.nodes < 3:
        log("need at least 3 nodes")
        return 2
    cl = None
    try:
        size = build(log, out)
        cl = Cluster(run_id, o.nodes, out, log)
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
        cl.heal_all(keep_isolated=isolated)
        # After the heal, every node writes once more and must reach all.
        for nm in cl.names:
            for _ in range(30):
                if send(cl, book, nm, "f", counts.get(nm, 0) + 1, "final")["ok"]:
                    break
                time.sleep(1)
        took = settle(cl, book, me, o.settle, log)
        log("settled in %.0fs" % took if took is not None else "did not settle in %ds" % o.settle)
        snap = snapshot(cl, book, me, room_id, log)
        snap["meta"] = {"seed": o.seed, "nodes": o.nodes, "duration": o.duration, "settle": took,
                        "sabotage": o.sabotage, "image_bytes": size, "run_id": run_id}
        with open(os.path.join(out, "snapshot.json"), "w") as f:
            json.dump(snap, f, indent=1)
        violations = check(snap)
        if took is None:
            violations.insert(0, "the room did not settle within %ds of the heal" % o.settle)
        summary = summarize(snap, violations, {
            "seed": o.seed, "nodes": o.nodes, "duration": "%ss" % o.duration,
            "faults": dict(collections.Counter(a for _, a, _ in schedule)),
            "settled after": "%.0fs" % took if took is not None else "no",
            "image": "%.1f MB" % (size / 1e6), "sabotage": o.sabotage or "none", "out": out})
        with open(os.path.join(out, "summary.txt"), "w") as f:
            f.write(summary)
        print(summary)
        return 1 if violations else 0
    except Exception as e:
        log("RUN FAILED:", e)
        if cl:
            for nm in cl.names:
                rc, out_, err = sh(["docker", "logs", cl.cname(nm)])
                with open(os.path.join(out, "daemon-%s.log" % nm), "w") as f:
                    f.write(out_ + err)
        return 2
    finally:
        if cl and not o.keep:
            cl.destroy()
            log("removed the containers, volumes and network of run", run_id)


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
    violations = check(snap)
    print("violations: %d" % len(violations))
    for x in violations[:40]:
        print("  - " + x)
    if len(violations) > 40:
        print("  ... and %d more" % (len(violations) - 40))
    return 1 if violations else 0


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
    r.add_argument("--keep", action="store_true", help="leave the containers, volumes and network")
    c = sub.add_parser("check")
    c.add_argument("dir")
    c.add_argument("--drop", help="NODE:AUTHOR, delete AUTHOR's records from NODE's copy first")
    o = p.parse_args()
    sys.exit(cmd_run(o) if o.cmd == "run" else cmd_check(o))


if __name__ == "__main__":
    main()
