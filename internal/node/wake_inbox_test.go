package node

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Tests of how a waiting Inbox is woken (wake.go: inboxWaiter, touchWrite,
// mayShow). Each asserts on the wake channel itself: after a write has
// returned, the waiter registered for the look is closed or it is not.
// touchWrite runs before the write returns, so this needs no timing.

type inboxResult struct {
	v   InboxView
	err error
	at  time.Time
}

// armedInbox is a waiting Surface.Inbox call and the waiter registered for
// its current look.
type armedInbox struct {
	x  *inboxWaiter
	ch <-chan inboxResult
}

// woken reports whether the waiter was closed, at once.
func (a *armedInbox) woken() bool { return isClosed(a.x) }

func isClosed(x *inboxWaiter) bool {
	select {
	case <-x.ch:
		return true
	default:
		return false
	}
}

// waiterAfter returns agent's inbox waiter once it is registered and its
// look has given it levels (it waits), other than prev.
func (e *surfEnv) waiterAfter(agent string, prev *inboxWaiter) *inboxWaiter {
	e.t.Helper()
	label, err := e.s.label(agent)
	if err != nil {
		e.t.Fatal(err)
	}
	w := &e.s.m.wakes
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		for _, xs := range w.inbox {
			for x := range xs {
				if x.agent == label && x != prev && x.levels != nil {
					w.mu.Unlock()
					return x
				}
			}
		}
		w.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	e.t.Fatalf("no waiting inbox of %s", agent)
	return nil
}

// arm starts agent's Inbox of room ("" every room) waiting a minute and
// returns it once it waits. The call is ended when the test ends.
func (e *surfEnv) arm(agent, room string) *armedInbox {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan inboxResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := e.s.Inbox(ctx, agent, room, 0, time.Minute)
		out <- inboxResult{v, err, time.Now()}
	}()
	e.t.Cleanup(func() { cancel(); <-done })
	return &armedInbox{x: e.waiterAfter(agent, nil), ch: out}
}

// result is the call's result, which must come (it was woken with entries).
func (a *armedInbox) result(t *testing.T) []Entry {
	t.Helper()
	select {
	case r := <-a.ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.v.Entries
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting inbox did not return")
	}
	return nil
}

// looks counts the looks the surface's Inbox takes from now.
func (e *surfEnv) looks() *atomic.Int64 {
	var n atomic.Int64
	e.s.scanned = func(int, time.Time) { n.Add(1) }
	return &n
}

func onlyEntry(t *testing.T, es []Entry, id string) {
	t.Helper()
	if len(es) != 1 || es[0].ID != id {
		t.Fatalf("entries = %+v, want only %s", es, id)
	}
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
	a := e.arm(bob, "")
	for i := 0; i < 20; i++ {
		e.send(SendInput{Agent: alice, Text: fmt.Sprintf("to carol %d", i), To: []string{carol}})
		e.send(SendInput{Agent: bob, Text: fmt.Sprintf("bob's own %d", i)})
	}
	if _, err := e.s.Ack(ctx, carol, q.ID); err != nil {
		t.Fatal(err)
	}
	if a.woken() {
		t.Fatal("bob's inbox was woken by traffic it cannot see")
	}
	if n := e.s.m.wakes.inboxWoken() - woke; n != 0 {
		t.Fatalf("%d waiters woken by traffic bob cannot see", n)
	}
	if n := looks.Load(); n != 1 {
		t.Fatalf("bob's inbox looked %d times, want 1", n)
	}
	m := e.send(SendInput{Agent: alice, Text: "for bob", To: []string{bob}})
	if !a.woken() {
		t.Fatal("a message to bob did not wake his inbox")
	}
	onlyEntry(t, a.result(t), m.ID)
}

