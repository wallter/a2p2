package delivery

import (
	"strings"
	"testing"
)

// The schedules from the independent review
// (docs/research/decisions/model-delivery-review-sol.md), one test each.

// Review 1. Requirements: D2, D5, D6, T3
func TestReportHeldOnlyByANoHistoryNewcomerSurvivesTheLossOfItsAuthor(t *testing.T) {
	w := room("A", "C")
	m1 := send(t, w, "A", "C", "m1")
	m2 := send(t, w, "A", "C", "m2")
	pull(t, w, "C", "A")
	read(t, w, "C")
	must(w.Handle("C", "c", m1))
	c1 := must(w.Send("C", "c", Msg{Body: "c1 before N", To: []Addr{at("A")}}))
	must(w.Admit("N", "C", false, "n"))
	w.Advance(2 * Day)
	must(w.Handle("C", "c", m2))
	c2 := must(w.Send("C", "c", Msg{Body: "c2 after N", To: []Addr{at("A")}}))
	pull(t, w, "N", "C")
	if h := w.Machine("N").get(c1); h != nil && h.body != "" {
		t.Fatal("N, admitted without history, holds earlier text")
	}
	w.DiskLost("C")
	pull(t, w, "A", "N")
	if st := w.Status("A", m2); len(st.Handled) != 1 {
		t.Fatalf("A never got C's report on m2 from N: %+v", st.Handled)
	}
	if w.Machine("A").get(c2) == nil {
		t.Fatal("A never got c2 from N")
	}
	// c1's text is gone with C's disk. A is told it is waiting for it, then,
	// once it would have expired, that it expired; then it sees c2.
	v := must(w.Inbox("A", "a"))
	if len(v.Entries) != 0 || len(v.Waiting) == 0 {
		t.Fatalf("A shown %s, waiting %+v", texts(v.Entries), v.Waiting)
	}
	w.Advance(29 * Day) // c1 is 31 days old, c2 29
	wantTexts(t, read(t, w, "A"), "expired: shown:c2 after N")
}

// Review 2. Requirements: D4
func TestRestoredAuthorThatCatchesUpBeforeWritingKeepsItsOrder(t *testing.T) {
	w := room("A", "B", "C")
	empty := w.Backup("A")
	send(t, w, "A", "C", "m1")
	pull(t, w, "B", "A")
	w.Restore("A", empty)
	pull(t, w, "A", "B") // A gets m1 back before writing
	send(t, w, "A", "C", "m2")
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:m1 shown:m2")
}

// Review 3. Requirements: D7, D8, D9
func TestRelayThatStripsTextAndClaimsWithdrawalCannotLoseIt(t *testing.T) {
	w := room("A", "B", "C")
	send(t, w, "A", "C", "keep")
	pull(t, w, "B", "A")
	w.tamper = func(srv MachineID, it wire) wire {
		if srv == "B" {
			it.body, it.hasBody = "", false // and, in words, "withdrawn"
		}
		return it
	}
	pull(t, w, "C", "B")
	w.tamper = nil
	v := must(w.Inbox("C", "c"))
	if len(v.Waiting) == 0 {
		t.Fatalf("text missing at C is not shown as a gap: %+v", v)
	}
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:keep")
}

// Review 4. Requirements: D2, D6
func TestARefusedStreamDoesNotStarveOthersUnderAPageLimit(t *testing.T) {
	w := room("A", "Z", "B", "C")
	a1 := send(t, w, "A", "C", "a1")
	send(t, w, "A", "C", "a2")
	send(t, w, "Z", "C", "z1")
	pull(t, w, "B", "A")
	pull(t, w, "B", "Z")
	w.Sleep("A")
	w.Sleep("Z")
	w.Withhold("B", a1)
	for i := 0; i < 5; i++ {
		must(w.Pull("C", "B", PullOpt{Limit: 1}))
	}
	wantTexts(t, read(t, w, "C"), "shown:z1")
}

// Review 5a. Requirements: D9
func TestWithdrawDropsTheTextOfReplacementsToo(t *testing.T) {
	w := room("A", "C")
	m := send(t, w, "A", "C", "v1")
	r := must(w.Replace("A", "a", m, "v2"))
	pull(t, w, "C", "A")
	must(w.Withdraw("A", "a", m))
	pull(t, w, "C", "A")
	for _, id := range []MachineID{"A", "C"} {
		if h := w.Machine(id).get(r); h.body != "" {
			t.Fatalf("%s keeps replacement text of a withdrawn message", id)
		}
	}
}

// Review 5b. Requirements: D7
func TestThreadAndBranchDoNotShowTextPastItsThirtyDays(t *testing.T) {
	w := room("A", "C")
	root := must(w.Send("A", "a", Msg{Body: "old"}))
	pull(t, w, "C", "A")
	w.SetSkew("C", 31*Day)
	for _, es := range [][]Entry{w.Thread("C", root), w.Branch("C", root)} {
		if len(es) != 1 || es[0].Kind != ExpiredNotice || es[0].Text != "" {
			t.Fatalf("shown %s", texts(es))
		}
	}
}

// Review 5c. Requirements: D9
func TestNewestReplacementIsByTheAuthorsHistoryNotItsClock(t *testing.T) {
	w := room("A", "C")
	m := send(t, w, "A", "C", "v1")
	w.Advance(Day)
	must(w.Replace("A", "a", m, "v2"))
	w.Crash("A")
	w.SetSkew("A", -2*Day) // A's clock is set back
	must(w.Replace("A", "a", m, "v3"))
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:v3")
}

// Review 7. The rejected alternative: keep one stream across ordinary
// restarts, detecting a restore by a head file kept outside the database.
// Requirements: D3
func TestHeadFileOutsideTheBackupSavesStreamsButFailsWhenRestoredWithIt(t *testing.T) {
	// Ordinary restarts: one stream.
	w := room("A", "B")
	w.persistHeads = true
	for i := 0; i < 3; i++ {
		send(t, w, "A", "B", "x")
		w.Crash("A")
	}
	if n := len(w.Machine("A").st.contig); n != 1 {
		t.Fatalf("%d streams after 3 restarts, want 1", n)
	}
	// Database restored alone: the head file disagrees, so a new stream.
	bk := w.Backup("A")
	send(t, w, "A", "B", "y")
	pull(t, w, "B", "A")
	w.Restore("A", bk)
	n := send(t, w, "A", "B", "after restore")
	pull(t, w, "B", "A")
	if w.Machine("B").get(n) == nil {
		t.Fatal("database-only restore lost the new message")
	}
	// Database and head file restored together (a whole-disk restore): the
	// restart looks ordinary, a position is reused, and B drops the new
	// message silently.
	bk = w.Backup("A")
	send(t, w, "A", "B", "z")
	pull(t, w, "B", "A")
	w.restoreWhole("A", bk)
	lost := send(t, w, "A", "B", "after whole restore")
	pull(t, w, "B", "A")
	if w.Machine("B").get(lost) != nil {
		t.Fatal("expected the whole-disk restore to lose the message")
	}
	if strings.Contains(texts(read(t, w, "B")), "after whole restore") {
		t.Fatal("unexpected")
	}
}
