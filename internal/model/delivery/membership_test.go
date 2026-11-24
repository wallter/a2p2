package delivery

import (
	"errors"
	"testing"
)

// Membership changes, with each machine acting on its own view.

// learnAll tells every machine every change.
func learnAll(w *World) {
	for _, id := range w.order {
		if w.Machine(id).Gone {
			continue
		}
		for more := true; more; {
			more = must(w.Learn(id))
		}
	}
}

func holdsText(w *World, m MachineID, id RecordID) bool {
	h := w.Machine(m).get(id)
	return h != nil && h.dropped == Live
}

// Rule 1. Requirements: T5, D2 (records count by authorship, not by who relays them)
func TestMessageOfARemovedAuthorReachesAMachineThatNeverMetItMarked(t *testing.T) {
	w := room("A", "B", "C")
	m := must(w.Send("A", "a", Msg{Body: "before"}))
	pull(t, w, "B", "A")
	must(0, w.Remove("A", "B"))
	learnAll(w)
	if _, err := w.Pull("C", "A", PullOpt{}); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("C pulled from removed A: %v", err)
	}
	pull(t, w, "C", "B")
	es := read(t, w, "C")
	wantTexts(t, es, "shown:before")
	if es[0].Record != m || !es[0].AuthorRemoved {
		t.Fatalf("entry %+v: want A's message marked author removed", es[0])
	}
}

// Rule 1. Requirements: T5, D6
func TestWithdrawWrittenBeforeRemovalAndArrivingAfterStillWorks(t *testing.T) {
	w := room("A", "B", "C")
	m := send(t, w, "A", "B", "oops")
	pull(t, w, "B", "A")
	wantTexts(t, read(t, w, "B"), "shown:oops")
	must(w.Withdraw("A", "a", m))
	pull(t, w, "C", "A") // only C has the withdraw
	must(0, w.Remove("A", "C"))
	learnAll(w)
	pull(t, w, "B", "C")
	es := read(t, w, "B")
	if len(es) != 1 || es[0].Kind != WithdrawnNotice || !es[0].AuthorRemoved {
		t.Fatalf("B shown %+v, want the withdraw notice, marked", es)
	}
	if holdsText(w, "B", m) {
		t.Fatal("B kept the withdrawn text")
	}
}

// Rule 1. Requirements: T5
func TestRecordWrittenAfterRemovalReachesNoHonestMemberButARelayedCopyIsKeptMarked(t *testing.T) {
	w := room("A", "B", "C")
	must(0, w.Remove("A", "C"))
	// B has not heard: A is still a member to it.
	after := must(w.Send("A", "a", Msg{Body: "after"}))
	for _, m := range []MachineID{"C"} {
		if _, err := w.Pull(m, "A", PullOpt{}); !errors.Is(err, ErrNotAllow) {
			t.Fatalf("%s, which knows, pulled from A: %v", m, err)
		}
		if _, err := w.Pull("A", m, PullOpt{Offer: true}); !errors.Is(err, ErrNotAllow) {
			t.Fatalf("A pulled from %s, which knows: %v", m, err)
		}
	}
	pull(t, w, "B", "A") // B still trusts A
	pull(t, w, "C", "B") // C trusts B: the record is A's, and A was a member
	if !holdsText(w, "C", after) {
		t.Fatal("C refused a record relayed by a member")
	}
	es := read(t, w, "C")
	if len(es) != 1 || !es[0].AuthorRemoved {
		t.Fatalf("C shown %+v, want it marked author removed", es)
	}
	// Once everyone knows, a later record reaches nobody.
	learnAll(w)
	late := must(w.Send("A", "a", Msg{Body: "later"}))
	for _, m := range []MachineID{"B", "C"} {
		_, err1 := w.Pull(m, "A", PullOpt{})
		_, err2 := w.Pull("A", m, PullOpt{Offer: true})
		if err1 == nil || err2 == nil || w.Machine(m).get(late) != nil {
			t.Fatalf("%s exchanged with A after learning: %v %v", m, err1, err2)
		}
	}
}

