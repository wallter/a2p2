#!/usr/bin/env python3
"""Throughput and latency of a2p2 between machines: real daemons in Docker
containers on a private Docker network, links shaped with tc netem.
Python 3 standard library only. Run it with scripts/bench-net.sh.

    bench_net.py [--quick] [--only REGEX] [--out DIR] [--plain-daemon] [--keep]

Every daemon runs in its own container with its own home volume. A driver
container with no network mounts every home and drives the daemons through
their local sockets (internal/bench/netload), so one clock times a message
from the start of its send on one machine to the inbox call that returned
it on another. Links are shaped inside each daemon's container with
`tc qdisc ... netem` on eth0, so a one-way delay D is D in each direction
(a round trip of 2D) and a loss p is p in each direction. Only those
containers get a capability, NET_ADMIN, for tc.

The daemons are a2p2-netd by default: `a2p2 daemon` with its log at Debug,
so every request a listener serves is logged with its route and status;
the request and 429 counts come from those lines. --plain-daemon runs the
product's `a2p2 daemon` instead (no request counts).

Docker objects are made and removed as the chaos harness does (its Owned
class, scripts/chaos/chaos.py), under the prefix a2p2bnet and the label
a2p2bnet.run=<run id>: a name in use fails the run, and cleanup removes
only this run's objects, also on SIGINT and SIGTERM. Results go to DIR
(default .bench-net/<time>/): results.jsonl, results.md, run.log and
each daemon's log.
"""

import argparse
import collections
import datetime
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
sys.path.insert(0, os.path.join(ROOT, "scripts", "chaos"))
import chaos  # noqa: E402  the chaos harness: Owned, sh, Log, Interrupted

# This run's objects carry their own prefix and label, not the chaos run's.
chaos.PREFIX = "a2p2bnet"
chaos.LABEL = "a2p2bnet.run"
sh, Log, Interrupted, now = chaos.sh, chaos.Log, chaos.Interrupted, chaos.now
PORT = 7423

# --------------------------------------------------------------------------
# Image


def build(log, owned, out):
    rc, arch, err = sh(["docker", "version", "--format", "{{.Server.Arch}}"])
    if rc != 0:
        raise RuntimeError("docker is not available: " + err.strip())
    arch = arch.strip()
    ctx = os.path.join(out, "image")
    os.makedirs(ctx, exist_ok=True)
    try:
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
        for name, pkg, tags in (("a2p2", "./cmd/a2p2", []), ("a2p2-netd", "./internal/bench/netd", ["-tags", "bench"]),
                                ("a2p2-netload", "./internal/bench/netload", ["-tags", "bench"])):
            p = subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w"] + tags + ["-o", os.path.join(ctx, name), pkg],
                               cwd=ROOT, env=env, capture_output=True, text=True)
            if p.returncode != 0:
                raise RuntimeError("go build %s: %s" % (pkg, p.stderr))
        shutil.copy(os.path.join(HERE, "Dockerfile"), ctx)
        tag = "%s:%s" % (chaos.PREFIX, owned.run_id)
        owned._absent("image", tag)
        args = ["docker", "build", "-q", "--label", owned.label, "-t", tag]
        rc, o, e = sh(args + [ctx], timeout=900)
        if rc != 0:  # no network for the pinned base: the newest local alpine
            log("docker build with alpine:3.24 failed (%s); trying alpine:latest" % e.strip()[-200:])
            rc, o, e = sh(args + ["--build-arg", "BASE=alpine:latest", ctx], timeout=900)
        if rc != 0:
            raise RuntimeError("docker build: " + e.strip())
        owned.images.append(o.strip())
    finally:
        shutil.rmtree(ctx, ignore_errors=True)
    log("image", tag, "linux/%s" % arch)
    return tag


# --------------------------------------------------------------------------
# A topology: daemon containers, a driver, one room


