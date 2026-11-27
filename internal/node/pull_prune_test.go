package node

import (
	"context"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// A summary that counts live text expired by the clock asks for the
// pruning pass, as reading that text did when every summary loaded the
// room's view: an idle opening now reads no record, only the store's
// summary. Pruned as asked, the text stays without text when the clock is
// then set back to before its expiry.
func TestASummaryThatCountsTextPastRetentionAsksForPruningAndTheClockGoingBackDoesNotReviveIt(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a := newLNode(t, "a", clock)
	room := a.create("team")
	a.register("alice")
	a.send(room, "alice", "the first of its stream")
	sum := func() (store.StreamDetail, error) {
		var d store.StreamDetail
		err := a.m.viewLazy(ctx, room, func(v *roomView) error {
			s, err := a.r.summarize(v, a.state(room))
			if err == nil && len(s.detail[a.id]) > 0 {
				d = s.detail[a.id][0]
			}
			return err
		})
		return d, err
	}
	if err := a.m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(store.Retention + time.Hour)
	d, err := sum()
	if err != nil {
		t.Fatal(err)
	}
	if d.X != 1 || !a.m.PruneDue() {
		t.Fatalf("past retention: detail %+v, pruning due %v", d, a.m.PruneDue())
	}
	if err := a.m.pruneIfDue(ctx); err != nil { // as the daemon does
		t.Fatal(err)
	}
	*clock = clock.Add(-2 * 24 * time.Hour)
	if d, err = sum(); err != nil || d.X != 1 {
		t.Fatalf("the clock set back: detail %+v (%v): the text came back", d, err)
	}
}