// Rule 1. Requirements: T1, T3
func TestVoidedAdmissionHidesAndDropsItsRecordsAndTellsReadersWhoSawThem(t *testing.T) {
	w := room("A", "B", "C")
	must(w.Admit("D", "A", true, "d"))
	m := must(w.Send("D", "d", Msg{Body: "from d"}))
	pull(t, w, "B", "D")
	pull(t, w, "C", "D")
	wantTexts(t, read(t, w, "B"), "shown:from d")
	must(0, w.VoidAdmission("D", "A"))
	// C has not heard yet; A refuses D's records even when a member relays them.
	if r := pull(t, w, "A", "C"); r.Stored != 0 || w.Machine("A").get(m) != nil {
		t.Fatalf("A stored a voided author's record: %+v", r)
	}
	learnAll(w)
	for _, id := range []MachineID{"A", "B", "C"} {
		if w.Machine(id).get(m) != nil {
			t.Fatalf("%s still holds the voided record", id)
		}
	}
	es := read(t, w, "B")
	if len(es) != 1 || es[0].Kind != VoidedNotice || es[0].Message != m {
		t.Fatalf("B shown %+v, want a voided notice for the message it saw", es)
	}
	if es := read(t, w, "C"); len(es) != 0 {
		t.Fatalf("C, which never saw it, shown %+v", es)
	}
}

// Rule 2: the stated limit. Requirements: T5
func TestStaleViewHandsTextToARemovedMachineUntilTheServerLearns(t *testing.T) {
	w := room("A", "B", "C")
	must(0, w.Remove("B", "C"))
	m1 := send(t, w, "A", "B", "one")
	r := pull(t, w, "B", "A") // A has not heard
	if len(r.TextTo) != 1 || !holdsText(w, "B", m1) {
		t.Fatalf("want the limit shown: A hands B text before it learns, got %+v", r)
	}
	if _, err := w.Pull("B", "C", PullOpt{}); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("C, which knows, served B: %v", err)
	}
	must(w.Learn("A"))
	m2 := send(t, w, "A", "B", "two")
	if _, err := w.Pull("B", "A", PullOpt{}); !errors.Is(err, ErrNotAllow) || w.Machine("B").get(m2) != nil {
		t.Fatalf("A served B after learning: %v", err)
	}
}

// Rule 2. Requirements: T5
func TestHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited(t *testing.T) {
	w := room("A", "B", "C")
	m := send(t, w, "A", "B", "waiting")
	hd := must(w.Hold("B", "A", PullOpt{}))
	must(0, w.Remove("B", "C"))
	must(w.Learn("A"))
	if r, err := w.Release(hd); !errors.Is(err, ErrNotAllow) || len(r.TextTo) != 0 {
		t.Fatalf("held response released to a removed peer: %+v %v", r, err)
	}
	if w.Machine("B").get(m) != nil {
		t.Fatal("B got the record")
	}
}

// Rule 2. Requirements: T3, U3
func TestNoHistoryNewcomerIsNeverHandedEarlierTextIncludingThroughAnOffer(t *testing.T) {
	w := room("A", "B", "C")
	old := send(t, w, "A", "B", "old")
	pull(t, w, "B", "A")
	must(w.Admit("D", "A", false, "d"))
	pull(t, w, "B", "D") // B now has D's summary: nothing
	r := must(w.Pull("B", "D", PullOpt{Offer: true}))
	if r.OfferStored != 1 || len(r.TextFrom) != 0 {
		t.Fatalf("offer to D: %+v; want the header only", r)
	}
	if h := w.Machine("D").get(old); h == nil || h.dropped != NotAdmitted {
		t.Fatalf("D holds %v", h)
	}
	nu := send(t, w, "A", "D", "new")
	r = pull(t, w, "D", "A")
	if len(r.TextTo) != 1 || r.TextTo[0] != nu {
		t.Fatalf("D was handed %v, want only the new text", r.TextTo)
	}
}

