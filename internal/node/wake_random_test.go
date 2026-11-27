package node

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// A randomised comparison of waiting inbox calls with the behaviour they
// replace, in the spirit of the delivery-model comparison (which makes no
// waiting call). Before targeted wakes, every write woke every waiting
// inbox, so each waiting call took a plain look after every write. Here
// several agents wait while one driver makes random writes; after each,
// a plain look (Surface.inbox, which keeps no receipt) is made for every
// waiter. A call must return exactly when that look shows something, with
// what it shows, before the next write and without a fallback look; and
// it must not return while the look shows nothing. Before each write,
// every call has registered and finished a look that showed it nothing,
// so no write can slip in before a registration. Every operation must
// succeed (the driver makes only valid ones), and each kind must happen a
// minimum number of times across the schedules.

// randWaiter is one agent's waiting Inbox call, of one room or of all.
type randWaiter struct {
	agent, label string
	room         string // as given to Inbox: "" every room
	scope        string // the room id for the plain look, "" every room
	ch           chan inboxResult
	done         chan struct{}
	cancel       context.CancelFunc
}

func (w *randWaiter) start(s *Surface) {
	ctx, cancel := context.WithCancel(context.Background())
	w.ch, w.done, w.cancel = make(chan inboxResult, 1), make(chan struct{}), cancel
	ch, done := w.ch, w.done
	go func() {
		defer close(done)
		v, err := s.Inbox(ctx, w.agent, w.room, 0, time.Minute)
		ch <- inboxResult{v, err, time.Now()}
	}()
}

func (w *randWaiter) stop() {
	w.cancel()
	<-w.done
}

// armed reports whether w's call has a waiter registered whose look ended
// (it has its levels): it waits.
func (w *randWaiter) armed(s *Surface) bool {
	k := &s.m.wakes
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, xs := range k.inbox {
		for x := range xs {
			if x.agent == w.label && x.levels != nil {
				return true
			}
		}
	}
	return false
}

// randSent is a message the driver sent: replies, replaces, withdrawals
// and handled reports pick from them.
type randSent struct {
	id, author string
	ask        bool
	gone       bool // withdrawn
}

const (
	randSchedules = 30
	randSteps     = 40
)

// randKinds are the operations, each of which must happen at least
// randMin times across the schedules (a waiting call returning too).
var randKinds = []string{"post", "post @everyone", "send", "reply", "thread reply", "replace", "withdraw",
	"ask", "ack", "level all", "level addressed", "level none", "level one room", "returned"}

const randMin = 15

// [wake] Waiting inbox calls agree with a plain look after every write.
func TestWaitingInboxesAgreeWithALookAfterEveryWriteOnRandomSchedules(t *testing.T) {
	var mu sync.Mutex
	done := map[string]int{}
	count := func(kind string) {
		mu.Lock()
		done[kind]++
		mu.Unlock()
	}
	t.Run("schedules", func(t *testing.T) {
		for seed := uint64(1); seed <= randSchedules; seed++ {
			t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
				t.Parallel()
				runWaitSchedule(t, seed, count)
			})
		}
	})
	if t.Failed() {
		return
	}
	for _, k := range randKinds {
		if done[k] < randMin {
			t.Errorf("%q happened %d times in %d schedules, want at least %d", k, done[k], randSchedules, randMin)
		}
	}
	t.Logf("operations done: %v", done)
}

