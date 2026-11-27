package node

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// A page or offer reads at most limit+1 records of each stream, not the
// rest of the stream (buildPage). On random requester details (positions,
// wanted text), records already sent and limits, over authors whose
// streams link back across restarts, the page cut at a limit is the start
// of the page built with no limit, and it says it is cut exactly when that
// one is longer: the same records in the same order.
func TestAPageCutAtItsLimitIsTheStartOfTheWholePage(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	room := a.create("team")
	a.register("alice")
	var ws []*lnode
	for i := 0; i < 2; i++ {
		w := newLNode(t, fmt.Sprintf("w%d", i), clock)
		a.change(room, Change{Kind: core.EntryAdd, Target: w.id, Name: fmt.Sprintf("w%d", i), History: true})
		w.pin(room, a.lnode)
		w.pull(room, a.lnode)
		w.register("wes")
		ws = append(ws, w)
	}
	for r := 0; r < 3; r++ {
		for i := 0; i < 6; i++ {
			a.send(room, "alice", fmt.Sprintf("a %d %d", r, i))
			for _, w := range ws {
				w.send(room, "wes", fmt.Sprintf("w %d %d", r, i))
			}
		}
		a.restartP()
		for _, w := range ws {
			w.restart()
			w.pull(room, a.lnode)
		}
	}
	for _, w := range ws { // streams in any order: until each is placed after what it links to
		rs := w.records(room)
		for pass := 0; len(rs) > 0 && pass < 10; pass++ {
			var later []*core.VerifiedRecord
			for _, r := range rs {
				switch out := a.take(r); out {
				case store.OutcomeStored, store.OutcomeHeld:
				case store.OutcomeLinkMissing, store.OutcomeNotNext:
					later = append(later, r)
				default:
					t.Fatalf("take: %s", out)
				}
			}
			rs = later
		}
		if len(rs) > 0 {
			t.Fatalf("%d records not placed", len(rs))
		}
	}
	rng := rand.New(rand.NewSource(7))
	st := a.state(room)
	all := map[string]bool{}
	for _, r := range a.records(room) {
		all[r.ID()] = true
	}
	err := a.m.view(ctx, room, func(v *roomView) error {
		var authors []string
		for _, s := range v.streams {
			if !slices.Contains(authors, s.Author) {
				authors = append(authors, s.Author)
			}
		}
		readable := readableBy(v, st.Permissions[ws[0].id])
		cuts := 0
		for i := 0; i < 400; i++ {
			want := map[string][]store.StreamDetail{}
			for _, s := range v.streams {
				if rng.Intn(4) == 0 {
					continue // holds nothing of it
				}
				c := rng.Int63n(s.Contig + 1)
				d := store.StreamDetail{E: s.Epoch, C: c}
				for p := int64(1); p <= c; p++ {
					if rng.Intn(5) == 0 {
						d.NB = append(d.NB, p)
						if rng.Intn(2) == 0 {
							d.W = append(d.W, p)
						}
					}
				}
				want[s.Author] = append(want[s.Author], d)
			}
			skip := map[string]bool{}
			for id := range all {
				if rng.Intn(6) == 0 {
					skip[id] = true
				}
			}
			limit := 1 + rng.Intn(12)
			take := authors
			if rng.Intn(2) == 0 { // one author: one stream may fill the page alone
				i := rng.Intn(len(authors))
				take = authors[i : i+1]
			}
			// Each in a view of its own: what one read loaded must not help the other.
			w := a.m.newView(v.rd, room, v.now)
			if err := w.load(); err != nil {
				return err
			}
			whole, _, err := a.x.buildPage(w, readableBy(w, st.Permissions[ws[0].id]), want, take, skip, nil, 1<<20, 1<<30)
			if err != nil {
				return err
			}
			page, cut, err := a.x.buildPage(v, readable, want, take, skip, nil, limit, 1<<30)
			if err != nil {
				return err
			}
			n := min(limit, len(whole))
			if cut != (len(whole) > limit) || len(page) != n {
				return fmt.Errorf("limit %d: %d records, cut %v; the whole page has %d", limit, len(page), cut, len(whole))
			}
			for j := range page {
				if page[j].r.id != whole[j].r.id || page[j].text != whole[j].text {
					return fmt.Errorf("limit %d: record %d differs", limit, j)
				}
			}
			if cut {
				cuts++
			}
			v = a.m.newView(v.rd, room, v.now) // a fresh view each time
			if err := v.load(); err != nil {
				return err
			}
			readable = readableBy(v, st.Permissions[ws[0].id])
		}
		if cuts < 50 {
			return fmt.Errorf("only %d pages were cut: the test tested little", cuts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A machine admitted without history decides the text of each record it
// receives with only the record and the streams it holds: no view of the
// room is read when the permission is all or nothing (textAccess). Text
// written before the frontier in an earlier stream of the author, which
// the frontier's stream links back to, is still held without text, and
// text after it with text.
func TestTextBeforeTheFrontierInAnEarlierStreamIsHeldWithoutTextByAMachineAdmittedWithoutHistory(t *testing.T) {
	clock := lclock()
	a := newLNode(t, "a", clock)
	room := a.create("team")
	a.register("alice")
	m0 := a.send(room, "alice", "first stream")
	a.restart()
	m1 := a.send(room, "alice", "second stream, before e")
	m1b := a.send(room, "alice", "the frontier, not the stream's first")
	e := newLNode(t, "e", clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	if p := a.state(room).Permissions[e.id]; p.All || len(p.From[a.id]) != 1 {
		t.Fatalf("e's permission: %+v", p)
	}
	e.pin(room, a)
	e.pull(room, a)
	m2 := a.send(room, "alice", "after e")
	epochs := map[string]bool{}
	rs := a.records(room)
	for _, r := range rs {
		epochs[r.Fields().Epoch] = true
	}
	if len(epochs) < 2 {
		t.Fatalf("alice wrote in %d streams", len(epochs))
	}
	for pass := 0; len(rs) > 0 && pass < 5; pass++ {
		var later []*core.VerifiedRecord
		for _, r := range rs {
			switch out := e.take(r); out {
			case store.OutcomeStored, store.OutcomeHeld:
			case store.OutcomeLinkMissing, store.OutcomeNotNext:
				later = append(later, r)
			default:
				t.Fatalf("take: %s", out)
			}
		}
		rs = later
	}
	for _, id := range []string{m0, m1, m1b} {
		if h, ok := e.held(room, id); !ok || h.Reason != store.ReasonNotAdmitted || h.Record.Body() != nil {
			t.Fatalf("text before the frontier: %+v %v", h, ok)
		}
	}
	if h, _ := e.held(room, m2); h.Reason != store.ReasonLive {
		t.Fatalf("text after it: %+v", h)
	}
}