// [wake] At level addressed a room-wide post does not wake the waiter; a
// post marked for everyone does, and so does a room-wide post at level all.
func TestAWaitingInboxAtLevelAddressedIsWokenOnlyByWhatTheLevelLetsIn(t *testing.T) {
	e := newSurf(t)
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	e.level(bob, "", store.LevelAddressed)
	b, c := e.arm(bob, ""), e.arm(carol, "")
	plain := e.send(SendInput{Agent: alice, Text: "room-wide, not marked"})
	if b.woken() || !c.woken() {
		t.Fatalf("a room-wide post: bob (addressed) woken %v, carol (all) woken %v", b.woken(), c.woken())
	}
	onlyEntry(t, c.result(t), plain.ID)
	marked := e.send(SendInput{Agent: alice, Text: "@everyone stop"})
	if !b.woken() {
		t.Fatal("a post marked for everyone did not wake bob at level addressed")
	}
	onlyEntry(t, b.result(t), marked.ID)
}

// [wake] A level raised from addressed to all while the inbox waits wakes
// it at once (touchInbox), and the next room-wide post wakes it too. The
// post held before the change stays out (skipped, U3), as without waiting.
func TestALevelChangedFromAddressedToAllWhileWaitingTakesEffectAtOnce(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	e.level(bob, "", store.LevelAddressed)
	a := e.arm(bob, "")
	before := e.send(SendInput{Agent: alice, Text: "before the change"})
	if a.woken() {
		t.Fatal("a room-wide post woke bob at level addressed")
	}
	e.level(bob, "", store.LevelAll)
	if !a.woken() {
		t.Fatal("the level change did not wake bob's waiting inbox")
	}
	again := e.waiterAfter(bob, a.x) // the look at the new level found nothing
	after := e.send(SendInput{Agent: alice, Text: "after the change"})
	if !isClosed(again) {
		t.Fatal("a room-wide post at the new level did not wake bob")
	}
	es := a.result(t)
	if len(es) != 1 || es[0].ID != after.ID {
		t.Fatalf("entries = %+v (the post before the change, %s, is skipped)", es, before.ID)
	}
}

// [wake] A level change wakes only that agent's waiters.
func TestALevelChangeWakesOnlyThatAgentsWaitingInbox(t *testing.T) {
	e := newSurf(t)
	bob, carol := e.agent("bob"), e.agent("carol")
	b, c := e.arm(bob, ""), e.arm(carol, "")
	e.level(bob, "", store.LevelAddressed)
	if !b.woken() || c.woken() {
		t.Fatalf("bob's level change: bob woken %v, carol woken %v", b.woken(), c.woken())
	}
}

// [wake] A replacement of a message the waiter confirmed wakes it with the
// replaced notice; a withdrawal with the withdrawn notice.
func TestAWaitingInboxIsWokenByAReplacementAndAWithdrawal(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	m := e.send(SendInput{Agent: alice, Text: "v1", To: []string{bob}})
	e.read(bob)
	a := e.arm(bob, "")
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "v2"})
	if !a.woken() {
		t.Fatal("a replacement did not wake bob")
	}
	if es := a.result(t); len(es) != 1 || es[0].Kind != EntryReplaced || es[0].Text != "v2" {
		t.Fatalf("entries = %+v", es)
	}
	e.read(bob)
	a = e.arm(bob, "")
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: " "})
	if !a.woken() {
		t.Fatal("a withdrawal did not wake bob")
	}
	if es := a.result(t); len(es) != 1 || es[0].Kind != EntryWithdrawn {
		t.Fatalf("entries = %+v", es)
	}
}

// [wake] A handled report wakes its writer's waiting inbox, whose earlier
// entry it can change, and no one else's: not the asker's, which sees the
// report in the thread, and not a bystander's.
func TestAHandledReportWakesItsWriterAndNoOneElse(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	q := e.send(SendInput{Agent: alice, Text: "who handles this?", AskHandled: true})
	e.read(bob)
	e.read(carol)
	wa, wb, wc := e.arm(alice, ""), e.arm(bob, ""), e.arm(carol, "")
	if _, err := e.s.Ack(ctx, carol, q.ID); err != nil {
		t.Fatal(err)
	}
	if !wc.woken() || wa.woken() || wb.woken() {
		t.Fatalf("carol's handled report: carol woken %v, alice %v, bob %v", wc.woken(), wa.woken(), wb.woken())
	}
}

