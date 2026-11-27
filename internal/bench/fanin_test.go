//go:build bench

package bench

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// M8: fan-in. N workers each send one 200 byte question to one
// orchestrator and wait, in a waiting inbox, for its answer; the
// orchestrator waits in its inbox, answers each message in its thread and
// confirms what it read. Everyone is driven through the localapi client
// the MCP shim uses. A model's thinking time is not simulated: the
// orchestrator answers at once, unless A2P2_BENCH_M8_DELAY (a Go duration,
// default 0) sets a fixed delay before each reply.

// fanDelay is the fixed orchestrator delay per reply from the environment.
func fanDelay(t testing.TB) time.Duration {
	s := os.Getenv("A2P2_BENCH_M8_DELAY")
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		t.Fatalf("A2P2_BENCH_M8_DELAY=%q is not a duration such as 100ms", s)
	}
	return d
}

type fanCfg struct {
	cs      string
	n       int           // workers
	rounds  int           // every worker asks once per round; rounds are synchronised
	orch    *proc         // the daemon of the orchestrator
	work    *proc         // the daemon of the workers (the same, or the other machine)
	rooms   []string      // one room per worker (index i mod len); a single entry is the one room
	orchIn  string        // the room the orchestrator reads ("" every room)
	wide    bool          // questions posted room-wide, workers read at level all
	delay   time.Duration // the orchestrator's delay before each reply
	limit   time.Duration // give up after this long
	showRnk bool          // print the p50 of the 1st, 2nd, ... answer of a round
}

// batchLat stores a count as a duration of that many milliseconds, so that
// result prints the count's percentiles.
func batchLat(l *lat, n int) { l.add(time.Duration(n) * time.Millisecond) }

type fanStats struct {
	asked, answered int64
}

