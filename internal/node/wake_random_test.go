package node

import (
	"context"
	"errors"
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
// it must not return while the look shows nothing.

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

// randSent is a message the driver sent: replies, replaces, withdrawals
// and handled reports pick from them.
type randSent struct {
	id, author string
	ask        bool
}

const (
	randSchedules = 30
	randSteps     = 40
)

// [wake] Waiting inbox calls agree with a plain look after every write.
func TestWaitingInboxesAgreeWithALookAfterEveryWriteOnRandomSchedules(t *testing.T) {
	for seed := uint64(1); seed <= randSchedules; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			runWaitSchedule(t, seed)
		})
	}
}

func runWaitSchedule(t *testing.T, seed uint64) {
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

	var sent []randSent
	var log []string // the schedule, for a failure
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %d, after %v: %s", seed, log, fmt.Sprintf(format, args...))
	}
	okErr := func(what string, err error) {
		t.Helper()
		var ue *UserError
		if err != nil && !errors.As(err, &ue) {
			fail("%s: %v", what, err)
		}
	}
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	send := func(in SendInput, what string) {
		t.Helper()
		s, err := e.s.Send(ctx, in)
		okErr(what, err)
		log = append(log, what)
		if err == nil && in.Replaces == "" {
			sent = append(sent, randSent{id: s.ID, author: in.Agent, ask: in.AskHandled})
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
			reads := make([]store.Read, 0, len(r.v.Entries))
			for _, x := range r.v.Entries {
				reads = append(reads, store.Read{Key: x.Key, Version: x.Version})
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
		noReturn()
		from := pick(everyone)
		room := pick(rooms)
		switch op := rng.IntN(12); {
		case op < 3:
			text := fmt.Sprintf("post %d", step)
			if rng.IntN(4) == 0 {
				text = "@everyone " + text
			}
			send(SendInput{Agent: from, Room: room, Text: text}, fmt.Sprintf("%s posts %q in %s", from, text, room[:6]))
		case op < 5:
			to := []string{pick(everyone)}
			if rng.IntN(2) == 0 {
				to = append(to, pick(everyone))
			}
			send(SendInput{Agent: from, Room: room, Text: fmt.Sprintf("to %v %d", to, step), To: to},
				fmt.Sprintf("%s sends to %v in %s", from, to, room[:6]))
		case op == 5 && len(sent) > 0:
			p := sent[rng.IntN(len(sent))]
			send(SendInput{Agent: from, Text: fmt.Sprintf("reply %d", step), ReplyTo: p.id, Thread: rng.IntN(2) == 0},
				fmt.Sprintf("%s replies to %s", from, p.id[:6]))
		case op == 6 && len(sent) > 0:
			p := sent[rng.IntN(len(sent))]
			send(SendInput{Agent: p.author, Replaces: p.id, Text: fmt.Sprintf("edited %d", step)},
				fmt.Sprintf("%s replaces %s", p.author, p.id[:6]))
		case op == 7 && len(sent) > 0:
			p := sent[rng.IntN(len(sent))]
			send(SendInput{Agent: p.author, Replaces: p.id, Text: " "}, fmt.Sprintf("%s withdraws %s", p.author, p.id[:6]))
		case op == 8:
			send(SendInput{Agent: from, Room: room, Text: fmt.Sprintf("who handles %d?", step), AskHandled: true},
				fmt.Sprintf("%s asks in %s", from, room[:6]))
		case op == 9 && len(sent) > 0:
			p := sent[rng.IntN(len(sent))]
			for _, q := range sent { // a question asked, if any
				if q.ask && rng.IntN(2) == 0 {
					p = q
				}
			}
			_, err := e.s.Ack(ctx, from, p.id)
			okErr("ack", err)
			log = append(log, fmt.Sprintf("%s acks %s (ask %v, err %v)", from, p.id[:6], p.ask, err != nil))
		default:
			a := pick(leveled)
			l := []string{store.LevelAll, store.LevelAddressed, store.LevelNone}[rng.IntN(3)]
			r := ""
			if rng.IntN(2) == 0 {
				r = room
			}
			_, err := e.s.Levels(ctx, a, r, l)
			okErr("level", err)
			log = append(log, fmt.Sprintf("%s level %s in %q", a, l, r))
		}
		since := time.Now()
		for _, w := range ws {
			settle(w, since)
		}
	}
	time.Sleep(50 * time.Millisecond)
	noReturn()
}