func runWaitSchedule(t *testing.T, seed uint64, count func(string)) {
	e := newSurf(t)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	team, err := e.s.CreateRoom(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	local, err := e.s.ResolveRoom(ctx, LocalLabel)
	if err != nil {
		t.Fatal(err)
	}
	ag := func(l string) string { return e.agent(l) }
	w0, w1, w2, w3, s0 := ag("w0"), ag("w1"), ag("w2"), ag("w3"), ag("s0")
	human := e.s.HumanID()
	everyone := []string{w0, w1, w2, w3, s0, human}
	leveled := []string{w0, w1, w2, w3} // the person has no level
	rooms := []string{local, team.ID}

	var fmu sync.Mutex
	fell := map[string][]time.Time{}
	e.s.fallback = func(label string, at time.Time) {
		fmu.Lock()
		fell[label] = append(fell[label], at)
		fmu.Unlock()
	}
	ws := []*randWaiter{
		{agent: w0}, {agent: w1, room: local, scope: local}, {agent: w2, room: team.ID, scope: team.ID},
		{agent: w3}, {agent: human},
	}
	for _, w := range ws {
		if w.label, err = e.s.label(w.agent); err != nil {
			t.Fatal(err)
		}
		w.start(e.s)
	}
	defer func() {
		for _, w := range ws {
			w.stop()
		}
	}()

	var sent []*randSent
	shown := map[string]map[string]bool{} // agent -> messages a waiting call returned to it
	handled := map[string]bool{}          // agent+id reported handled
	var log []string                      // the schedule, for a failure
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %d, after %v: %s", seed, log, fmt.Sprintf(format, args...))
	}
	must := func(kind, what string, err error) {
		t.Helper()
		log = append(log, what)
		if err != nil {
			fail("%s: %v", what, err)
		}
		count(kind)
	}
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	live := func(f func(*randSent) bool) *randSent {
		var c []*randSent
		for _, s := range sent {
			if !s.gone && f(s) {
				c = append(c, s)
			}
		}
		if len(c) == 0 {
			return nil
		}
		return c[rng.IntN(len(c))]
	}
	send := func(kind string, in SendInput, what string) {
		t.Helper()
		s, err := e.s.Send(ctx, in)
		must(kind, what, err)
		if in.Replaces == "" {
			sent = append(sent, &randSent{id: s.ID, author: in.Agent, ask: in.AskHandled})
		}
	}
	// armAll waits until every call waits: registered, its look done.
	armAll := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for _, w := range ws {
			for !w.armed(e.s) {
				if time.Now().After(deadline) {
					fail("%s's inbox call never started to wait", w.label)
				}
				time.Sleep(200 * time.Microsecond)
			}
		}
	}
	// noReturn fails if a call returned while the plain look showed it
	// nothing (checked before each write: the look was after the last).
	noReturn := func() {
		t.Helper()
		for _, w := range ws {
			select {
			case r := <-w.ch:
				fail("%s's waiting inbox returned %d entries (err %v) when a plain look showed nothing", w.label, len(r.v.Entries), r.err)
			default:
			}
		}
	}
	// settle checks w after a write that returned at since: while a plain
	// look shows it something, its call returns exactly that, with no
	// fallback look after since; the driver confirms it (all, or at first
	// some) and starts the call again.
	settle := func(w *randWaiter, since time.Time) {
		t.Helper()
		for try := 0; ; try++ {
			want, err := e.s.inbox(ctx, w.label, w.scope)
			if err != nil {
				fail("plain look of %s: %v", w.label, err)
			}
			if len(want.Entries) == 0 {
				return
			}
			if try > 8 {
				fail("%s's inbox never empties: %+v", w.label, want.Entries)
			}
			var r inboxResult
			select {
			case r = <-w.ch:
			case <-time.After(10 * time.Second):
				fail("a plain look shows %s %d entries; its waiting inbox did not return (a lost wake)", w.label, len(want.Entries))
			}
			if r.err != nil {
				fail("%s's inbox: %v", w.label, r.err)
			}
			exp := want.Entries[:min(len(want.Entries), DefaultInboxLimit)]
			if len(r.v.Entries) != len(exp) {
				fail("%s's waiting inbox returned %d entries, a plain look shows %d", w.label, len(r.v.Entries), len(exp))
			}
			for i, x := range r.v.Entries {
				y := exp[i]
				if x.Key != y.Key || x.Version != y.Version || x.Kind != y.Kind || x.ID != y.ID {
					fail("%s entry %d: returned %s %s %s, a plain look shows %s %s %s", w.label, i, x.Kind, x.Key, x.Version, y.Kind, y.Key, y.Version)
				}
			}
			fmu.Lock()
			for _, at := range fell[w.label] {
				if at.After(since) {
					fmu.Unlock()
					fail("%s's waiting inbox returned only by a fallback look, %v after the write", w.label, at.Sub(since))
				}
			}
			fmu.Unlock()
			if try == 0 {
				count("returned")
			}
			reads := make([]store.Read, 0, len(r.v.Entries))
			for _, x := range r.v.Entries {
				reads = append(reads, store.Read{Key: x.Key, Version: x.Version})
				if x.Kind == EntryMessage {
					if shown[w.agent] == nil {
						shown[w.agent] = map[string]bool{}
					}
					shown[w.agent][x.ID] = true
				}
			}
			if try == 0 && len(reads) > 1 && rng.IntN(3) == 0 {
				reads = reads[:len(reads)/2] // some stay, and are shown again at once
			}
			if _, err := e.s.Seen(ctx, w.agent, reads); err != nil {
				fail("seen %s: %v", w.label, err)
			}
			<-w.done
			w.start(e.s)
			since = time.Now()
		}
	}

	for step := 0; step < randSteps; step++ {
		armAll()
		noReturn()
		from := pick(everyone)
		room := pick(rooms)
		switch op := rng.IntN(13); {
		case op < 3:
			text, kind := fmt.Sprintf("post %d", step), "post"
			if rng.IntN(3) == 0 {
				text, kind = "@everyone "+text, "post @everyone"
			}
			send(kind, SendInput{Agent: from, Room: room, Text: text}, fmt.Sprintf("%s posts %q in %s", from, text, room[:6]))
		case op < 5:
			to := []string{pick(everyone)}
			if rng.IntN(2) == 0 {
				to = append(to, pick(everyone))
			}
			send("send", SendInput{Agent: from, Room: room, Text: fmt.Sprintf("to %v %d", to, step), To: to},
				fmt.Sprintf("%s sends to %v in %s", from, to, room[:6]))
		case op == 5:
			if p := live(func(*randSent) bool { return true }); p != nil {
				thread := rng.IntN(2) == 0
				kind := "reply"
				if thread {
					kind = "thread reply"
				}
				send(kind, SendInput{Agent: from, Text: fmt.Sprintf("reply %d", step), ReplyTo: p.id, Thread: thread},
					fmt.Sprintf("%s replies to %s (thread %v)", from, p.id[:6], thread))
			}
		case op == 6:
			if p := live(func(*randSent) bool { return true }); p != nil {
				send("replace", SendInput{Agent: p.author, Replaces: p.id, Text: fmt.Sprintf("edited %d", step)},
					fmt.Sprintf("%s replaces %s", p.author, p.id[:6]))
			}
		case op == 7:
			if p := live(func(*randSent) bool { return true }); p != nil {
				send("withdraw", SendInput{Agent: p.author, Replaces: p.id, Text: " "}, fmt.Sprintf("%s withdraws %s", p.author, p.id[:6]))
				p.gone = true
			}
		case op == 8:
			send("ask", SendInput{Agent: from, Room: room, Text: fmt.Sprintf("who handles %d?", step), AskHandled: true},
				fmt.Sprintf("%s asks in %s", from, room[:6]))
		case op == 9 || op == 10:
			// A question that a waiter was shown, not its own and not yet
			// reported: that waiter reports it handled.
			var a string
			p := live(func(s *randSent) bool {
				if !s.ask {
					return false
				}
				for _, w := range ws {
					if shown[w.agent][s.id] && w.agent != s.author && !handled[w.agent+s.id] {
						a = w.agent
						return true
					}
				}
				return false
			})
			if p != nil {
				for _, w := range ws { // the candidate chosen may not be the last a set
					if shown[w.agent][p.id] && w.agent != p.author && !handled[w.agent+p.id] {
						a = w.agent
						break
					}
				}
				_, err := e.s.Ack(ctx, a, p.id)
				must("ack", fmt.Sprintf("%s acks %s", a, p.id[:6]), err)
				handled[a+p.id] = true
			}
		default:
			a := pick(leveled)
			l := []string{store.LevelAll, store.LevelAddressed, store.LevelNone}[rng.IntN(3)]
			r := ""
			if rng.IntN(2) == 0 {
				r = room
			}
			_, err := e.s.Levels(ctx, a, r, l)
			must("level "+l, fmt.Sprintf("%s level %s in %q", a, l, r), err)
			if r != "" {
				count("level one room")
			}
		}
		since := time.Now()
		for _, w := range ws {
			settle(w, since)
		}
	}
	armAll()
	time.Sleep(50 * time.Millisecond)
	noReturn()
}
