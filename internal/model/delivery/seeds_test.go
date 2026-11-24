package delivery

import (
	"errors"
	"testing"
)

// The reduced scenarios of the disagreements the node's deep random
// schedules of the real exchange found at other seeds: what the model does
// in each, which the implementation is compared with
// (internal/node/pull_seeds_test.go).

// Seeds 1, 2 and 4 of the real exchange. Requirements: T3 (G4)
func TestNewcomerWithoutHistoryHoldsEarlierTextNotAdmittedWhileItsAdmittersHeadIsWithheld(t *testing.T) {
	w := room("A")
	m1 := send(t, w, "A", "A", "one")
	m2 := send(t, w, "A", "A", "two") // the head N is admitted from
	w.Withhold("A", m2)
	must(w.Admit("N", "A", false, "n"))
	pull(t, w, "N", "A")
	n := w.Machine("N")
	// The model knows positions: m1 is before the head, so not admitted,
	// though the head has not arrived. The implementation, which knows the
	// head only by id, holds m1 undecided until it does.
	if h := n.get(m1); h == nil || h.dropped != NotAdmitted || h.body != "" {
		t.Fatalf("N holds m1 as %+v; want not admitted, without text", h)
	}
	if n.get(m2) != nil {
		t.Fatal("N holds the withheld head")
	}
	delete(w.machines["A"].withhold, m2)
	pull(t, w, "N", "A")
	if h := n.get(m2); h == nil || h.dropped != NotAdmitted || h.body != "" {
		t.Fatalf("N holds m2 as %+v; want not admitted, without text", h)
	}
}

// Seed 6 of the real exchange. Requirements: N1, T3, T5
func TestOfferLimitedToOneIsBuiltEachPullOnTheLastSummary(t *testing.T) {
	w := room("M", "P", "Q")
	pull(t, w, "Q", "P") // P observes Q holding nothing
	q1 := send(t, w, "Q", "Q", "q1")
	p1 := send(t, w, "P", "P", "p1")
	must(w.Pull("M", "Q", PullOpt{Offer: true}))
	pull(t, w, "P", "M") // P now holds q1 too
	var from []RecordID
	for i := 0; i < 4; i++ {
		r := must(w.Pull("P", "Q", PullOpt{Offer: true, Limit: 1}))
		from = append(from, r.TextFrom...)
	}
	// The first offer is built on the old observation and carries p1 (P's
	// stream sorts first); the next is built on Q's summary from that pull,
	// which holds q1: Q is never offered its own message.
	if len(from) != 1 || from[0] != p1 {
		t.Fatalf("texts offered: %v; want only p1 (%v), not q1 (%v)", from, p1, q1)
	}
}

// Seed 7 of the real exchange. Requirements: T5
func TestRemovedNewcomerNotToldOfItsRemovalIsServedNothingMore(t *testing.T) {
	w := room("A")
	m1 := send(t, w, "A", "A", "one")
	m2 := send(t, w, "A", "A", "two")
	w.Withhold("A", m2)
	must(w.Admit("N", "A", false, "n"))
	pull(t, w, "N", "A")
	must(0, w.Remove("N", "A"))
	delete(w.machines["A"].withhold, m2)
	if _, err := w.Pull("N", "A", PullOpt{}); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("A served N after removing it: %v", err)
	}
	n := w.Machine("N")
	if n.get(m2) != nil || n.get(m1) == nil {
		t.Fatalf("N holds m1 %v, m2 %v; want m1 only", n.get(m1) != nil, n.get(m2) != nil)
	}
}

// Seeds 5, 8, 10 and 11 of the real exchange. Requirements: N1, D3
func TestAPullWhoseReplyIsLostGivesThePullerNoSummaryToOfferOn(t *testing.T) {
	w := room("A", "B")
	if _, err := w.Pull("B", "A", PullOpt{LostReply: true}); err != nil {
		t.Fatal(err)
	}
	b1 := send(t, w, "B", "A", "b1")
	if r := must(w.Pull("B", "A", PullOpt{Offer: true, LostReply: true})); len(r.TextFrom) != 0 {
		t.Fatalf("B offered %v with no summary of A", r.TextFrom)
	}
	pull(t, w, "B", "A") // now B has A's summary
	if r := must(w.Pull("B", "A", PullOpt{Offer: true})); len(r.TextFrom) != 1 || r.TextFrom[0] != b1 {
		t.Fatalf("B offered %v; want b1", r.TextFrom)
	}
}