class Topo:
    def __init__(self, owned, image, net, base, name, nodes, outbound, log, out, plain):
        self.owned, self.image, self.net, self.name, self.log, self.out = owned, image, net, name, log, out
        self.names = ["n%d" % (i + 1) for i in range(nodes)]
        self.outbound = set(outbound)
        self.ip = {nm: "%s.%d" % (base, 11 + i) for i, nm in enumerate(self.names)}
        self.cid, self.vol = {}, {}
        self.driver = None
        self.room = None
        self.nodes = {}  # name -> {name, home, node, agent} for the driver
        self.plain = plain
        self.log_lines = collections.Counter()  # name -> lines of its log already counted
        self.shaping = "none"

    def start(self):
        for nm in self.names:
            self.vol[nm] = self.owned.volume("%s-%s" % (self.name, nm))
            cmd = ["a2p2", "daemon"] if self.plain else ["a2p2-netd"]
            self.cid[nm] = self.owned.container([
                "--name", self.owned.name("%s-%s" % (self.name, nm)), "--hostname", nm, "--network", self.net,
                "--ip", self.ip[nm], "--cap-add", "NET_ADMIN", "-v", self.vol[nm] + ":/a2p2", self.image] + cmd)
        mounts = []
        for nm in self.names:
            mounts += ["-v", "%s:/homes/%s" % (self.vol[nm], nm)]
        self.driver = self.owned.container(["--name", self.owned.name(self.name + "-driver"), "--network", "none"]
                                           + mounts + [self.image, "sleep", "100000"])
        spec = {"room": "bench", "nodes": [{"name": nm, "home": "/homes/" + nm,
                                            "listen": "" if nm in self.outbound else "%s:%d" % (self.ip[nm], PORT)}
                                           for nm in self.names]}
        t0 = now()
        res = self.drive("setup", spec, timeout=600)
        self.room = res["room"]
        self.nodes = {nm: {"name": nm, "home": n["home"], "node": n["node"], "agent": n["agent"]}
                      for nm, n in res["nodes"].items()}
        self.log("topology", self.name, "formed in %.0fs:" % (now() - t0),
                 ", ".join("%s %s %s" % (nm, self.ip[nm], "outbound only" if nm in self.outbound else "listening")
                           for nm in self.names), "joins", res.get("join_secs"))

    def drive(self, cmd, spec, timeout):
        rc, o, e = sh(["docker", "exec", "-i", self.driver, "a2p2-netload", cmd], timeout=timeout,
                      input=json.dumps(spec))
        if rc != 0:
            raise RuntimeError("a2p2-netload %s: rc %s %s" % (cmd, rc, (e or o).strip()[-500:]))
        return json.loads(o)

    def exec(self, nm, args, timeout=30):
        return sh(["docker", "exec", self.cid[nm]] + args, timeout=timeout)

    # -- shaping

    def shape(self, netem):
        """netem is the argument list after `netem`, or None for no shaping;
        applied to eth0 of every daemon container and read back."""
        for nm in self.names:
            if netem:
                rc, _, err = self.exec(nm, ["tc", "qdisc", "replace", "dev", "eth0", "root", "netem"] + netem.split())
                if rc != 0:
                    raise RuntimeError("tc on %s: %s" % (nm, err.strip()))
            else:
                self.exec(nm, ["tc", "qdisc", "del", "dev", "eth0", "root"])
            rc, o, _ = self.exec(nm, ["tc", "qdisc", "show", "dev", "eth0"])
            if (netem and "netem" not in o) or (not netem and "netem" in o):
                raise RuntimeError("shaping on %s not applied: %s" % (nm, o.strip()))
        self.shaping = netem or "none"

    # -- counters

    def counters(self):
        c = {}
        for nm in self.names:
            rc, o, _ = self.exec(nm, ["sh", "-c", "cat /sys/fs/cgroup/cpu.stat; cat /proc/net/snmp; "
                                                  "grep -E '^Vm(RSS|HWM)' /proc/1/status; "
                                                  "echo tx_bytes $(cat /sys/class/net/eth0/statistics/tx_bytes)"])
            d = {}
            lines = o.splitlines()
            for l in lines:
                f = l.split()
                if len(f) == 2 and f[0] == "usage_usec":
                    d["cpu_s"] = int(f[1]) / 1e6
                if len(f) == 2 and f[0] == "tx_bytes" and f[1].isdigit():
                    d["tx_bytes"] = int(f[1])
                if f and f[0] in ("VmRSS:", "VmHWM:"):
                    d[f[0][:-1]] = int(f[1]) / 1024  # MiB
            tcp = [l.split() for l in lines if l.startswith("Tcp:")]
            if len(tcp) == 2:
                t = dict(zip(tcp[0][1:], tcp[1][1:]))
                for k in ("ActiveOpens", "PassiveOpens", "RetransSegs", "OutSegs", "InSegs", "EstabResets"):
                    d[k] = int(t.get(k, 0))
            c[nm] = d
        return c

    def requests(self):
        """Peer requests each listener logged since the last call, by route
        and status (a2p2-netd logs every one at Debug)."""
        got = {}
        for nm in self.names:
            rc, o, e = sh(["docker", "logs", self.cid[nm]], timeout=120)
            lines = (o + e).splitlines()
            new = lines[self.log_lines[nm]:]
            self.log_lines[nm] = len(lines)
            cnt = collections.Counter()
            for l in new:
                if '"peer request"' not in l and '"peer connection refused"' not in l:
                    continue
                try:
                    j = json.loads(l)
                except ValueError:
                    continue
                if j.get("msg") == "peer connection refused":
                    cnt["refused"] += 1
                    continue
                route = j.get("route", "")
                route = "pull" if route.endswith("/pull") else "join" if "join" in route else \
                    "files" if "file" in route or "blob" in route else route
                cnt["%s %s" % (route, j.get("status"))] += 1
            got[nm] = cnt
        return got

    def save_logs(self):
        for nm in self.names:
            rc, o, e = sh(["docker", "logs", self.cid[nm]], timeout=120)
            with open(os.path.join(self.out, "daemon-%s-%s.log" % (self.name, nm)), "w") as f:
                f.write(o + e)

    def stop(self):
        """Removes this topology's containers and volumes (all of them this
        run's own) to free the machine for the next."""
        self.save_logs()
        for c in [self.driver] + list(self.cid.values()):
            if c:
                sh(["docker", "rm", "-f", c], timeout=120)
                if c in self.owned.containers:
                    self.owned.containers.remove(c)
        for v in self.vol.values():
            rc, _, _ = sh(["docker", "volume", "rm", v], timeout=120)
            if rc == 0 and v in self.owned.volumes:
                self.owned.volumes.remove(v)