// [wake] A reply to the thread at level addressed wakes the waiters that
// posted in the thread (the reply addresses them), not another waiter at
// level addressed.
func TestAThreadReplyWakesAWaiterAtLevelAddressedThatPostedInTheThread(t *testing.T) {
	e := newSurf(t)
	alice, bob, carol, dave := e.agent("alice"), e.agent("bob"), e.agent("carol"), e.agent("dave")
	root := e.send(SendInput{Agent: alice, Text: "root"})
	e.send(SendInput{Agent: bob, Text: "bob joins", ReplyTo: root.ID})
	e.read(bob)
	e.read(carol)
	e.level(bob, "", store.LevelAddressed)
	e.level(carol, "", store.LevelAddressed)
	b, c := e.arm(bob, ""), e.arm(carol, "")
	r := e.send(SendInput{Agent: dave, Text: "to the thread", ReplyTo: root.ID, Thread: true})
	if !b.woken() || c.woken() {
		t.Fatalf("a thread reply: bob (in the thread) woken %v, carol (not) woken %v", b.woken(), c.woken())
	}
	onlyEntry(t, b.result(t), r.ID)
}

// [wake] A message to the person wakes the person's waiting mailbox and
// not an agent's; a post by the person wakes the agents it can show
// something to and not the person's own mailbox.
func TestAMessageToThePersonWakesTheHumanMailboxOnly(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	h, b := e.arm(e.s.HumanID(), ""), e.arm(bob, "")
	m := e.send(SendInput{Agent: alice, Text: "look", To: []string{HumanLabel}})
	if !h.woken() || b.woken() {
		t.Fatalf("a message to the person: human woken %v, bob woken %v", h.woken(), b.woken())
	}
	onlyEntry(t, h.result(t), m.ID)
	e.read(e.s.HumanID())
	h = e.arm(e.s.HumanID(), "")
	p := e.send(SendInput{Agent: e.s.HumanID(), Text: "from the person", To: []string{bob}})
	if !b.woken() || h.woken() {
		t.Fatalf("a post by the person: bob woken %v, human woken %v", b.woken(), h.woken())
	}
	onlyEntry(t, b.result(t), p.ID)
}

