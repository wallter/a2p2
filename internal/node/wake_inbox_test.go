package node

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// Tests of how a waiting Inbox is woken (wake.go: inboxWaiter, touchWrite,
// mayShow). A wake that ends a wait must come from the write, not from the
// one-second fallback look: each test that needs that checks the time.

// waitInbox starts agent's Inbox of room ("" every room) waiting up to
// wait, and returns once it waits; the result arrives on the channel.
type inboxResult struct {
	v   InboxView
	err error
	at  time.Time
}

func (e *surfEnv) waitingInbox(agent, room string, wait time.Duration) <-chan inboxResult {
	e.t.Helper()
	ready := make(chan struct{})
	var once sync.Once
	e.s.waiting = func() { once.Do(func() { close(ready) }) }
	out := make(chan inboxResult, 1)
	go func() {
		v, err := e.s.Inbox(context.Background(), agent, room, 0, wait)
		out <- inboxResult{v, err, time.Now()}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		e.t.Fatal("the inbox did not start to wait")
	}
	e.s.waiting = nil
	return out
}

// looks counts the looks the surface's Inbox takes from now.
func (e *surfEnv) looks() *atomic.Int64 {
	var n atomic.Int64
	e.s.scanned = func(int, time.Time) { n.Add(1) }
	return &n
}

// woken returns the entries of a waiting inbox, failing unless they come
// within 700 ms of sent (a wake, not the fallback look).
func woken(t *testing.T, ch <-chan inboxResult, sent time.Time) []Entry {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if d := r.at.Sub(sent); len(r.v.Entries) > 0 && d > 700*time.Millisecond {
			t.Fatalf("returned %v after the write: the fallback look, not a wake", d)
		}
		return r.v.Entries
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting inbox did not return")
	}
	return nil
}