class Sampler:
    """Samples every daemon's resident memory each second during a case."""

    def __init__(self, topo):
        self.topo, self.stop_ev, self.peak = topo, threading.Event(), collections.Counter()
        self.t = threading.Thread(target=self.run, daemon=True)

    def run(self):
        while not self.stop_ev.is_set():
            for nm in self.topo.names:
                rc, o, _ = self.topo.exec(nm, ["grep", "VmRSS", "/proc/1/status"], timeout=10)
                f = o.split()
                if rc == 0 and len(f) >= 2:
                    self.peak[nm] = max(self.peak[nm], int(f[1]) / 1024)
            self.stop_ev.wait(1)

    def __enter__(self):
        self.t.start()
        return self

    def __exit__(self, *a):
        self.stop_ev.set()
        self.t.join(timeout=20)


# --------------------------------------------------------------------------
# Cases


class Bench:
    def __init__(self, o, log, out):
        self.o, self.log, self.out = o, log, out
        self.rows = []
        self.res = open(os.path.join(out, "results.jsonl"), "a", buffering=1)
        self.only = re.compile(o.only) if o.only else None

    def wanted(self, section):
        return not self.only or bool(self.only.search(section))

    def case(self, topo, section, name, spec, timeout, file=False):
        """Runs one case on topo with its current shaping; records the row."""
        if not self.wanted(section):
            return None
        spec = dict(spec, room=topo.room, nodes=topo.nodes, tag="%s%d" % (re.sub(r"\W", "", name)[:12], len(self.rows)))
        topo.requests()  # what came before is not this case's
        before = topo.counters()
        t0 = now()
        self.log("case", section, "|", name, "|", topo.name, "| shaping:", topo.shaping)
        err = None
        with Sampler(topo) as smp:
            try:
                r = topo.drive("file" if file else "run", spec, timeout=timeout + 120)
            except Exception as e:  # a case that breaks is reported, not fatal
                r, err = {}, str(e)
        wall = now() - t0
        after = topo.counters()
        reqs = topo.requests()
        row = {"section": section, "case": name, "topo": topo.name, "shaping": topo.shaping, "wall_s": round(wall, 1),
               "result": r, "error": err, "requests": {nm: dict(c) for nm, c in reqs.items()},
               "cpu_s": {nm: round(after[nm].get("cpu_s", 0) - before[nm].get("cpu_s", 0), 2) for nm in topo.names},
               "rss_peak_mib": {nm: round(smp.peak[nm], 1) for nm in topo.names},
               "tcp": {nm: {k: after[nm].get(k, 0) - before[nm].get(k, 0)
                            for k in ("ActiveOpens", "PassiveOpens", "RetransSegs", "OutSegs", "EstabResets")}
                       for nm in topo.names}}
        if not file:  # messages meant to be sent, per sender
            row["planned"] = spec["count"] if spec["mode"] != "rate" else int(spec["rate"] * spec["duration"])
            row["planned"] *= len(spec["senders"])
        tot = collections.Counter()
        for c in reqs.values():
            tot.update(c)
        row["pull_requests"] = sum(v for k, v in tot.items() if k.startswith("pull "))
        row["file_requests"] = sum(v for k, v in tot.items() if k.startswith("files "))
        row["http_429"] = sum(v for k, v in tot.items() if k.endswith(" 429"))
        row["retrans"] = sum(t["RetransSegs"] for t in row["tcp"].values())
        row["new_conns"] = sum(t["ActiveOpens"] for t in row["tcp"].values())
        # Bytes every daemon put on the private network (eth0), both ways.
        row["wire_bytes"] = sum(after[nm].get("tx_bytes", 0) - before[nm].get("tx_bytes", 0) for nm in topo.names)
        self.rows.append(row)
        self.res.write(json.dumps(row) + "\n")
        self.log("  ->", line(row))
        # Let what a timed-out case left in flight arrive before the next.
        if not file and not r.get("complete", True):
            d = topo.drive("drain", {"room": topo.room, "nodes": topo.nodes, "quiet": 5, "max": 45}, timeout=120)
            self.log("  drained %d late message(s) in %.0fs%s" % (d["read"], d["secs"], "" if d["quiet"] else
                                                                  " (still arriving: the next case shares the link)"))
            row["late_after_case"] = d
        return row