// runFanIn runs one configuration and prints its rows.
func runFanIn(t *testing.T, c fanCfg) fanStats {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), c.limit)
	defer cancel()
	orchID := c.orch.agent("orch")
	oc := c.orch.client()
	level := "addressed"
	if c.wide {
		level = "all"
	}
	ids := make([]string, c.n)
	cls := make([]*localapi.Client, c.n)
	for i := range ids {
		ids[i] = c.work.agent(fmt.Sprintf("w%d", i))
		cls[i] = c.work.client()
		// through the worker's own client, which so opens its connection
		// before the rounds (200 at once can overrun the listen backlog)
		call[localapi.LevelResp](t, cls[i], "level", localapi.LevelReq{Agent: ids[i], Level: level})
	}
	roomOf := func(i int) string { return c.rooms[i%len(c.rooms)] }

	var oInbox, oInbox2, oBatch, oReply lat
	var errMu sync.Mutex
	var firstErr error
	fail := func(err error) {
		if ctx.Err() != nil {
			return // the limit ended the run: reported as unanswered
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	// the orchestrator
	odone := make(chan struct{})
	go func() {
		defer close(odone)
		for ctx.Err() == nil {
			t0 := time.Now()
			var in localapi.InboxResp
			if err := oc.Call(ctx, "inbox", localapi.InboxReq{Agent: orchID, Room: c.orchIn, Limit: node.MaxInboxLimit, WaitMS: 1000}, &in); err != nil {
				fail(fmt.Errorf("orchestrator inbox: %w", err))
				return
			}
			d := time.Since(t0)
			var qs []localapi.Entry
			for _, e := range in.Entries {
				if e.Kind == node.EntryMessage {
					qs = append(qs, e)
				}
			}
			if len(qs) > 0 {
				oInbox.add(d)
				if len(qs) > 1 {
					oInbox2.add(d)
				}
				batchLat(&oBatch, len(qs))
			}
			for _, e := range qs {
				if c.delay > 0 {
					time.Sleep(c.delay)
				}
				r0 := time.Now()
				// no to: a reply goes to the parent's author
				if err := oc.Call(ctx, "send", localapi.SendReq{Agent: orchID, Text: text(200), ReplyTo: e.ID}, nil); err != nil {
					fail(fmt.Errorf("orchestrator reply: %w", err))
					return
				}
				oReply.add(time.Since(r0))
			}
			if err := seen(oc, orchID, in.Entries); err != nil {
				fail(fmt.Errorf("orchestrator seen: %w", err))
				return
			}
		}
	}()

	// the workers
	var wSend, wInbox, wEntries, wAnswer lat
	var calls, entries atomic.Int64
	var asked, answered atomic.Int64
	starts := make([]chan struct{}, c.n)
	var wg sync.WaitGroup
	roundLat := make([]time.Duration, c.n)
	for i := range starts {
		starts[i] = make(chan struct{}, 1)
		go func() {
			for range starts[i] {
				roundLat[i] = -1
				func() {
					defer wg.Done()
					req := localapi.SendReq{Agent: ids[i], Room: roomOf(i), Text: text(200)}
					if !c.wide {
						req.To = []string{orchID}
					}
					t0 := time.Now()
					var m localapi.SendResp
					if err := cls[i].Call(ctx, "send", req, &m); err != nil {
						fail(fmt.Errorf("worker send: %w", err))
						return
					}
					wSend.add(time.Since(t0))
					asked.Add(1)
					for ctx.Err() == nil {
						i0 := time.Now()
						var in localapi.InboxResp
						if err := cls[i].Call(ctx, "inbox", localapi.InboxReq{Agent: ids[i], Room: roomOf(i), Limit: node.MaxInboxLimit, WaitMS: 5000}, &in); err != nil {
							fail(fmt.Errorf("worker inbox: %w", err))
							return
						}
						now := time.Now()
						if len(in.Entries) > 0 {
							wInbox.add(now.Sub(i0))
							batchLat(&wEntries, len(in.Entries))
							calls.Add(1)
							entries.Add(int64(len(in.Entries)))
						}
						got := false
						for _, e := range in.Entries {
							if e.Kind == node.EntryMessage && e.Parent == m.ID {
								got = true
							}
						}
						if got {
							roundLat[i] = now.Sub(t0)
							wAnswer.add(roundLat[i])
							answered.Add(1)
						}
						if err := seen(cls[i], ids[i], in.Entries); err != nil {
							fail(fmt.Errorf("worker seen: %w", err))
							return
						}
						if got {
							return
						}
					}
				}()
			}
		}()
	}
	ranks := make([]lat, c.n)
	start := time.Now()
	for r := 0; r < c.rounds && ctx.Err() == nil; r++ {
		wg.Add(c.n)
		for i := range starts {
			starts[i] <- struct{}{}
		}
		wg.Wait()
		var got []time.Duration
		for _, d := range roundLat {
			if d >= 0 {
				got = append(got, d)
			}
		}
		slices.Sort(got)
		for k, d := range got {
			ranks[k].add(d)
		}
	}
	el := time.Since(start)
	timedOut := ctx.Err() != nil && firstErr == nil
	cancel()
	<-odone
	for _, ch := range starts {
		close(ch)
	}
	if firstErr != nil {
		t.Fatalf("%s: %v", c.cs, firstErr)
	}

	total := int64(c.n * c.rounds)
	st := fanStats{asked: asked.Load(), answered: answered.Load()}
	if timedOut || st.answered < total {
		note("M8 %s: %d of %d questions answered within the %s limit (%d asked)", c.cs, st.answered, total, c.limit, st.asked)
	}
	r := func(what string, l *lat, e time.Duration, unit string) {
		if len(l.d) > 0 {
			result(t, what, c.cs, l, e, unit)
		}
	}
	r("M8 question->answer", &wAnswer, el, "answered/s")
	r("M8 worker send call", &wSend, 0, "-")
	r("M8 worker inbox call", &wInbox, 0, "-")
	r("M8 worker inbox entries", &wEntries, 0, "entries/call (n=calls)")
	r("M8 orch inbox call", &oInbox, 0, "-")
	r("M8 orch inbox call batch>=2", &oInbox2, 0, "-")
	r("M8 orch inbox entries", &oBatch, 0, "messages/call (n=calls)")
	r("M8 orch reply send", &oReply, 0, "-")
	if st.answered > 0 {
		note("M8 %s: worker inbox calls per question %.2f, entries returned to a worker per question %.1f",
			c.cs, float64(calls.Load())/float64(st.answered), float64(entries.Load())/float64(st.answered))
	}
	if c.showRnk {
		var sb strings.Builder
		for k := range ranks {
			if len(ranks[k].d) > 0 {
				slices.Sort(ranks[k].d)
				fmt.Fprintf(&sb, " %d:%.0f", k+1, float64(ranks[k].pct(.5).Microseconds())/1000)
			}
		}
		note("M8 %s: p50 ms of the k-th answer of a round (k:ms):%s", c.cs, sb.String())
	}
	return st
}

// fanRounds gives about 2,000 questions (800 at N=200, where one answer
// takes seconds and 2,000 would not fit the limit). A2P2_BENCH_M8_ROUNDS
// sets the rounds instead (a short run to profile).
func fanRounds(n int) int {
	if r, err := strconv.Atoi(os.Getenv("A2P2_BENCH_M8_ROUNDS")); err == nil && r > 0 {
		return r
	}
	q := scaled(2000, 200)
	if n >= 200 {
		q = scaled(800, 200)
	}
	return max((q+n-1)/n, 2)
}

// fanWant reports whether the one-machine case named by key runs:
// A2P2_BENCH_M8_CASES is a comma-separated list of keys (empty: all). The
// keys are N1, N10, N50 and N200 (one room, addressed), wide50 (room-wide,
// level all), rooms50 (a room per worker) and delay10 (queue shape).
func fanWant(key string) bool {
	cs := os.Getenv("A2P2_BENCH_M8_CASES")
	return cs == "" || slices.Contains(strings.Split(cs, ","), key)
}

// fanDaemon is the daemon of a one-machine case: a real one from the
// binary, or with A2P2_BENCH_M8_INPROC set the product's surface and local
// API in this process (newInproc), so that go test's -cpuprofile,
// -mutexprofile and -blockprofile see the daemon's side, and the counts of
// benchHooks are printed after each case.
func fanDaemon(t *testing.T, name string) (*proc, func(cs string, answered int64)) {
	if os.Getenv("A2P2_BENCH_M8_INPROC") == "" {
		return startDaemon(t, name), func(string, int64) {}
	}
	x := newInproc(t) // closed at the test's end
	report := benchHooks(x)
	return &proc{t: t, c: x.c, sock: x.sock}, report
}

// One machine: the orchestrator and the workers share one daemon.
func fanLocal(t *testing.T, name string, rooms int, wide bool, n, rounds int, delay time.Duration, cs string, rank bool) {
	t.Helper()
	p, report := fanDaemon(t, name)
	defer func() { report(cs, int64(n*rounds)) }()
	var rs []string
	if rooms == 0 {
		rs = []string{""} // the room of this machine only
	} else {
		for i := 0; i < rooms; i++ {
			rs = append(rs, call[localapi.Room](t, p.c, "room/create", localapi.RoomReq{Room: fmt.Sprintf("room%d", i)}).ID)
		}
	}
	runFanIn(t, fanCfg{cs: cs, n: n, rounds: rounds, orch: p, work: p, rooms: rs, wide: wide, delay: delay,
		limit: 8 * time.Minute, showRnk: rank})
	p.stop()
	p.home = ""
}

// M8 on one machine. Every configuration starts on a new daemon, so each
// has a history of 0 to its own questions and answers.
func TestM8FanIn(t *testing.T) {
	delay := fanDelay(t)
	note("M8 fan-in: the orchestrator's thinking time is not simulated: it answers at once, plus A2P2_BENCH_M8_DELAY per reply (here %s); rounds are synchronised: every worker asks, then all wait", delay)
	for _, n := range []int{1, 10, 50, 200} {
		if fanWant(fmt.Sprintf("N%d", n)) {
			fanLocal(t, "m8", 0, false, n, fanRounds(n), delay,
				fmt.Sprintf("1 machine, 1 room, to=orch, level addressed, N=%d, delay=%s", n, delay), false)
		}
	}
	if fanWant("wide50") {
		fanLocal(t, "m8b", 0, true, 50, fanRounds(50), delay,
			"1 machine, 1 room, room-wide, level all, N=50, delay="+delay.String(), false)
	}
	if fanWant("rooms50") {
		fanLocal(t, "m8c", 50, false, 50, fanRounds(50), delay,
			"1 machine, 50 rooms (one per worker), to=orch, orch reads all rooms, N=50, delay="+delay.String(), false)
	}
	// the shape of queueing: a 100 ms orchestrator delay, ten workers asking at once
	if fanWant("delay10") {
		fanLocal(t, "m8d", 0, false, 10, scaled(30, 5), 100*time.Millisecond,
			"1 machine, 1 room, to=orch, N=10, delay=100ms (queue shape)", true)
	}
}

// M8 on two machines, joined as in M5: the orchestrator is on the
// listening daemon, the workers on the one that connects out only. Each
// N has two minutes.
func TestM8FanInTwoMachines(t *testing.T) {
	delay := fanDelay(t)
	for _, n := range []int{10, 50} {
		func() {
			a, b := startDaemon(t, "m8a"), startDaemon(t, "m8b")
			t.Cleanup(func() {
				if t.Failed() {
					a.tailLog(40)
					b.tailLog(40)
				}
			})
			a.setNet("on", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
			b.setNet("outbound", "")
			room := call[localapi.Room](t, a.c, "room/create", localapi.RoomReq{Room: "bench"}).ID
			join(t, a, b, room)
			// one round each way first: streams begin, connections open
			aa, bb := a.agent("warm-a"), b.agent("warm-b")
			ra := startReader(t, a.client(), aa, room)
			rb := startReader(t, b.client(), bb, room)
			for _, w := range []struct {
				p     *proc
				agent string
				r     *reader
			}{{a, aa, rb}, {b, bb, ra}} {
				m := call[localapi.SendResp](t, w.p.client(), "send", localapi.SendReq{Agent: w.agent, Room: room, Text: "hello"})
				w.r.await(t, m.ID)
			}
			ra.close()
			rb.close()
			cs := fmt.Sprintf("2 machines, orch on listener, workers connect out only, to=orch, level addressed, N=%d, delay=%s [main before the fix]", n, delay)
			t0 := time.Now()
			st := runFanIn(t, fanCfg{cs: cs, n: n, rounds: fanRounds(n), orch: a, work: b, rooms: []string{room}, orchIn: room,
				delay: delay, limit: 2 * time.Minute})
			note("M8 N=%d two machines: %d questions answered in %.0f s; log lines mentioning 429: listener %d, worker machine %d (the daemon logs a refusal at most about once in ten seconds, so this undercounts); lines mentioning rate: %d/%d",
				n, st.answered, time.Since(t0).Seconds(), a.grepLog("429"), b.grepLog("429"), a.grepLog("rate"), b.grepLog("rate"))
			note("M8 N=%d two machines: pull requests per question are not counted: the daemon logs a successful pull at debug level only and keeps no counter", n)
			b.stop()
			b.home = ""
			a.stop()
			a.home = ""
		}()
	}
}
