package delivery

import (
	"errors"
	"testing"
)

// The schedules from the second review
// (docs/research/decisions/model-delivery-rereview-sol.md), one test each.

// Rereview 1. Requirements: D5
func TestOnlyAnAgentShownTheTextMayReportHandled(t *testing.T) {
	w := room("A", "B")
	ask := must(w.Send("A", "a", Msg{Body: "all: confirm", AskHandled: true}))
	pull(t, w, "B", "A")
	must(w.Admit("N", "B", false, "n"))
	pull(t, w, "N", "B") // N holds only the header
	if _, err := w.Handle("N", "n", ask); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("N reported handling a message it was never shown: %v", err)
	}
	if _, err := w.Handle("B", "b", ask); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("B reported handling before being shown it: %v", err)
	}
	read(t, w, "B")
	must(w.Handle("B", "b", ask))
}

// Rereview 2. Requirements: D4
func TestRestartAfterFetchingBothBranchesLinksToBoth(t *testing.T) {
	w := room("A", "B", "C")
	empty := w.Backup("A")
	m1 := send(t, w, "A", "C", "m1")
	pull(t, w, "B", "A")
	w.Restore("A", empty)
	m2 := send(t, w, "A", "C", "m2") // a branch beside m1
	pull(t, w, "A", "B")             // A now holds both branches
	w.Crash("A")
	m3 := send(t, w, "A", "C", "m3") // after A caught up
	pull(t, w, "C", "B")
	read(t, w, "C") // C confirms m1
	pull(t, w, "C", "A")
	es := read(t, w, "C")
	late := map[RecordID]bool{}
	for _, e := range es {
		late[e.Record] = e.Late
	}
	if !late[m2] || late[m3] {
		t.Fatalf("late: m2 %v (want true: beside m1), m3 %v (want false: after both)", late[m2], late[m3])
	}
	_ = m1
}

// Rereview 3. Requirements: D7
func TestTextExpiredHereStaysExpiredWhenTheClockIsSetBack(t *testing.T) {
	w := room("A", "C")
	m := send(t, w, "A", "C", "x")
	pull(t, w, "C", "A")
	w.Advance(25 * Day)
	w.SetSkew("C", 6*Day) // C's clock passes 30 days: text dropped here
	w.SetSkew("C", 0)     // and is set back
	pull(t, w, "C", "A")  // A still has the text
	c := w.Machine("C")
	if h := c.get(m); h.dropped != Expired || !c.reasonHolds(h, w.Now) {
		t.Fatalf("C holds %v; want expired, and recorded as evidence", h.dropped)
	}
	wantTexts(t, read(t, w, "C"), "expired:")
}

// Rereview 4. Requirements: D2, D6
func TestTextThePullerWillRefuseIsNeverOfferedAndCannotStarveAnotherStream(t *testing.T) {
	w := room("A", "Z", "B", "C")
	w.tamper = func(srv MachineID, it wire) wire { // C gets a1 without its text
		it.body, it.hasBody = "", false
		return it
	}
	send(t, w, "A", "C", "a1")
	pull(t, w, "C", "A")
	w.tamper = nil
	w.Advance(25 * Day)
	w.SetSkew("C", 6*Day) // C's clock passes a1's 30 days: C no longer wants its text
	send(t, w, "Z", "C", "z1")
	pull(t, w, "B", "A") // B, on a true clock, still holds a1's text
	pull(t, w, "B", "Z")
	w.Sleep("A")
	w.Sleep("Z")
	for i := 0; i < 3; i++ {
		r := must(w.Pull("C", "B", PullOpt{Limit: 1}))
		if r.Sent != r.Stored {
			t.Fatalf("pull %d: %+v, a slot spent on something refused", i, r)
		}
	}
	wantTexts(t, read(t, w, "C"), "expired: shown:z1")
}

// Rereview 5. Requirements: D9
func TestReplacementWinnerIsTheSameForEveryArrivalOrder(t *testing.T) {
	w := room("A", "B")
	m := send(t, w, "A", "B", "v0")
	pull(t, w, "B", "A")
	bk := w.Backup("A")
	w.SetSkew("A", 2*Day)
	r1 := must(w.Replace("A", "a", m, "R1"))
	w.SetSkew("A", 0)
	r2 := must(w.Replace("A", "a", m, "R2")) // follows R1 in A's history, earlier clock
	pull(t, w, "B", "A")
	w.Restore("A", bk)
	w.SetSkew("A", 1*Day)
	r3 := must(w.Replace("A", "a", m, "R3")) // a branch beside R1, R2
	pull(t, w, "B", "A")
	b := w.Machine("B")
	recs := map[RecordID]*held{}
	for _, id := range []RecordID{m, r1, r2, r3} {
		recs[id] = b.get(id)
	}
	want := ""
	for _, order := range [][]RecordID{{r1, r2, r3}, {r1, r3, r2}, {r3, r1, r2}, {r2, r3, r1}, {r3, r2, r1}, {r2, r1, r3}} {
		x := room("X")
		mx := x.Machine("X")
		mx.insert(recs[m].rec, recs[m].body, true, w.Now)
		for _, id := range order {
			mx.insert(recs[id].rec, recs[id].body, true, w.Now)
		}
		text, _, _ := mx.currentText(mx.get(m))
		if want == "" {
			want = text
		}
		if text != want {
			t.Fatalf("arrival order %v gives %q, another gives %q", order, text, want)
		}
	}
	if want != "R2" {
		t.Fatalf("winner %q, want R2 (the longest chain of replacements)", want)
	}
}

// Rereview 5, the pruning check made honest. Requirements: D7
func TestThreadAndBranchDropExpiredTextThemselves(t *testing.T) {
	w := room("A", "C")
	root := must(w.Send("A", "a", Msg{Body: "old"}))
	pull(t, w, "C", "A")
	w.Machine("C").Skew = 31 * Day // no prune on the way in
	for _, es := range [][]Entry{w.Thread("C", root), w.Branch("C", root)} {
		if len(es) != 1 || es[0].Kind != ExpiredNotice {
			t.Fatalf("shown %s", texts(es))
		}
	}
}

// Rereview 6: what "late" promises. Requirements: D4
func TestLateMeansAfterAConfirmedRecordItDoesNotFollowNotAfterAnUnconfirmedShow(t *testing.T) {
	w := room("A", "C")
	empty := w.Backup("A")
	send(t, w, "A", "C", "m1")
	pull(t, w, "C", "A")
	must(w.Inbox("C", "c")) // shown, not confirmed: not remembered
	w.Restore("A", empty)
	send(t, w, "A", "C", "m2") // a branch beside m1
	pull(t, w, "C", "A")
	es := read(t, w, "C")
	if len(es) != 2 || es[0].Late || !es[1].Late {
		t.Fatalf("listing %s: want the second of two unordered branches marked late, the first not", texts(es))
	}
}