def fmt_ms(v):
    return "-" if v is None else "%.0f" % v if v >= 10 else "%.1f" % v


def line(row):
    r = row["result"]
    if row["error"]:
        return "ERROR " + row["error"][:200]
    if "mb_per_s" in r:
        return "file %.0f MB: %.1f MB/s, verified copy after %.1fs (%s)" % (
            r.get("size", 0) / 1e6, r.get("mb_per_s", 0), r.get("fetched_secs", 0), r.get("state"))
    lat = r.get("latency", {})
    return "%d/%d delivered in %.1fs, %.1f msg/s, p50 %s p95 %s p99 %s ms, pull req %d, 429 %d%s" % (
        r.get("delivered", 0), r.get("expected", 0), r.get("secs", 0), r.get("msgs_per_s", 0), fmt_ms(lat.get("p50_ms")),
        fmt_ms(lat.get("p95_ms")), fmt_ms(lat.get("p99_ms")), row["pull_requests"], row["http_429"],
        " TIMED OUT" if r.get("timed_out") else "")


def table(rows, title):
    """A markdown table of message cases."""
    out = ["### " + title, "",
           "| case | link | delivered | msgs/s | MB/s | p50 ms | p95 ms | p99 ms | secs | pull req/msg | wire B/msg | 429 | retrans | new conns | CPU s per daemon | note |",
           "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for row in rows:
        r = row["result"]
        if row["error"]:
            out.append("| %s | %s | error | | | | | | | | | | | | | %s |" % (row["case"], row["shaping"], row["error"][:120]))
            continue
        lat = r.get("latency", {})
        dl = r.get("delivered", 0)
        rate = r.get("msgs_per_s", 0)
        note = []
        if r.get("timed_out"):
            note.append("timed out: %d of %d planned sent" % (r.get("sent", 0), row.get("planned", 0)))
        if r.get("size") and r.get("size") != row.get("size_asked", r.get("size")):
            note.append("text %d B" % r["size"])
        if r.get("mode") == "rate":
            note.append("window %.1f msg/s, backlog at end %d, max %d" % (
                r.get("msgs_per_s_window", 0), r.get("backlog_at_send_end", 0), r.get("backlog_max", 0)))
            if r.get("send_lag_max_ms", 0) > 1000:
                note.append("sender lagged %.1fs" % (r["send_lag_max_ms"] / 1000))
        if r.get("send_errors"):
            note.append("%d send errors" % r["send_errors"])
        late = row.get("late_after_case")
        if late:
            note.append("%d arrived after the timeout" % late["read"])
        out.append("| %s | %s | %d/%d | %.1f | %.3f | %s | %s | %s | %.1f | %s | %s | %d | %d | %d | %s | %s |" % (
            row["case"], row["shaping"], dl, r.get("expected", 0), rate, rate * r.get("size", 0) / 1e6,
            fmt_ms(lat.get("p50_ms")), fmt_ms(lat.get("p95_ms")), fmt_ms(lat.get("p99_ms")), r.get("secs", 0),
            "%.2f" % (row["pull_requests"] / dl) if dl and row["pull_requests"] else "-",
            "%.0f" % (row["wire_bytes"] / dl) if dl and row.get("wire_bytes") else "-",
            row["http_429"], row["retrans"], row["new_conns"],
            " ".join("%s %.1f" % (nm, v) for nm, v in row["cpu_s"].items()), "; ".join(note)))
    return "\n".join(out) + "\n"


def file_table(rows, title):
    out = ["### " + title, "",
           "| case | link | size MB | send call s | approve s | visible s | verified copy s | MB/s | file req | 429 | retrans | CPU s per daemon | state |",
           "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for row in rows:
        r = row["result"]
        if row["error"]:
            out.append("| %s | %s | error | | | | | | | | | | %s |" % (row["case"], row["shaping"], row["error"][:120]))
            continue
        out.append("| %s | %s | %.0f | %.2f | %.2f | %.2f | %.1f | %.2f | %d | %d | %d | %s | %s%s |" % (
            row["case"], row["shaping"], r.get("size", 0) / 1e6, r.get("send_call_secs", 0), r.get("confirm_secs", 0),
            r.get("visible_secs", 0), r.get("fetched_secs", 0), r.get("mb_per_s", 0), row["file_requests"],
            row["http_429"], row["retrans"], " ".join("%s %.1f" % (nm, v) for nm, v in row["cpu_s"].items()),
            r.get("state", ""), ", verified" if r.get("verified") else ", NOT verified here"))
    return "\n".join(out) + "\n"


# --------------------------------------------------------------------------
# The plan


LINKS = [  # (name, netem arguments): harsh last, so a stalled case delays nothing after it
    ("10 ms", "delay 10ms limit 10000"),
    ("50 ms", "delay 50ms limit 10000"),
    ("50 ms +-10 ms jitter", "delay 50ms 10ms distribution normal limit 10000"),
    ("150 ms", "delay 150ms limit 10000"),
    ("10 Mbit/s", "rate 10mbit limit 10000"),
    ("1 Mbit/s", "rate 1mbit limit 10000"),
    ("50 ms, 1% loss", "delay 50ms loss 1% limit 10000"),
    ("50 ms, 5% loss", "delay 50ms loss 5% limit 10000"),
    ("400 ms", "delay 400ms limit 10000"),
]


def plan(o):
    """Sizes and timeouts of every case: the full run, or --quick."""
    if o.quick:
        return dict(serial=100, serial_t=90, burst=500, burst_t=90, rates=[10], rate_s=15, drain=20,
                    base_runs=1, links=[LINKS[1]], link_burst=300, link_burst_t=60, link_rate_s=15,
                    big_burst=200, big_burst_t=60, big_rates=[10], dir_burst=200, dir_burst_t=45, dir_rate_s=15,
                    members=[], files=[(10, None)], file_t=180)
    return dict(serial=1000, serial_t=240, burst=5000, burst_t=240, rates=[1, 10, 50, 200], rate_s=60, drain=30,
                base_runs=2, links=LINKS, link_burst=5000, link_burst_t=90, link_rate_s=30,
                big_burst=2000, big_burst_t=120, big_rates=[10, 50, 200], dir_burst=2000, dir_burst_t=90, dir_rate_s=30,
                members=[3, 5, "star"], mem_burst=2000, mem_burst_t=150, mem_rate_s=30,
                files=[(10, None), (100, None), (10, "delay 50ms rate 10mbit limit 10000"),
                       (100, "delay 50ms rate 10mbit limit 10000")], file_t=420)


def run_all(o, log, owned, out, image, net, base):
    p = plan(o)
    b = Bench(o, log, out)
    topos = []

    def topo(name, n, outbound=()):
        t = Topo(owned, image, net, base, name, n, outbound, log, out, o.plain_daemon)
        topos.append(t)
        t.start()
        return t

    def message_cases(t, section, prefix, sender, receivers, burst, burst_t, rates, rate_s, size=200, serial=None):
        if serial:
            b.case(t, section, prefix + "serial %d" % serial[0], {"mode": "serial", "senders": [sender], "receivers": receivers,
                                                                 "count": serial[0], "size": size, "timeout": serial[1]}, serial[1])
        if burst:
            b.case(t, section, prefix + "burst %d" % burst, {"mode": "burst", "senders": [sender], "receivers": receivers,
                                                             "count": burst, "size": size, "timeout": burst_t}, burst_t)
        for rate in rates:
            b.case(t, section, prefix + "%g/s for %ds" % (rate, rate_s),
                   {"mode": "rate", "senders": [sender], "receivers": receivers, "rate": rate, "duration": rate_s,
                    "size": size, "timeout": p["drain"], "timeline": True}, rate_s + p["drain"])

    # 1, 2, 4, 6: two machines, both listening.
    want = b.wanted
    if any(want(s) for s in ("baseline", "link", "size", "files")):
        t = topo("pair", 2)
        try:
            for run in range(1, p["base_runs"] + 1):
                # The second run, for the spread, leaves out 1/s and 50/s.
                rates = p["rates"] if run == 1 else [r for r in p["rates"] if r not in (1, 50)] or p["rates"]
                message_cases(t, "baseline", "run %d: " % run, "n1", ["n2"], p["burst"], p["burst_t"], rates, p["rate_s"],
                              serial=(p["serial"], p["serial_t"]))
            sz = 16384
            message_cases(t, "size", "16 KB: ", "n1", ["n2"], p["big_burst"], p["big_burst_t"], p["big_rates"],
                          p["link_rate_s"], size=sz)
            for name, netem in p["links"] if want("link") else []:
                t.shape(netem)
                message_cases(t, "link", "", "n1", ["n2"], p["link_burst"], p["link_burst_t"], [10], p["link_rate_s"])
            t.shape(None)
            for mb, netem in p["files"] if want("files") else []:
                t.shape(netem)
                # New random bytes for every case: a receiver that holds a
                # file's SHA-256 already would copy nothing over the link.
                path = "/tmp/f%d-%d" % (mb, len(b.rows))
                t.exec("n1", ["sh", "-c", "head -c %d /dev/urandom > %s" % (mb * 1000000, path)], timeout=120)
                b.case(t, "files", "%d MB" % mb, {"from": "n1", "to": "n2", "path": path, "timeout": p["file_t"]},
                       p["file_t"], file=True)
                t.exec("n1", ["rm", "-f", path])
            t.shape(None)
        finally:
            t.stop()
    # 3: direction and reachability. n1 listens, n2 connects out only.
    if want("direction"):
        t = topo("dir", 2, outbound=["n2"])
        try:
            message_cases(t, "direction", "sender listens, receiver outbound only: ", "n1", ["n2"], p["dir_burst"],
                          p["dir_burst_t"], [10], p["dir_rate_s"], serial=(min(200, p["serial"]), 90))
            message_cases(t, "direction", "sender outbound only, receiver listens: ", "n2", ["n1"], p["dir_burst"],
                          p["dir_burst_t"], [10], p["dir_rate_s"], serial=(min(200, p["serial"]), 90))
        finally:
            t.stop()
    # 5: members.
    for m in p["members"]:
        if not want("members"):
            break
        n, star = (5, True) if m == "star" else (m, False)
        name = "star5" if star else "mesh%d" % n
        t = topo(name, n, outbound=["n%d" % i for i in range(2, n + 1)] if star else ())
        everyone = t.names
        try:
            label = "5, star through n1: " if star else "%d, all listening: " % n
            b.case(t, "members", label + "n1 burst %d" % p["mem_burst"],
                   {"mode": "burst", "senders": ["n1"], "receivers": everyone, "count": p["mem_burst"], "size": 200,
                    "timeout": p["mem_burst_t"]}, p["mem_burst_t"])
            if star:
                b.case(t, "members", label + "leaf n2 burst %d" % p["mem_burst"],
                       {"mode": "burst", "senders": ["n2"], "receivers": everyone, "count": p["mem_burst"], "size": 200,
                        "timeout": p["mem_burst_t"]}, p["mem_burst_t"])
            b.case(t, "members", label + "every machine 10/s for %ds" % p["mem_rate_s"],
                   {"mode": "rate", "senders": everyone, "receivers": everyone, "rate": 10, "duration": p["mem_rate_s"],
                    "size": 200, "timeout": p["drain"], "timeline": True}, p["mem_rate_s"] + p["drain"])
        finally:
            t.stop()
    return b


def report(b, out, extra):
    secs = collections.OrderedDict()
    for r in b.rows:
        secs.setdefault(r["section"], []).append(r)
    md = ["# a2p2 network benchmark", ""] + ["- %s: %s" % kv for kv in extra.items()] + [""]
    titles = {"baseline": "1. Baseline: two machines, no shaping, 200 B", "size": "4. Size: 16 KB messages, no shaping",
              "link": "2. Link quality (one-way delay and loss in each direction; rate on each side's egress)",
              "files": "6. Files", "direction": "3. Direction and reachability (n1 listens, n2 outbound only)",
              "members": "5. Members"}
    for s in ("baseline", "link", "direction", "size", "members", "files"):
        if s in secs:
            md.append((file_table if s == "files" else table)(secs[s], titles[s]))
            if s == "members":
                md.append("Peak resident memory (MiB) per daemon during each members case:\n")
                for r in secs[s]:
                    md.append("- %s: %s" % (r["case"], ", ".join("%s %.0f" % kv for kv in r["rss_peak_mib"].items())))
                md.append("")
    text = "\n".join(md)
    with open(os.path.join(out, "results.md"), "w") as f:
        f.write(text)
    return text


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--quick", action="store_true", help="the smallest cases, about five minutes")
    ap.add_argument("--only", help="only the sections matching this regex: baseline, link, size, files, direction, members")
    ap.add_argument("--out")
    ap.add_argument("--plain-daemon", action="store_true", help="run `a2p2 daemon` (no request counts)")
    ap.add_argument("--keep", action="store_true", help="leave this run's Docker objects and print them")
    o = ap.parse_args()
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    run_id = os.environ.get("A2P2_BENCH_NET_RUN") or secrets.token_hex(4)
    out = os.path.abspath(o.out or os.path.join(ROOT, ".bench-net", stamp + ("-quick" if o.quick else "")))
    os.makedirs(out, exist_ok=True)
    log = Log(os.path.join(out, "run.log"))
    log("run", run_id, "quick" if o.quick else "full", "out", out)
    owned = chaos.Owned(run_id, log)
    cleaning = threading.Event()

    def on_signal(signum, frame):
        if cleaning.is_set():
            return
        raise Interrupted("signal %d" % signum)

    import signal
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)
    status, t0 = 2, now()
    b = None
    try:
        image = build(log, owned, out)
        net = base = None
        for third in range(40, 120):
            subnet = "10.214.%d.0/24" % third
            net, err = owned.network(subnet)
            if net:
                base = "10.214.%d" % third
                break
            if "overlap" not in err and "Pool" not in err:
                raise RuntimeError("docker network create: " + err.strip())
        if not net:
            raise RuntimeError("no free 10.214.x.0/24 subnet")
        log("network", net, base + ".0/24")
        b = run_all(o, log, owned, out, image, net, base)
        status = 0 if b.rows and not any(r["error"] for r in b.rows) else 1
    except Interrupted as e:
        log("INTERRUPTED (%s): cleaning up" % e)
    except Exception as e:
        log("RUN FAILED:", e)
    finally:
        cleaning.set()
        if o.keep:
            log("--keep: left in Docker:", json.dumps(owned.describe()))
        else:
            failures = owned.remove()
            for x in failures:
                log("CLEANUP FAILED:", x)
            if failures:
                status = 2
            else:
                log("removed everything run", run_id, "created")
    if b and b.rows:
        rc, cpus, _ = sh(["docker", "info", "--format", "{{.NCPU}} CPUs, {{.MemTotal}} bytes, kernel {{.KernelVersion}}, {{.OperatingSystem}}"])
        text = report(b, out, collections.OrderedDict([
            ("run", "%s (%s), %.0f minutes" % (run_id, "quick" if o.quick else "full", (now() - t0) / 60)),
            ("docker", cpus.strip()),
            ("daemon", "a2p2 daemon" if o.plain_daemon else "a2p2-netd (a2p2 daemon, log at Debug)"),
            ("results", out)]))
        print(text)
    log("exit status", status, "after %.0fs" % (now() - t0))
    return status


if __name__ == "__main__":
    sys.exit(main())
