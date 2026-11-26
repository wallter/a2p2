//go:build bench

package bench

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// maxText is the longest ASCII text one send takes: the record body is
// {"text":...} and must fit 16,384 bytes (core.MaxBodyBytes).
func maxText(t testing.TB, p *proc, agent string) int {
	t.Helper()
	lo, hi := 15000, 16384
	for lo < hi { // largest accepted
		mid := (lo + hi + 1) / 2
		err := p.c.Call(context.Background(), "send", localapi.SendReq{Agent: agent, Text: text(mid)}, nil)
		if err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// sendLoad sends total messages of size bytes into the local room from c
// senders at once (each its own client and label) and records each call.
func sendLoad(t *testing.T, p *proc, c, total, size int, what, cs string) {
	t.Helper()
	var l lat
	el := sendChunk(t, p, c, total, size, &l)
	result(t, what, cs, &l, el, "sends/s")
}

// sendChunk is one part of a send load into p; it returns the wall time.
func sendChunk(t *testing.T, p *proc, c, total, size int, l *lat) time.Duration {
	t.Helper()
	agents := make([]string, c)
	clients := make([]*localapi.Client, c)
	for i := range agents {
		agents[i] = p.agent(fmt.Sprintf("s%d", i))
		clients[i] = p.client()
	}
	msg := text(size)
	// warm each client's connection and the daemon's caches
	for i := range agents {
		if err := clients[i].Call(context.Background(), "send", localapi.SendReq{Agent: agents[i], Text: msg}, nil); err != nil {
			t.Fatal(err)
		}
	}
	per := max(total/c, 1)
	var wg sync.WaitGroup
	errs := make(chan error, c)
	start := time.Now()
	for i := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < per; k++ {
				t0 := time.Now()
				if err := clients[i].Call(context.Background(), "send", localapi.SendReq{Agent: agents[i], Text: msg}, nil); err != nil {
					errs <- err
					return
				}
				l.add(time.Since(t0))
			}
		}()
	}
	wg.Wait()
	el := time.Since(start)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return el
}

// m1Chunk is how many sends M1 makes into one daemon before it starts a
// new one: a send's cost grows with the room's history (M7), so M1 is
// measured over a history of 0 to m1Chunk messages.
const m1Chunk = 500

// sendFresh is sendLoad over new daemons, at most chunk sends each.
func sendFresh(t *testing.T, c, total, size int, what, cs string) {
	t.Helper()
	chunk := max(m1Chunk, c*20)
	var l lat
	var el time.Duration
	for done := 0; done < total; done += chunk {
		p := startDaemon(t, "m1")
		el += sendChunk(t, p, c, min(chunk, total-done), size, &l)
		p.stop()
		p.home = ""
	}
	result(t, what, cs, &l, el, "sends/s")
}

// M1: local send over the Unix socket through the localapi client.
func TestM1LocalSend(t *testing.T) {
	n := scaled(5000, 500)
	{
		p := startDaemon(t, "m1")
		a := p.agent("probe")
		note("M1 longest ASCII text accepted by one send: %d bytes", maxText(t, p, a))
		p.stop()
		p.home = ""
	}
	for _, size := range []int{200, 4096, 16300} {
		sendFresh(t, 1, n, size, "M1 send serial", fmt.Sprintf("%dB c=1 history<=%d", size, m1Chunk))
	}
	for _, c := range []int{4, 16, 64} {
		for _, size := range []int{200, 4096, 16300} {
			if size != 200 && c != 16 {
				continue
			}
			sendFresh(t, c, max(n, c*60), size, "M1 send concurrent", fmt.Sprintf("%dB c=%d history<=%d", size, c, max(m1Chunk, c*20)))
		}
	}
	// the same serial send, one daemon, history growing 0 to n
	p := startDaemon(t, "m1g")
	sendLoad(t, p, 1, n, 200, "M1 send serial", fmt.Sprintf("200B c=1 history 0..%d", n))
}

// reader is a label whose inbox waits, in a loop, and reports when each
// message became visible to it; it confirms what it saw.
type reader struct {
	c     *localapi.Client
	agent string
	got   chan visible
	stop  chan struct{}
	done  chan struct{}
	shim  bool
}

type visible struct {
	id string
	at time.Time
}