// [wake] An inbox waiting on one named room is woken by a write it can see
// there and not by one in another room; an inbox waiting on every room is
// woken by a write in a second room, and by a room created while it waits.
func TestAWaitingInboxOfOneRoomOrEveryRoomIsWokenByTheRightRoom(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	team, err := e.s.CreateRoom(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	b := e.arm(bob, team.ID) // bob waits on team only
	c := e.arm(carol, "")    // carol on every room
	e.send(SendInput{Agent: alice, Text: "local, to bob", To: []string{bob}})
	if b.woken() || c.woken() {
		t.Fatalf("a local message to bob: bob (on team) woken %v, carol woken %v", b.woken(), c.woken())
	}
	m := e.send(SendInput{Agent: alice, Room: team.ID, Text: "team, to bob", To: []string{bob}})
	if !b.woken() || c.woken() {
		t.Fatalf("a team message to bob: bob woken %v, carol woken %v", b.woken(), c.woken())
	}
	onlyEntry(t, b.result(t), m.ID)
	n := e.send(SendInput{Agent: alice, Room: team.ID, Text: "team, to carol", To: []string{carol}})
	if !c.woken() {
		t.Fatal("a message to carol in a second room did not wake her inbox of every room")
	}
	onlyEntry(t, c.result(t), n.ID)
	e.read(carol)
	c = e.arm(carol, "")
	if _, err := e.s.CreateRoom(ctx, "later"); err != nil {
		t.Fatal(err)
	}
	if !c.woken() {
		t.Fatal("a room created while carol waits did not wake her inbox of every room")
	}
}

// [wake] An agent that registers while others wait is not shown to them,
// and a message to it reaches it and wakes none of them.
func TestAnAgentRegisteredWhileOthersWaitGetsItsMessagesAndWakesNoOne(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	b := e.arm(bob, "")
	woke := e.s.m.wakes.inboxWoken()
	dave := e.agent("dave")
	d := e.arm(dave, "")
	m := e.send(SendInput{Agent: alice, Text: "welcome", To: []string{dave}})
	if !d.woken() || b.woken() {
		t.Fatalf("dave woken %v, bob woken %v", d.woken(), b.woken())
	}
	onlyEntry(t, d.result(t), m.ID)
	if n := e.s.m.wakes.inboxWoken() - woke; n != 1 {
		t.Fatalf("%d waiters woken, want 1 (dave)", n)
	}
}

// [wake] No lost wake-up at the worst moment: a write is committed after
// the look has read the room and before the waiter knows the level the
// look read. The waiter registered before the look, so the write closes
// it; with the level not yet known, a write it may not show wakes it too.
func TestAMessageCommittedBetweenTheLookAndTheWaitWakesTheInbox(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	label, _ := e.s.label(bob)
	// waiterOf is bob's waiter, registered and not yet given levels.
	waiterOf := func() *inboxWaiter {
		w := &e.s.m.wakes
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, xs := range w.inbox {
			for x := range xs {
				if x.agent == label {
					return x
				}
			}
		}
		return nil
	}
	for _, c := range []struct {
		name  string
		level string
		in    SendInput
		shows bool
	}{
		{"a message to bob", store.LevelAll, SendInput{Agent: alice, Text: "between", To: []string{bob}}, true},
		{"a room-wide post at level addressed", store.LevelAddressed, SendInput{Agent: alice, Text: "between, room-wide"}, false},
	} {
		e.level(bob, "", c.level)
		var once sync.Once
		var id string
		e.s.scanned = func(int, time.Time) {
			once.Do(func() { // the first look's transactions are over
				x := waiterOf()
				if x == nil || x.levels != nil {
					t.Errorf("%s: bob's waiter is %+v at the end of the look", c.name, x)
					return
				}
				m, err := e.s.Send(ctx, c.in)
				if err != nil {
					t.Error(err)
				}
				id = m.ID
				if !isClosed(x) {
					t.Errorf("%s between the look and the wait did not wake the waiter", c.name)
				}
			})
		}
		wait := 20 * time.Second
		if !c.shows {
			wait = 300 * time.Millisecond
		}
		v, err := e.s.Inbox(ctx, bob, "", 0, wait)
		e.s.scanned = nil
		if err != nil {
			t.Fatal(err)
		}
		if c.shows {
			onlyEntry(t, v.Entries, id)
			e.read(bob)
		} else if len(v.Entries) != 0 {
			t.Fatalf("%s: %+v", c.name, v.Entries)
		}
	}
}

// [wake] A level change racing a send never leaves a waiting inbox to the
// fallback look: whichever commits first, the waiter is woken, and when a
// plain look made after both shows the post, the call returns it with no
// fallback look after both returned. Run with the race detector.
func TestALevelChangeRacingASendNeverLeavesTheWaiterOnTheFallback(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	label, _ := e.s.label(bob)
	var fmu sync.Mutex
	var fell []time.Time
	e.s.fallback = func(l string, at time.Time) {
		if l == label {
			fmu.Lock()
			fell = append(fell, at)
			fmu.Unlock()
		}
	}
	shown := 0
	for i := 0; i < 30; i++ {
		e.level(bob, "", store.LevelAddressed)
		e.read(bob)
		a := e.arm(bob, "")
		var wg sync.WaitGroup
		wg.Add(2)
		// Either may go first: half the rounds give the send a head start
		// of up to 2 ms, the other half the level change.
		lag := time.Duration(i/2%5) * 500 * time.Microsecond
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				time.Sleep(lag)
			}
			if _, err := e.s.Levels(ctx, bob, "", store.LevelAll); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if i%2 == 1 {
				time.Sleep(lag)
			}
			if _, err := e.s.Send(ctx, SendInput{Agent: alice, Text: fmt.Sprintf("room-wide %d", i)}); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		both := time.Now()
		if !a.woken() {
			t.Fatalf("round %d: the waiter was not woken", i)
		}
		want, err := e.s.inbox(ctx, label, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(want.Entries) == 0 {
			continue // the post came first and stays out (U3)
		}
		shown++
		es := a.result(t)
		if len(es) != len(want.Entries) || es[0].Key != want.Entries[0].Key {
			t.Fatalf("round %d: %+v, a plain look shows %+v", i, es, want.Entries)
		}
		fmu.Lock()
		for _, at := range fell {
			if at.After(both) {
				t.Fatalf("round %d: a fallback look at %v after both returned", i, at.Sub(both))
			}
		}
		fmu.Unlock()
	}
	t.Logf("the post was shown in %d of 30 rounds", shown)
}

// [wake] No lost wake-up under concurrent senders and waiters: every
// message reaches its addressee, each waiter loops inbox (waiting) and
// seen, and no waiter's wait ends unwoken (by the fallback look or at its
// deadline) after a message it is later shown was sent: such a wait was
// waiting when the message committed, and should have been woken. The
// send's return is the reference, so no allowance for timing. Run with
// the race detector.
func TestNoLostWakeUpWithManyConcurrentSendersAndWaiters(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	const waiters, senders, each = 12, 6, 25
	ws := make([]string, waiters)
	for i := range ws {
		ws[i] = e.agent(fmt.Sprintf("w%d", i))
		if i%3 == 0 { // some at level addressed: messages to them still wake them
			e.level(ws[i], "", store.LevelAddressed)
		}
	}
	ss := make([]string, senders)
	for i := range ss {
		ss[i] = e.agent(fmt.Sprintf("s%d", i))
	}
	want := make([]atomic.Int64, waiters)
	got := make([]atomic.Int64, waiters)
	var late atomic.Int64
	// sentAt is when each send returned: its wake was done by then.
	var sentAt sync.Map
	// unwoken is, per waiter, when each wait ended with no wake.
	var umu sync.Mutex
	unwoken := map[string][]time.Time{}
	e.s.fallback = func(label string, at time.Time) {
		umu.Lock()
		unwoken[label] = append(unwoken[label], at)
		umu.Unlock()
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			label := fmt.Sprintf("w%d", i)
			for {
				select {
				case <-done:
					if got[i].Load() == want[i].Load() {
						return
					}
				default:
				}
				v, err := e.s.Inbox(ctx, ws[i], "", 0, 2*time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				if len(v.Entries) == 0 {
					continue // the deadline, which ends a wait by the timer: counted as a fallback
				}
				umu.Lock()
				ends := unwoken[label]
				umu.Unlock()
				var rs []store.Read
				for _, x := range v.Entries {
					if s, ok := sentAt.Load(x.ID); ok {
						for _, u := range ends {
							if u.After(s.(time.Time)) {
								late.Add(1)
								t.Logf("%s: %s sent, then a wait ended unwoken %v later", label, x.ID, u.Sub(s.(time.Time)))
								break
							}
						}
					}
					rs = append(rs, store.Read{Key: x.Key, Version: x.Version})
					got[i].Add(1)
				}
				if _, err := e.s.Seen(ctx, ws[i], rs); err != nil {
					t.Error(err)
					return
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
				m, err := e.s.Send(ctx, SendInput{Agent: ss[k], Text: fmt.Sprintf("m %d %d", k, j), To: []string{ws[to]}})
				if err != nil {
					t.Error(err)
					return
				}
				sentAt.Store(m.ID, time.Now())
				if j%5 == 0 { // room-wide traffic too: it wakes only waiters at level all
					for i := range ws {
						if i%3 != 0 {
							want[i].Add(1)
						}
					}
					m, err := e.s.Send(ctx, SendInput{Agent: ss[k], Text: "room-wide"})
					if err != nil {
						t.Error(err)
						return
					}
					sentAt.Store(m.ID, time.Now())
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
		t.Fatalf("%d messages were shown only after a wait that ended unwoken", n)
	}
}

// BenchmarkATouchWriteScansTwoHundredWaiters is the cost of one narrow
// wake with 200 inbox waiters in the room, none of them woken: the scan
// that touchWrite does under the wakes' mutex.
func BenchmarkATouchWriteScansTwoHundredWaiters(b *testing.B) {
	var w roomWakes
	for i := 0; i < 200; i++ {
		x := w.waitInbox(fmt.Sprintf("a%d", i), "")
		w.setLevels(x, map[string]string{"room": store.LevelAll})
	}
	f := core.RecordFields{Kind: core.KindMessage, Agent: "sender", Text: "to someone else",
		To: []core.Addressee{{Node: "self", Agent: "nobody"}}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.touchWrite("room", "self", f)
	}
}