// [wake] A record that cannot show a waiting inbox anything does not wake
// it: messages addressed to another agent, the waiter's own messages and
// another agent's handled report. A message to it then wakes it at once.
func TestAWaitingInboxIsNotWokenByTrafficItCannotSee(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	q := e.send(SendInput{Agent: alice, Text: "for carol", To: []string{carol}})
	e.read(carol)
	looks := e.looks()
	woke := e.s.m.wakes.inboxWoken()
	ch := e.waitingInbox(bob, "", 20*time.Second)
	for i := 0; i < 20; i++ {
		e.send(SendInput{Agent: alice, Text: fmt.Sprintf("to carol %d", i), To: []string{carol}})
		e.send(SendInput{Agent: bob, Text: fmt.Sprintf("bob's own %d", i)})
	}
	if _, err := e.s.Ack(ctx, carol, q.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.s.m.wakes.inboxWoken() - woke; n != 0 {
		t.Fatalf("%d waiters woken by traffic bob cannot see", n)
	}
	if n := looks.Load(); n != 1 {
		t.Fatalf("bob's inbox looked %d times, want 1", n)
	}
	sent := time.Now()
	m := e.send(SendInput{Agent: alice, Text: "for bob", To: []string{bob}})
	es := woken(t, ch, sent)
	if len(es) != 1 || es[0].ID != m.ID {
		t.Fatalf("entries = %+v", es)
	}
}

// [wake] At level addressed a room-wide post does not wake the waiter; a
// post marked for everyone does, and so does a room-wide post at level all.
func TestAWaitingInboxAtLevelAddressedIsWokenOnlyByWhatTheLevelLetsIn(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	if _, err := e.s.Levels(ctx, bob, "", store.LevelAddressed); err != nil {
		t.Fatal(err)
	}
	woke := e.s.m.wakes.inboxWoken()
	ch := e.waitingInbox(bob, "", 20*time.Second)
	cw := e.waitingInbox(carol, "", 20*time.Second)
	sent := time.Now()
	plain := e.send(SendInput{Agent: alice, Text: "room-wide, not marked"})
	if es := woken(t, cw, sent); len(es) != 1 || es[0].ID != plain.ID {
		t.Fatalf("carol at level all: %+v", es)
	}
	if n := e.s.m.wakes.inboxWoken() - woke; n != 1 {
		t.Fatalf("%d waiters woken by a room-wide post, want 1 (carol)", n)
	}
	sent = time.Now()
	marked := e.send(SendInput{Agent: alice, Text: "@everyone stop"})
	if es := woken(t, ch, sent); len(es) != 1 || es[0].ID != marked.ID {
		t.Fatalf("bob at level addressed: %+v", es)
	}
}

// [wake] A level raised from addressed to all while the inbox waits wakes
// it, and the room-wide posts that follow reach it at once. The post held
// before the change stays out (skipped, U3), as without waiting.
func TestALevelChangedFromAddressedToAllWhileWaitingTakesEffectAtOnce(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	if _, err := e.s.Levels(ctx, bob, "", store.LevelAddressed); err != nil {
		t.Fatal(err)
	}
	looks := e.looks()
	ch := e.waitingInbox(bob, "", 20*time.Second)
	before := e.send(SendInput{Agent: alice, Text: "before the change"})
	if _, err := e.s.Levels(ctx, bob, "", store.LevelAll); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for looks.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := looks.Load(); n < 2 {
		t.Fatalf("the level change did not wake the inbox (%d looks)", n)
	}
	sent := time.Now()
	after := e.send(SendInput{Agent: alice, Text: "after the change"})
	es := woken(t, ch, sent)
	if len(es) != 1 || es[0].ID != after.ID {
		t.Fatalf("entries = %+v (the post before the change, %s, is skipped)", es, before.ID)
	}
}

// [wake] A replacement of a message the waiter confirmed wakes it with the
// replaced notice; a withdrawal with the withdrawn notice.
func TestAWaitingInboxIsWokenByAReplacementAndAWithdrawal(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	m := e.send(SendInput{Agent: alice, Text: "v1", To: []string{bob}})
	e.read(bob)
	ch := e.waitingInbox(bob, "", 20*time.Second)
	sent := time.Now()
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "v2"})
	es := woken(t, ch, sent)
	if len(es) != 1 || es[0].Kind != EntryReplaced || es[0].Text != "v2" {
		t.Fatalf("entries = %+v", es)
	}
	e.read(bob)
	ch = e.waitingInbox(bob, "", 20*time.Second)
	sent = time.Now()
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: " "})
	if es := woken(t, ch, sent); len(es) != 1 || es[0].Kind != EntryWithdrawn {
		t.Fatalf("entries = %+v", es)
	}
}

// [wake] An agent that registers while others wait is not shown to them,
// and a message to it reaches it and wakes none of them.
func TestAnAgentRegisteredWhileOthersWaitGetsItsMessagesAndWakesNoOne(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	ch := e.waitingInbox(bob, "", 3*time.Second)
	woke := e.s.m.wakes.inboxWoken()
	dave := e.agent("dave")
	dw := e.waitingInbox(dave, "", 20*time.Second)
	sent := time.Now()
	m := e.send(SendInput{Agent: alice, Text: "welcome", To: []string{dave}})
	if es := woken(t, dw, sent); len(es) != 1 || es[0].ID != m.ID {
		t.Fatalf("dave: %+v", es)
	}
	if n := e.s.m.wakes.inboxWoken() - woke; n != 1 {
		t.Fatalf("%d waiters woken, want 1 (dave)", n)
	}
	if r := <-ch; r.err != nil || len(r.v.Entries) != 0 {
		t.Fatalf("bob: %+v %v", r.v, r.err)
	}
	if _, err := e.s.Inbox(ctx, dave, "", 0, 0); err != nil {
		t.Fatal(err)
	}
}