func startReader(t testing.TB, c *localapi.Client, agent, room string) *reader {
	r := &reader{c: c, agent: agent, got: make(chan visible, 1<<16), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			select {
			case <-r.stop:
				return
			default:
			}
			var in localapi.InboxResp
			err := c.Call(context.Background(), "inbox", localapi.InboxReq{Agent: agent, Room: room, Limit: 200, WaitMS: 2000}, &in)
			now := time.Now()
			if err != nil {
				t.Errorf("inbox: %v", err)
				return
			}
			for _, e := range in.Entries {
				r.got <- visible{e.ID, now}
			}
			if err := seen(c, agent, in.Entries); err != nil {
				t.Errorf("%v", err)
				return
			}
		}
	}()
	return r
}

func (r *reader) close() { close(r.stop); <-r.done }

// await returns when message id became visible to r.
func (r *reader) await(t testing.TB, id string) time.Time {
	t.Helper()
	at, ok := r.awaitUntil(id, time.Now().Add(60*time.Second))
	if !ok {
		t.Fatalf("message %s never became visible", id)
	}
	return at
}

// awaitUntil is await giving up at deadline.
func (r *reader) awaitUntil(id string, deadline time.Time) (time.Time, bool) {
	timeout := time.After(time.Until(deadline))
	for {
		select {
		case v := <-r.got:
			if v.id == id {
				return v.at, true
			}
		case <-timeout:
			return time.Time{}, false
		}
	}
}

// sendToVisible: n sends from a, each to b, timed from the send's start
// to b's waiting inbox returning it, until n are done or budget has
// passed (a message not visible by then ends the run and is reported).
// Before each send the sender pauses
// pause, so that b's inbox is waiting again (not counted).
func sendToVisible(t *testing.T, sp *proc, sc *localapi.Client, sa string, room string, to []string, r *reader, n int, pause, budget time.Duration, what, cs string) {
	t.Helper()
	var l, sendOnly lat
	var total time.Duration
	end := time.Now().Add(budget)
	for i := 0; i < n && time.Now().Before(end); i++ {
		time.Sleep(pause)
		t0 := time.Now()
		var m localapi.SendResp
		if err := sc.Call(context.Background(), "send", localapi.SendReq{Agent: sa, Room: room, Text: text(200), To: to}, &m); err != nil {
			t.Fatal(err)
		}
		sendOnly.add(time.Since(t0))
		at, ok := r.awaitUntil(m.ID, end)
		if !ok {
			note("%s %s: message %d sent %.1fs before the %s budget ended was not visible by then; stopped", what, cs, i+1, time.Since(t0).Seconds(), budget)
			break
		}
		l.add(at.Sub(t0))
		total += at.Sub(t0)
	}
	if len(l.d) < n {
		note("%s %s: %d of %d done within the %s budget", what, cs, len(l.d), n, budget)
	}
	result(t, what, cs, &l, total, "msgs/s (serial)")
	result(t, what, cs+" [send call only]", &sendOnly, 0, "-")
}

// M2: local send to visible, one daemon, two labels.
func TestM2LocalSendToVisible(t *testing.T) {
	n := scaled(2000, 300)
	for _, room := range []string{"", node.LocalLabel} {
		p := startDaemon(t, "m2")
		a, b := p.agent("alice"), p.agent("bob")
		r := startReader(t, p.client(), b, room)
		where := "inbox of every room"
		if room != "" {
			where = "inbox room=" + room
		}
		sendToVisible(t, p, p.client(), a, "", []string{b}, r, n, 2*time.Millisecond, 10*time.Minute, "M2 send->visible", fmt.Sprintf("200B to=bob, %s, history 0..%d", where, n))
		r.close()
		p.stop()
		p.home = ""
	}
}

