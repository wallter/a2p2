package node

// Tests for the findings of docs/research/decisions/step4a-review-sol.md.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// twoAdmins is a room created by a with b made admin, both holding the log.
func twoAdmins(t *testing.T) (a, b *lnode, room string) {
	t.Helper()
	clock := lclock()
	a, b = newLNode(t, "a", clock), newLNode(t, "b", clock)
	room = a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	return a, b, room
}

func logRows(t *testing.T, n *lnode, room string) []store.LogRow {
	t.Helper()
	rows, err := n.st.Log(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A batch that fails at any point before its commit leaves memory and the
// store exactly as they were, and a retry of the same batch acts as a
// first attempt: the vote is decided and the second admin's request
// raised. A cancelled context does not evict the room. A failure
// after the commit leaves memory equal to the store and is not reported
// as a failure (step 5a review).
//
// Requirements: DESIGN-next section 5; review blocker 2
func TestIngressFailureLeavesTheRoomAsItWasAndARetryActsAsTheFirst(t *testing.T) {
	for _, point := range []string{"ingest:requests", "ingest:apply", "cancelled", "ingest:committed"} {
		t.Run(point, func(t *testing.T) {
			a, b, room := twoAdmins(t)
			c := newLNode(t, "c", a.clock)
			vote := a.change(room, Change{Kind: core.EntryAdmin, Target: c.id, Name: "sea"})
			w := a.r.rooms[room].ents[vote.Entry].Wire()
			before, rowsBefore := b.state(room), logRows(t, b, room)
			ctx := context.Background()
			if point == "cancelled" {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			} else {
				testHook = func(p string) error {
					if p == point {
						return errors.New("injected")
					}
					return nil
				}
			}
			_, err := b.r.Receive(ctx, room, 0, w)
			testHook = nil
			if point == "ingest:committed" {
				// Step 5a review: a failure after the commit is not
				// reported as a failure of what was committed.
				if err != nil {
					t.Fatalf("committed batch reported as failed: %v", err)
				}
				// Committed: memory is the store, and a restart agrees.
				after := b.state(room)
				b.restart()
				sameState(t, "after commit", after, b.state(room))
				if len(b.requests(room)) != 1 {
					t.Fatalf("request after commit: %+v", b.requests(room))
				}
				if res, err := b.r.Receive(context.Background(), room, 0, w); err != nil || res[0].Disposition != members.Accepted {
					t.Fatalf("retry after commit: %+v %v", res, err)
				}
				return
			}
			if err == nil {
				t.Fatal("failure not reported")
			}
			if _, err := b.r.State(room); err != nil {
				t.Fatalf("room evicted: %v", err)
			}
			sameState(t, "after failure", before, b.state(room))
			if rows := logRows(t, b, room); len(rows) != len(rowsBefore) || len(b.requests(room)) != 0 {
				t.Fatalf("written: %d rows, %d requests", len(rows), len(b.requests(room)))
			}
			res, err := b.r.Receive(context.Background(), room, 0, w)
			if err != nil || res[0].Disposition != members.Accepted || res[0].ID != vote.Entry {
				t.Fatalf("retry: %+v %v", res, err)
			}
			if qs := b.requests(room); len(qs) != 1 || qs[0].Seconds != vote.Entry {
				t.Fatalf("retry raised: %+v", qs)
			}
			after := b.state(room)
			b.restart()
			sameState(t, "retry then restart", after, b.state(room))
		})
	}
}

// A request records exactly what is approved. If the room changed so that
// signing would now do something else (the member to promote was removed,
// so signing would admit it; the machine to admit as an admin was added,
// so signing would only promote it and drop the history choice), it is
// refused with the reason and nothing is signed.
//
// Requirements: P3; review blocker 3
func TestConfirmRefusesARequestWhoseActionWouldNowBeDifferent(t *testing.T) {
	a, b, room := twoAdmins(t)
	c, d := newLNode(t, "c", a.clock), newLNode(t, "d", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
	b.pull(room, a)
	promote := a.propose(room, Change{Kind: core.EntryAdmin, Target: c.id})
	if promote.Action != ActionPromote || promote.Role != "admin" {
		t.Fatalf("recorded: %+v", promote)
	}
	admitAdmin := a.propose(room, Change{Kind: core.EntryAdmin, Target: d.id, Name: "dee", History: true})
	if admitAdmin.Action != ActionAdmitAdmin || !admitAdmin.History {
		t.Fatalf("recorded: %+v", admitAdmin)
	}
	b.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	b.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee"})
	a.pull(room, b)
	signs := a.r.signs
	for _, x := range []struct {
		q    store.Request
		want string
	}{{promote, ReasonActionMoved + ActionAdmitAdmin + " instead of " + ActionPromote},
		{admitAdmin, ReasonActionMoved + ActionPromote + " instead of " + ActionAdmitAdmin}} {
		got, err := a.r.Confirm(context.Background(), x.q.ID, true)
		if err != nil || got.State != store.RequestFailed || got.Reason != x.want {
			t.Fatalf("%s: %+v %v", x.q.Action, got, err)
		}
	}
	if a.r.signs != signs {
		t.Fatal("a changed request was signed")
	}
	// A request raised to second another admin's entry records its action too.
	e := newLNode(t, "e", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	a.change(room, Change{Kind: core.EntryAdmin, Target: e.id})
	b.pull(room, a)
	qs := b.requests(room)
	if len(qs) != 1 || qs[0].Action != ActionPromote || qs[0].Role != "admin" || qs[0].Seconds == "" {
		t.Fatalf("raised: %+v", qs)
	}
}

// Decisions on one request are serialised: two approvals and a decline at
// once sign at most once, and exactly one decision is recorded.
//
// Requirements: P3; review should-fix 3
func TestConcurrentDecisionsOnOneRequestSignAtMostOnce(t *testing.T) {
	for i := 0; i < 5; i++ {
		clock := lclock()
		a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
		room := a.create("team")
		q := a.propose(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
		var wg sync.WaitGroup
		var mu sync.Mutex
		decided := 0
		for _, approve := range []bool{true, true, false} {
			wg.Add(1)
			go func(approve bool) {
				defer wg.Done()
				if c, err := a.r.Confirm(context.Background(), q.ID, approve); err == nil && c.State != store.RequestFailed {
					mu.Lock()
					decided++
					mu.Unlock()
				}
			}(approve)
		}
		wg.Wait()
		ws, _ := a.r.Offer(room)
		if a.r.signs > 1 || decided != 1 || len(ws) != 1+a.r.signs {
			t.Fatalf("signs %d, decisions %d, entries %d", a.r.signs, decided, len(ws))
		}
	}
}

// A restart judges the dependants of an entry found invalid as before:
// an entry whose previous entry is another author's is invalid, and so is
// a later child of it, whether or not the machine restarted in between.
//
// Requirements: DESIGN-next section 5 (replay is deterministic); members rule M11; review should-fix 1
func TestRestartJudgesDependantsOfAnInvalidEntryAsBefore(t *testing.T) {
	a, b, room := twoAdmins(t)
	adminB := ""
	for id, v := range a.r.rooms[room].ents {
		if v.Fields().Kind == core.EntryAdmin {
			adminB = id
		}
	}
	sign := func(f core.EntryFields) *core.Entry {
		f.Room, f.Kind, f.Target, f.Name, f.Created = room, core.EntryAdd, a.id, "x", *a.clock
		e, err := core.NewEntry(b.key, f)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	x := sign(core.EntryFields{Seq: 2, Prev: adminB, Parents: []string{adminB}}) // prev is a's entry
	y := sign(core.EntryFields{Seq: 3, Prev: x.ID(), Parents: []string{x.ID()}})
	obs := newLNode(t, "obs", a.clock)
	obs.pin(room, a)
	obs.pull(room, a)
	res, err := obs.r.Receive(context.Background(), room, 0, x.Wire())
	if err != nil || res[0].Disposition != members.Invalid || res[0].Reason != members.ReasonBadChain {
		t.Fatalf("x: %+v %v", res, err)
	}
	obs.restart()
	res, err = obs.r.Receive(context.Background(), room, 0, y.Wire())
	if err != nil || res[0].Disposition != members.Invalid || res[0].Reason != members.ReasonInvalidParent {
		t.Fatalf("child after restart: %+v %v", res, err)
	}
	obs.restart()
	if got := obs.r.rooms[room].state[y.ID()]; got.State != store.LogInvalid {
		t.Fatalf("stored: %+v", got)
	}
	if ws, _ := obs.r.Offer(room); len(ws) != 2 {
		t.Fatalf("invalid entries passed on: %d", len(ws))
	}
}

// A frontier head not held yet makes that author's text undecided: the
// record is stored without text as missing, the reader waits at it and
// Seen does not move past it; once the head arrives the text is readable
// and, repaired, shown.
//
// Requirements: RM2, R7; review should-fix 2
func TestUndecidedHistoryKeepsTheReaderWaitingUntilTheHeadArrives(t *testing.T) {
	a, b, room := twoAdmins(t)
	a.register("alice")
	u1 := a.send(room, "alice", "u1")
	for _, r := range a.records(room) {
		b.take(r)
	}
	// A blank restore of a's key writes a stream that links nothing.
	a2 := newLNodeAs(t, "a2", "a", a.clock)
	a2.pin(room, a)
	a2.register("alice")
	x1 := a2.send(room, "alice", "x1")
	e := newLNode(t, "e", a.clock)
	b.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	e.pin(room, b)
	e.pull(room, b)
	e.register("erin")
	if p := e.state(room).Permissions[e.id]; !reflect.DeepEqual(p.From, map[string][]string{a.id: {u1}}) {
		t.Fatalf("frontier: %+v", p)
	}
	var rx1, ru1 *core.VerifiedRecord
	for _, r := range a2.records(room) {
		rx1 = r
	}
	for _, r := range a.records(room) {
		if r.ID() == u1 {
			ru1 = r
		}
	}
	if out := e.take(rx1); out != store.OutcomeStored {
		t.Fatalf("x1: %s", out)
	}
	if h, _ := e.held(room, x1); h.Reason != store.ReasonMissing || h.Record.Body() != nil {
		t.Fatalf("undecided text: %+v", h)
	}
	v, err := e.m.Inbox(context.Background(), room, "erin")
	// Nothing is shown and nothing waits until the head arrives: erin's
	// cursors start at it (U3; step 5a, which the delivery model decided).
	if err != nil || len(v.Entries) != 0 || len(v.Waiting) != 0 {
		t.Fatalf("erin before the head: %+v %v", v, err)
	}
	if _, err := e.m.Seen(context.Background(), room, "erin", nil); err != nil {
		t.Fatal(err)
	}
	if cs := cursors(t, e, "erin", room); len(cs) != 0 {
		t.Fatalf("cursor moved past undecided text: %+v", cs)
	}
	e.take(ru1)
	if h, _ := e.held(room, u1); h.Reason != store.ReasonNotAdmitted {
		t.Fatalf("head: %+v", h)
	}
	if out := e.take(rx1); out != store.OutcomeRepaired {
		t.Fatalf("repair: %s", out)
	}
	if es := e.inbox(room, "erin"); len(es) != 1 || es[0].ID != x1 || es[0].Text != "x1" {
		t.Fatalf("erin after the head: %+v", es)
	}
	if cs := cursors(t, e, "erin", room); len(cs) != 1 || cs[0].Pos != 1 || cs[0].Epoch != ru1.Fields().Epoch {
		t.Fatalf("seeded cursor: %+v", cs)
	}
}

func cursors(t *testing.T, n *lnode, agent, room string) []store.Cursor {
	t.Helper()
	var cs []store.Cursor
	if err := n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		cs, err = rd.Cursors(agent, room)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return cs
}

// shrinkAdmitted runs the members example where Admitted shrinks: a adds
// and removes c while b, having seen neither, adds and removes c. It
// returns c's message, held by a and confirmed there by alice first.
func shrinkAdmitted(t *testing.T) (a *lnode, room, msg string) {
	t.Helper()
	a, b, room := twoAdmins(t)
	a.register("alice")
	c := newLNode(t, "c", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, a)
	c.pull(room, a)
	c.register("carol")
	msg = c.send(room, "carol", "from c")
	for _, r := range c.records(room) {
		a.take(r)
	}
	es := a.inbox(room, "alice")
	if len(es) != 1 {
		t.Fatalf("inbox: %+v", es)
	}
	if res, err := a.m.Seen(context.Background(), room, "alice", []store.Read{{Key: es[0].Key, Version: es[0].Version}}); err != nil || !res[0].Confirmed {
		t.Fatalf("seen: %+v %v", res, err)
	}
	a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	b.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
	b.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	a.pull(room, b)
	wantNodes(t, "admitted", a.state(room).Admitted, a.id, b.id)
	return a, room, msg
}

// A reader that confirmed text whose author left Admitted gets one
// confirmable "voided" notice; Branch no longer shows the text; the
// record is kept.
//
// Requirements: RM1; members rule M13; review should-fix 4
func TestVoidedAuthorsTextLeavesBranchAndItsReadersAreTold(t *testing.T) {
	a, room, msg := shrinkAdmitted(t)
	if br, err := a.m.Branch(context.Background(), room, msg); err != nil || len(br) != 0 {
		t.Fatalf("branch: %+v %v", br, err)
	}
	es := a.inbox(room, "alice")
	if len(es) != 1 || es[0].Kind != EntryVoided || es[0].ID != msg || es[0].Text != "" {
		t.Fatalf("voided notice: %+v", es)
	}
	if res, err := a.m.Seen(context.Background(), room, "alice", []store.Read{{Key: es[0].Key}}); err != nil || !res[0].Confirmed {
		t.Fatalf("confirm notice: %+v %v", res, err)
	}
	if es := a.inbox(room, "alice"); len(es) != 0 {
		t.Fatalf("after confirming: %+v", es)
	}
	if h, ok := a.held(room, msg); !ok || h.Reason != store.ReasonLive {
		t.Fatalf("record: %+v %v", h, ok)
	}
}

// A newly admitted machine's agents start exactly at the admitter's
// positions, with history or without; history decides only whether
// earlier text is readable [R8].
//
// Requirements: R8, T3; review should-fix 5
func TestNewcomersAgentsStartAtTheAdmittersPositionsWithOrWithoutHistory(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(map[bool]string{false: "without", true: "with"}[history], func(t *testing.T) {
			if history && !fromWithHistory {
				t.Skip("internal/core refuses from together with history (PROTOCOL-next section 3, S7); " +
					"until it accepts both, a grant with history carries no frontier and its agents start at 0")
			}
			clock := lclock()
			a, e := newLNode(t, "a", clock), newLNode(t, "e", clock)
			room := a.create("team")
			a.register("alice")
			m1 := a.send(room, "alice", "one")
			a.send(room, "alice", "two")
			a.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee", History: history})
			e.pin(room, a)
			e.pull(room, a)
			e.register("erin")
			m3 := a.send(room, "alice", "three")
			for _, r := range a.records(room) {
				e.take(r)
			}
			if cs := cursors(t, e, "erin", room); len(cs) != 1 || cs[0].Pos != 2 || cs[0].Start != 2 {
				t.Fatalf("cursor: %+v", cs)
			}
			if es := e.inbox(room, "erin"); len(es) != 1 || es[0].ID != m3 {
				t.Fatalf("inbox: %+v", es)
			}
			want := map[bool]string{false: store.ReasonNotAdmitted, true: store.ReasonLive}[history]
			if h, _ := e.held(room, m1); h.Reason != want {
				t.Fatalf("earlier text: %+v", h)
			}
		})
	}
}

// Bounds and cost: a batch that changes nothing writes nothing; arrival
// positions are unique; pending requests and refused dependants are
// capped, fork evidence kept.
//
// Requirements: review should-fix 6
func TestBoundsAndBatchesThatChangeNothing(t *testing.T) {
	a, b, room := twoAdmins(t)
	testHook = func(p string) error {
		if p == "ingest:apply" {
			return errors.New("a batch that changes nothing reached the store")
		}
		return nil
	}
	b.pull(room, a)
	testHook = nil

	seen := map[[2]int64]bool{}
	for _, r := range logRows(t, b, room) {
		k := [2]int64{r.Batch, int64(r.Pos)}
		if seen[k] {
			t.Fatalf("arrival position %v used twice", k)
		}
		seen[k] = true
	}

	other := a.create("other")
	target := func(i int) string { return strings.Repeat("0", 60) + fmt.Sprintf("%04x", i+1) }
	for i := 0; i < MaxPendingRequests; i++ {
		a.propose(other, Change{Kind: core.EntryAdd, Target: target(i), Name: "x"})
	}
	if _, err := a.r.Propose(context.Background(), other, Change{Kind: core.EntryAdd, Target: target(999), Name: "x"}); err == nil ||
		!strings.Contains(err.Error(), ReasonTooMany) {
		t.Fatalf("over the request cap: %v", err)
	}

	// A fork, then more dependants of its refused branch than are kept.
	d := newLNode(t, "d", a.clock)
	addD := a.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee"})
	fork := signFork(t, a, room, addD.Entry, b.id)
	obs := newLNode(t, "obs", a.clock)
	obs.pin(room, a)
	obs.pull(room, a)
	wires := [][]byte{fork}
	prev, parents := "", []string{coreID(t, fork)}
	for i := 1; i <= MaxRefusedDependants+3; i++ {
		e, err := core.NewEntry(d.key, core.EntryFields{Room: room, Seq: int64(i), Prev: prev, Parents: parents,
			Kind: core.EntryAdd, Target: a.id, Name: "x", Created: *a.clock})
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, e.Wire())
		prev, parents = e.ID(), []string{e.ID()}
	}
	res, err := obs.r.Receive(context.Background(), room, 0, wires...)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Disposition != members.Refused || res[0].Reason != members.ReasonForkRefused {
		t.Fatalf("evidence: %+v", res[0])
	}
	refused, dropped := 0, 0
	for _, x := range res[1:] {
		switch {
		case x.Disposition == members.Refused:
			refused++
		case x.Disposition == members.Dropped && x.Reason == ReasonOverLimit:
			dropped++
		}
	}
	if refused != MaxRefusedDependants || dropped != 3 {
		t.Fatalf("refused %d, dropped %d", refused, dropped)
	}
	st := obs.state(room)
	obs.restart()
	sameState(t, "bounded room after restart", st, obs.state(room))
	if info, _ := obs.r.Info(context.Background(), room); len(info.Forks) != 1 {
		t.Fatalf("fork evidence: %+v", info.Forks)
	}
}

func coreID(t *testing.T, wire []byte) string {
	t.Helper()
	e, err := core.ParseEntry(wire)
	if err != nil {
		t.Fatal(err)
	}
	return e.ID()
}

// A removed machine may still write locally; its own reader marks those
// records as from a removed author. A grant that would need more than 16
// heads of one author is refused, naming the author.
//
// Requirements: RM1, PROTOCOL-next section 3; review notes
func TestRemovedMachineMarksItsOwnRecordsAndGrantsOverTheHeadLimitAreRefused(t *testing.T) {
	a, b, room := twoAdmins(t)
	c := newLNode(t, "c", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
	c.pin(room, a)
	c.pull(room, a)
	c.register("carol")
	c.register("cid")
	a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	c.pull(room, a)
	c.send(room, "carol", "still writing")
	if es := c.inbox(room, "cid"); len(es) != 1 || !es[0].AuthorRemoved {
		t.Fatalf("own reader: %+v", es)
	}

	// b's key restored blank 17 times: 17 streams that link nothing.
	for i := 0; i <= maxHeadsPerAuthor; i++ {
		x := newLNodeAs(t, "b"+string(rune('a'+i)), "b", a.clock)
		x.pin(room, a)
		x.register("bob")
		x.send(room, "bob", "hi")
		for _, r := range x.records(room) {
			a.take(r)
		}
	}
	e := newLNode(t, "e", a.clock)
	q := a.propose(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	signs := a.r.signs
	_, err := a.r.Confirm(context.Background(), q.ID, true)
	if err == nil || !strings.Contains(err.Error(), short(b.id)) {
		t.Fatalf("over the head limit: %v", err)
	}
	if a.r.signs != signs {
		t.Fatal("signed a grant over the head limit")
	}
}