// [wake] No lost wake-up at the worst moment: a message for the waiter is
// committed after its look has read the room and before it starts to wait.
// The waiter registered before the look, so the write wakes it at once.
func TestAMessageCommittedBetweenTheLookAndTheWaitWakesTheInbox(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	var once sync.Once
	var sent time.Time
	var id string
	e.s.scanned = func(int, time.Time) {
		once.Do(func() { // the first look's transactions are over
			sent = time.Now()
			m, err := e.s.Send(context.Background(), SendInput{Agent: alice, Text: "between", To: []string{bob}})
			if err != nil {
				t.Error(err)
			}
			id = m.ID
		})
	}
	v, err := e.s.Inbox(context.Background(), bob, "", 0, 20*time.Second)
	if err != nil || len(v.Entries) != 1 || v.Entries[0].ID != id {
		t.Fatalf("%+v %v", v, err)
	}
	if d := time.Since(sent); d > 700*time.Millisecond {
		t.Fatalf("returned %v after the write: the fallback look, not a wake", d)
	}
}

// [wake] No lost wake-up under concurrent senders and waiters: every
// message reaches its addressee, each waiter loops inbox (waiting) and
// seen, and none of them needs the one-second fallback to see a message
// (a wait that ends by the fallback while a message for it is held is
// counted). Run with the race detector.
func TestNoLostWakeUpWithManyConcurrentSendersAndWaiters(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	const waiters, senders, each = 12, 6, 25
	ws := make([]string, waiters)
	for i := range ws {
		ws[i] = e.agent(fmt.Sprintf("w%d", i))
		if i%3 == 0 { // some at level addressed: messages to them still wake them
			if _, err := e.s.Levels(ctx, ws[i], "", store.LevelAddressed); err != nil {
				t.Fatal(err)
			}
		}
	}
	ss := make([]string, senders)
	for i := range ss {
		ss[i] = e.agent(fmt.Sprintf("s%d", i))
	}
	want := make([]atomic.Int64, waiters)
	got := make([]atomic.Int64, waiters)
	var late atomic.Int64
	// fell is when each waiter's last fallback look began: a message it
	// then finds that was written well before had no wake (a lost one).
	var fmu sync.Mutex
	fell := map[string]time.Time{}
	e.s.fallback = func(label string, at time.Time) {
		fmu.Lock()
		fell[label] = at
		fmu.Unlock()
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					if got[i].Load() == want[i].Load() {
						return
					}
				default:
				}
				label := fmt.Sprintf("w%d", i)
				fmu.Lock()
				delete(fell, label)
				fmu.Unlock()
				v, err := e.s.Inbox(ctx, ws[i], "", 0, 2*time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				fmu.Lock()
				at, ok := fell[label]
				fmu.Unlock()
				for _, x := range v.Entries {
					if ok && x.Created.Before(at.Add(-300*time.Millisecond)) {
						late.Add(1)
					}
				}
				var rs []store.Read
				for _, x := range v.Entries {
					rs = append(rs, store.Read{Key: x.Key, Version: x.Version})
					got[i].Add(1)
				}
				if len(rs) > 0 {
					if _, err := e.s.Seen(ctx, ws[i], rs); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}()
	}
	var sw sync.WaitGroup
	for k := range ss {
		sw.Add(1)
		go func() {
			defer sw.Done()
			for j := 0; j < each; j++ {
				to := (k*each + j) % waiters
				want[to].Add(1)
				if _, err := e.s.Send(ctx, SendInput{Agent: ss[k], Text: fmt.Sprintf("m %d %d", k, j), To: []string{ws[to]}}); err != nil {
					t.Error(err)
					return
				}
				if j%5 == 0 { // room-wide traffic too: it wakes only waiters at level all
					if _, err := e.s.Send(ctx, SendInput{Agent: ss[k], Text: "room-wide"}); err != nil {
						t.Error(err)
						return
					}
					for i := range ws {
						if i%3 != 0 {
							want[i].Add(1)
						}
					}
				}
			}
		}()
	}
	sw.Wait()
	close(done)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		for i := range ws {
			t.Logf("w%d: got %d of %d", i, got[i].Load(), want[i].Load())
		}
		t.Fatal("not every message reached its waiter")
	}
	for i := range ws {
		if got[i].Load() != want[i].Load() {
			t.Fatalf("w%d got %d, want %d", i, got[i].Load(), want[i].Load())
		}
	}
	if n := late.Load(); n > 0 {
		t.Fatalf("%d waits saw their message only after the fallback look", n)
	}
}