// M3: ping-pong, two labels answering each other.
func TestM3PingPong(t *testing.T) {
	n := scaled(1000, 200)
	p := startDaemon(t, "m3")
	a, b := p.agent("ping"), p.agent("pong")
	ca, cb := p.client(), p.client()
	ctx := context.Background()
	waitOne := func(c *localapi.Client, who string) (localapi.Entry, error) {
		for {
			var in localapi.InboxResp
			if err := c.Call(ctx, "inbox", localapi.InboxReq{Agent: who, WaitMS: 10000}, &in); err != nil {
				return localapi.Entry{}, err
			}
			if len(in.Entries) == 0 {
				continue
			}
			if err := seen(c, who, in.Entries); err != nil {
				return localapi.Entry{}, err
			}
			return in.Entries[len(in.Entries)-1], nil
		}
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			e, err := waitOne(cb, b)
			if err == nil {
				err = cb.Call(ctx, "send", localapi.SendReq{Agent: b, Text: "pong " + e.Text[:min(10, len(e.Text))], ReplyTo: e.ID}, nil)
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var l lat
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if err := ca.Call(ctx, "send", localapi.SendReq{Agent: a, Text: fmt.Sprintf("ping %d %s", i, text(180)), To: []string{b}}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := waitOne(ca, a); err != nil {
			t.Fatal(err)
		}
		l.add(time.Since(t0))
	}
	el := time.Since(start)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result(t, "M3 ping-pong", fmt.Sprintf("round = (send,inbox,seen) x2, history 0..%d", 2*n), &l, el, "rounds/s")
}

// M5: two daemons joined over 127.0.0.1 through the real pinned TLS
// path. A listens; B connects out only, as a laptop that drops inbound.
func TestM5TwoDaemons(t *testing.T) {
	n := scaled(1000, 100)
	burst := scaled(2000, 200)
	a, b := startDaemon(t, "alpha"), startDaemon(t, "beta")
	t.Cleanup(func() { // runs before the daemons stop
		if t.Failed() {
			a.tailLog(40)
			b.tailLog(40)
		}
	})
	a.setNet("on", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	b.setNet("outbound", "")
	room := call[localapi.Room](t, a.c, "room/create", localapi.RoomReq{Room: "bench"}).ID
	join(t, a, b, room)
	aa, bb := a.agent("ann"), b.agent("bea")
	ra := startReader(t, a.client(), aa, room)
	defer ra.close()
	rb := startReader(t, b.client(), bb, room)
	defer rb.close()
	// one round each way first: streams begin, connections open
	ac, bc := a.client(), b.client()
	for _, w := range []struct {
		c     *localapi.Client
		agent string
		r     *reader
	}{{ac, aa, rb}, {bc, bb, ra}} {
		var m localapi.SendResp
		if err := w.c.Call(context.Background(), "send", localapi.SendReq{Agent: w.agent, Room: room, Text: "hello"}, &m); err != nil {
			t.Fatal(err)
		}
		w.r.await(t, m.ID)
	}
	sendToVisible(t, a, ac, aa, room, nil, rb, n, 5*time.Millisecond, 4*time.Minute, "M5 A->B send->visible", "serial, B waiting pull held at A")
	for _, w := range []struct {
		name  string
		c     *localapi.Client
		agent string
		r     *reader
	}{{"A->B", ac, aa, rb}, {"B->A", bc, bb, ra}} {
		time.Sleep(time.Second)
		starts := map[string]time.Time{}
		ids := make([]string, 0, burst)
		var sendL lat
		start := time.Now()
		for i := 0; i < burst; i++ {
			t0 := time.Now()
			var m localapi.SendResp
			if err := w.c.Call(context.Background(), "send", localapi.SendReq{Agent: w.agent, Room: room, Text: text(200)}, &m); err != nil {
				t.Fatal(err)
			}
			sendL.add(time.Since(t0))
			starts[m.ID] = t0
			ids = append(ids, m.ID)
		}
		sent := time.Since(start)
		var l lat
		var last time.Time
		left := map[string]bool{}
		for _, id := range ids {
			left[id] = true
		}
		timeout := time.After(8 * time.Minute)
		for len(left) > 0 {
			select {
			case v := <-w.r.got:
				if left[v.id] {
					delete(left, v.id)
					l.add(v.at.Sub(starts[v.id]))
					last = v.at
				}
			case <-timeout:
				t.Fatalf("%s burst: %d of %d never arrived", w.name, len(left), burst)
			}
		}
		result(t, "M5 "+w.name+" burst", fmt.Sprintf("%d sends back to back: send->visible", burst), &l, last.Sub(start), "msgs/s delivered")
		result(t, "M5 "+w.name+" burst", "[send call only]", &sendL, sent, "sends/s")
	}
	// last: once B backs off (sched.go spun), it stays backed off for minutes
	time.Sleep(2 * time.Second)
	sendToVisible(t, b, bc, bb, room, nil, ra, n, 5*time.Millisecond, 150*time.Second, "M5 B->A send->visible", "serial, B offers in its pull to A")
	note("M5 log lines on A mentioning 429/rate: %d/%d; on B: %d/%d", a.grepLog("429"), a.grepLog("rate"), b.grepLog("429"), b.grepLog("rate"))
}

// M7: scale shape: rooms on the daemon and messages in the room.
func TestM7Scale(t *testing.T) {
	n := scaled(2000, 200)
	ctx := context.Background()
	measure := func(p *proc, cs string) {
		s, rd, other := p.agent("sender"), p.agent("reader"), p.agent("other")
		sc, rc := p.client(), p.client()
		// send, addressed to another so the reader's inbox stays empty
		var l lat
		start := time.Now()
		for i := 0; i < n; i++ {
			t0 := time.Now()
			if err := sc.Call(ctx, "send", localapi.SendReq{Agent: s, Text: text(200), To: []string{other}}, nil); err != nil {
				t.Fatal(err)
			}
			l.add(time.Since(t0))
		}
		result(t, "M7 send", cs, &l, time.Since(start), "sends/s")
		// an empty inbox of every room, no wait
		l = lat{}
		start = time.Now()
		for i := 0; i < n; i++ {
			t0 := time.Now()
			var in localapi.InboxResp
			if err := rc.Call(ctx, "inbox", localapi.InboxReq{Agent: rd}, &in); err != nil {
				t.Fatal(err)
			}
			l.add(time.Since(t0))
			if len(in.Entries) != 0 {
				t.Fatalf("reader's inbox has %d", len(in.Entries))
			}
		}
		result(t, "M7 inbox empty", cs, &l, time.Since(start), "calls/s")
		// one new message: inbox (every room) and seen
		l = lat{}
		var tot time.Duration
		for i := 0; i < n/2; i++ {
			if err := sc.Call(ctx, "send", localapi.SendReq{Agent: s, Text: text(200), To: []string{rd}}, nil); err != nil {
				t.Fatal(err)
			}
			t0 := time.Now()
			var in localapi.InboxResp
			if err := rc.Call(ctx, "inbox", localapi.InboxReq{Agent: rd}, &in); err != nil {
				t.Fatal(err)
			}
			if len(in.Entries) != 1 {
				t.Fatalf("reader's inbox has %d", len(in.Entries))
			}
			if err := seen(rc, rd, in.Entries); err != nil {
				t.Fatal(err)
			}
			l.add(time.Since(t0))
			tot += time.Since(t0)
		}
		result(t, "M7 inbox1+seen", cs, &l, tot, "calls/s")
	}
	for _, rooms := range []int{1, 10, 100} {
		p := startDaemon(t, fmt.Sprintf("m7r%d", rooms))
		for i := 1; i < rooms; i++ {
			call[localapi.Room](t, p.c, "room/create", localapi.RoomReq{Room: fmt.Sprintf("room%d", i)})
		}
		measure(p, fmt.Sprintf("rooms=%d history=0", rooms))
		p.stop()
		p.home = ""
	}
	// 100,000 is not run: a send's cost grows with the room's history
	// (store.place's resolveDependants scans the room), so filling 100,000
	// would take most of an hour; 30,000 shows the slope.
	hist := []int{100, 1000, 10000, 30000}
	if testingQuick() {
		hist = []int{100, 1000, 10000}
	}
	for _, h := range hist {
		p := startDaemon(t, fmt.Sprintf("m7h%d", h))
		backlog := p.agent("backlog") // registered before the history: all of it unread
		start := time.Now()
		sendLoad(t, p, 8, h, 200, "M7 fill", fmt.Sprintf("history=%d c=8", h))
		note("M7 filled %d messages in %.1fs", h, time.Since(start).Seconds())
		measure(p, fmt.Sprintf("rooms=1 history=%d", h))
		// the backlog reader: first inbox page (limit 50) and its seen
		var l lat
		var tot time.Duration
		calls := 5
		if h > 1000 {
			calls = 2 // each takes seconds (Reader.ByRef scans the room per entry)
		}
		for i := 0; i < calls && h <= 10000; i++ {
			t0 := time.Now()
			var in localapi.InboxResp
			if err := p.c.Call(ctx, "inbox", localapi.InboxReq{Agent: backlog}, &in); err != nil {
				t.Fatal(err)
			}
			if err := seen(p.c, backlog, in.Entries); err != nil {
				t.Fatal(err)
			}
			l.add(time.Since(t0))
			tot += time.Since(t0)
		}
		if len(l.d) > 0 {
			result(t, "M7 backlog inbox50+seen", fmt.Sprintf("rooms=1 history=%d", h), &l, tot, "calls/s")
		}
		p.stop()
		p.home = ""
	}
}

func testingQuick() bool { return scaled(10, 1) == 1 }
