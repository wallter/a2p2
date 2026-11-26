package node

// Tests of the second surfaces pass: questions in alerts, a replaced
// question, and when text missing for a day is an alert.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func TestAlertsListQuestionsWaitingForThePerson(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	e.send(SendInput{Agent: alice, Text: "for everyone"})
	q := e.send(SendInput{Agent: alice, Text: "Ship it?", To: []string{e.s.HumanID()}})
	as, err := e.s.Alerts(ctx, 0)
	if err != nil || len(as) != 1 || as[0].Kind != AlertQuestion || as[0].ID != q.ID || as[0].Room != e.s.LocalRoom() ||
		as[0].Text != alice+" [this machine] asks: Ship it?" {
		t.Fatalf("alerts: %+v %v", as, err)
	}
}

// A question the person had read, then replaced by its author, is a
// question again: its replacement is listed with the new text.
func TestAReplacementOfAQuestionIsAQuestion(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	human := e.s.HumanID()
	q := e.send(SendInput{Agent: alice, Text: "Deploy to staging?", To: []string{human}})
	e.read(human)
	if qs, _ := e.s.Questions(ctx); len(qs) != 0 {
		t.Fatalf("read question still listed: %+v", qs)
	}
	e.send(SendInput{Agent: alice, Replaces: q.ID, Text: "Deploy to production?"})
	qs, err := e.s.Questions(ctx)
	if err != nil || len(qs) != 1 || qs[0].ID != q.ID || qs[0].Kind != EntryReplaced || qs[0].Text != "Deploy to production?" {
		t.Fatalf("replaced question: %+v %v", qs, err)
	}
}

func missingAlerts(t *testing.T, n *lnode) []string {
	t.Helper()
	as, err := n.r.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range as {
		if a.Kind == AlertMissing {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

func recordOf(t *testing.T, n *lnode, room, id string) *core.VerifiedRecord {
	t.Helper()
	for _, r := range n.records(room) {
		if r.ID() == id {
			return r
		}
	}
	t.Fatalf("no record %s", id)
	return nil
}

// Text that is undecided here (a head of this machine's admission
// frontier is not held yet) is not reported missing, however long it
// waits: whether it may be held at all is not known.
func TestNoMissingAlertForTextWhoseReadabilityIsUndecided(t *testing.T) {
	a, b, room := twoAdmins(t)
	a.register("alice")
	a.send(room, "alice", "u1")
	for _, r := range a.records(room) {
		b.take(r)
	}
	a2 := newLNodeAs(t, "a2", "a", a.clock)
	a2.pin(room, a)
	a2.register("alice")
	x1 := a2.send(room, "alice", "x1")
	e := newLNode(t, "e", a.clock)
	b.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	e.pin(room, b)
	e.pull(room, b)
	e.register("erin")
	if out := e.take(recordOf(t, a2, room, x1)); out != store.OutcomeStored {
		t.Fatalf("x1: %s", out)
	}
	if h, _ := e.held(room, x1); h.Reason != store.ReasonMissing {
		t.Fatalf("undecided text: %+v", h)
	}
	*e.clock = e.clock.Add(MissingAlertAfter + time.Hour)
	if ids := missingAlerts(t, e); len(ids) != 0 {
		t.Fatalf("undecided text alerted: %v", ids)
	}
}

// Text from before this machine's reading start (its admission frontier,
// with history or without) is not alerted; text after it is, until it
// passes retention by this machine's clock, even before a prune marks it.
func TestNoMissingAlertBeforeTheReadersStartOrPastRetention(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	a.register("alice")
	before := a.send(room, "alice", "before b was added")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	b.register("bea")
	after := a.send(room, "alice", "after b was added")
	for _, id := range []string{before, after} {
		if out := b.take(recordOf(t, a, room, id).WithoutBody()); out != store.OutcomeStored {
			t.Fatalf("%s: %s", id, out)
		}
	}
	*clock = clock.Add(MissingAlertAfter + time.Hour)
	if ids := missingAlerts(t, b); !slices.Equal(ids, []string{after}) {
		t.Fatalf("missing alerts %v, want only %s (before %s)", ids, after, before)
	}
	*clock = clock.Add(store.Retention)
	if ids := missingAlerts(t, b); len(ids) != 0 {
		t.Fatalf("text past retention alerted: %v", ids)
	}
}

func TestATooLongMessageSaysSoInPlainWords(t *testing.T) {
	e := newSurf(t)
	alice := e.agent("alice")
	_, err := e.s.Send(context.Background(), SendInput{Agent: alice, Text: strings.Repeat("x", core.MaxBodyBytes+1)})
	if !isUser(err) || !strings.Contains(err.Error(), "too long") || !strings.Contains(err.Error(), "16 KB") ||
		strings.Contains(err.Error(), "S3") || strings.Contains(err.Error(), "too_large") {
		t.Fatalf("too long: %v", err)
	}
}

// A room whose every other member has refused this machine as not a
// member for ten minutes is shown as perhaps removed; one member still
// answering, a shorter time, or the local room, is not.
func TestARoomEveryOtherMemberKeepsRefusingMayHaveRemovedThisMachine(t *testing.T) {
	e := newSurf(t)
	now := time.Now()
	refused := map[string]time.Time{}
	e.s.notMb = func(string) map[string]time.Time { return refused }
	r := RoomInfo{ID: "r_x", Members: []MemberInfo{{Node: e.s.Node()}, {Node: "p1"}, {Node: "p2"}}}
	if e.s.mayBeRemoved(r) {
		t.Fatal("nobody refused")
	}
	refused["p1"] = now.Add(-time.Hour)
	if e.s.mayBeRemoved(r) {
		t.Fatal("p2 still answers")
	}
	refused["p2"] = now.Add(-time.Minute)
	if e.s.mayBeRemoved(r) {
		t.Fatal("p2 refused only a minute ago")
	}
	refused["p2"] = now.Add(-MaybeRemovedAfter - time.Second)
	if !e.s.mayBeRemoved(r) {
		t.Fatal("every other member kept refusing")
	}
	if e.s.mayBeRemoved(RoomInfo{ID: e.s.LocalRoom(), Members: []MemberInfo{{Node: e.s.Node()}}}) {
		t.Fatal("the local room")
	}
}