// Rule 2. Requirements: T3
func TestTwoGrantsThatDifferOnHistoryTheMoreRestrictiveApplies(t *testing.T) {
	w := room("A", "B", "C")
	old := must(w.Send("A", "a", Msg{Body: "old"}))
	pull(t, w, "B", "A")
	must(w.Grant("D", "A", true, "d"))
	must(w.Grant("D", "B", false))
	// A knows only its own grant, with history: it hands the text over, and
	// D, which knows both, does not keep it.
	r := pull(t, w, "D", "A")
	if len(r.TextTo) != 1 || holdsText(w, "D", old) {
		t.Fatalf("D got %+v and kept text %v", r, holdsText(w, "D", old))
	}
	learnAll(w)
	if a := w.Machine("A").st.view.adm["D"]; a.History || a.Heads[w.Machine("A").get(old).rec.Stream()] != 1 {
		t.Fatalf("A's view of D's permission: %+v", a)
	}
	w.Machine("D").get(old).dropped = Missing // as if D wanted it again
	if r := pull(t, w, "D", "A"); len(r.TextTo) != 0 {
		t.Fatalf("after learning both grants, A still hands D old text: %+v", r)
	}
}

// Rule 3. Requirements: N1, D2
func TestStaleOfferCostsBytesButNeverCorrectness(t *testing.T) {
	w := room("A", "B", "C")
	m := send(t, w, "A", "B", "x")
	pull(t, w, "C", "A")
	pull(t, w, "C", "B") // C's picture of B: nothing of A's
	pull(t, w, "B", "A")
	must(w.Withdraw("A", "a", m))
	pull(t, w, "B", "A") // B now holds m without text, withdrawn
	before := len(w.Machine("B").st.recs)
	r := must(w.Pull("C", "B", PullOpt{Offer: true}))
	if r.Offered != 1 || r.OfferStored != 0 {
		t.Fatalf("stale offer: %+v; want it sent and refused", r)
	}
	if len(w.Machine("B").st.recs) != before || holdsText(w, "B", m) {
		t.Fatal("the stale offer changed B")
	}
	// The response refreshed C's picture: the next offer carries nothing.
	if r := must(w.Pull("C", "B", PullOpt{Offer: true})); r.Offered != 0 {
		t.Fatalf("second offer: %+v", r)
	}
}

// Rule 3. Requirements: N1, D2
func TestOffersDoNotStarveOtherStreamsUnderAPageLimit(t *testing.T) {
	w := room("A", "B", "C")
	for i := 0; i < 3; i++ {
		must(w.Send("A", "a", Msg{Body: "a"}))
	}
	pull(t, w, "C", "A")
	pull(t, w, "C", "B") // stale from here: B is about to get A's records elsewhere
	pull(t, w, "B", "A")
	c := must(w.Send("C", "c", Msg{Body: "c"}))
	w.Machine("C").NoInbound = true
	for i := 0; i < 2 && w.Machine("B").get(c) == nil; i++ {
		must(w.Pull("C", "B", PullOpt{Offer: true, Limit: 1}))
	}
	if w.Machine("B").get(c) == nil {
		t.Fatal("C's own record did not get through within two limited offers")
	}
}

// Rule 3. Requirements: N1, D2
func TestMachineThatOnlyOpensRequestsStillConverges(t *testing.T) {
	w := room("A", "B", "C")
	w.Machine("C").NoInbound = true
	var ids []RecordID
	for _, m := range []MachineID{"A", "B", "C"} {
		ids = append(ids, must(w.Send(m, ag(m), Msg{Body: string(m)})))
	}
	if _, err := w.Pull("A", "C", PullOpt{}); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("A reached C: %v", err)
	}
	for round := 0; round < 2; round++ {
		for _, p := range []MachineID{"A", "B", "C"} {
			for _, s := range []MachineID{"A", "B"} {
				if p != s {
					must(w.Pull(p, s, PullOpt{Offer: true}))
				}
			}
		}
	}
	for _, m := range []MachineID{"A", "B", "C"} {
		for _, id := range ids {
			if !holdsText(w, m, id) {
				t.Fatalf("%s lacks %s", m, id)
			}
		}
	}
}

// Rule 2. Requirements: T3
func TestHeldResponseIsCheckedAgainstAPermissionNarrowedWhileItWaited(t *testing.T) {
	w := room("A", "B", "C")
	old := must(w.Send("A", "a", Msg{Body: "old"}))
	pull(t, w, "B", "A")
	must(w.Admit("D", "C", true, "d"))
	hd := must(w.Hold("D", "B", PullOpt{}))
	must(w.Grant("D", "B", false)) // B now holds a grant without history
	r := must(w.Release(hd))
	if r.Sent != 1 || len(r.TextTo) != 0 || holdsText(w, "D", old) {
		t.Fatalf("released %+v; want the header without its text", r)
	}
}
